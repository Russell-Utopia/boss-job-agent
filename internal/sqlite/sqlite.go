package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/Russell-Utopia/boss-job-agent/internal/sqlite/internal/sqlitedb"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

var memoryDatabaseID atomic.Uint64

type databaseEvidence struct {
	Counts             sqlitedb.CountBusinessRowsRow
	KeyDataFingerprint [sha256.Size]byte
}

// Open opens one local SQLite database and applies pending migrations. File
// databases are migrated on a verified candidate; an existing database is
// replaced only after a read-only upgrade backup has been retained.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	migrations, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("open embedded sqlite migrations: %w", err)
	}
	return openWithMigrations(ctx, path, migrations)
}

func openConnection(ctx context.Context, path string) (*sql.DB, error) {
	dsn, err := dataSourceName(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		return nil, closeDatabaseAfterError(db, fmt.Errorf("ping sqlite: %w", err))
	}
	return db, nil
}

func dataSourceName(path string) (string, error) {
	if path == "" {
		return "", errors.New("database path is required")
	}
	if path == ":memory:" {
		id := memoryDatabaseID.Add(1)
		return fmt.Sprintf("file:boss_job_agent_%d?mode=memory&cache=shared&_pragma=foreign_keys%%281%%29", id), nil
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		return "", fmt.Errorf("create database directory: %w", err)
	}

	u := &url.URL{Scheme: "file", Path: absolutePath}
	query := u.Query()
	query.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func migrate(ctx context.Context, db *sql.DB, migrations fs.FS) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations)
	if err != nil {
		return fmt.Errorf("create sqlite migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply sqlite migrations: %w", err)
	}
	return nil
}

func closeDatabaseAfterError(db *sql.DB, cause error) error {
	if err := db.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close sqlite after failure: %w", err))
	}
	return cause
}

func verifyForeignKeys(ctx context.Context, db *sql.DB) error {
	var enabled int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		return fmt.Errorf("read sqlite foreign key setting: %w", err)
	}
	if enabled != 1 {
		return errors.New("sqlite foreign keys are disabled")
	}
	return nil
}

func verifyDatabase(ctx context.Context, db *sql.DB, migrations fs.FS) (databaseEvidence, error) {
	pending, err := hasPendingMigrations(ctx, db, migrations)
	if err != nil {
		return databaseEvidence{}, err
	}
	if pending {
		return databaseEvidence{}, errors.New("sqlite database still has pending migrations")
	}
	return verifyDatabaseHealth(ctx, db)
}

func verifyDatabaseHealth(ctx context.Context, db *sql.DB) (databaseEvidence, error) {
	if err := verifyForeignKeys(ctx, db); err != nil {
		return databaseEvidence{}, err
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return databaseEvidence{}, fmt.Errorf("check sqlite integrity: %w", err)
	}
	if integrity != "ok" {
		return databaseEvidence{}, fmt.Errorf("sqlite integrity check returned %q", integrity)
	}
	hasViolation, err := hasForeignKeyViolation(ctx, db)
	if err != nil {
		return databaseEvidence{}, err
	}
	if hasViolation {
		return databaseEvidence{}, errors.New("sqlite foreign key check found a violation")
	}
	queries := sqlitedb.New(db)
	counts, err := queries.CountBusinessRows(ctx)
	if err != nil {
		return databaseEvidence{}, fmt.Errorf("count sqlite business rows: %w", err)
	}
	keyData, err := queries.ListKeyBusinessData(ctx)
	if err != nil {
		return databaseEvidence{}, fmt.Errorf("read sqlite key business data: %w", err)
	}
	return databaseEvidence{
		Counts:             counts,
		KeyDataFingerprint: fingerprintKeyBusinessData(keyData),
	}, nil
}

func fingerprintKeyBusinessData(rows []sqlitedb.ListKeyBusinessDataRow) [sha256.Size]byte {
	hasher := sha256.New()
	for _, row := range rows {
		writeFingerprintField(hasher, []byte(row.TableName))
		var rowID [8]byte
		binary.BigEndian.PutUint64(rowID[:], uint64(row.RowID)) //nolint:gosec // SQLite identifiers are positive.
		writeFingerprintField(hasher, rowID[:])
		writeFingerprintField(hasher, []byte(row.RowData))
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return fingerprint
}

func writeFingerprintField(hasher hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write(value)
}

func hasPendingMigrations(ctx context.Context, db *sql.DB, migrations fs.FS) (bool, error) {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations)
	if err != nil {
		return false, fmt.Errorf("create sqlite migration provider: %w", err)
	}
	var versionTableExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_schema
			WHERE type = 'table' AND name = 'goose_db_version'
		)
	`).Scan(&versionTableExists); err != nil {
		return false, fmt.Errorf("inspect sqlite migration metadata: %w", err)
	}
	if !versionTableExists {
		return true, nil
	}

	applied, err := appliedMigrationVersions(ctx, db)
	if err != nil {
		return false, err
	}
	sources := provider.ListSources()
	known := make(map[int64]struct{}, len(sources))
	for _, source := range sources {
		known[source.Version] = struct{}{}
	}
	for version := range applied {
		if _, ok := known[version]; !ok {
			return false, fmt.Errorf("sqlite migration version %d is newer than this application", version)
		}
	}
	for _, source := range sources {
		if _, ok := applied[source.Version]; !ok {
			return true, nil
		}
	}
	return false, nil
}

func hasForeignKeyViolation(ctx context.Context, db *sql.DB) (_ bool, retErr error) {
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return false, fmt.Errorf("check sqlite foreign keys: %w", err)
	}
	defer func() {
		retErr = joinCloseError(retErr, rows.Close(), "close sqlite foreign key check")
	}()
	if rows.Next() {
		return true, nil
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate sqlite foreign key check: %w", err)
	}
	return false, nil
}

func appliedMigrationVersions(ctx context.Context, db *sql.DB) (_ map[int64]struct{}, retErr error) {
	rows, err := db.QueryContext(ctx, `
		SELECT version_id
		FROM goose_db_version
		WHERE is_applied = 1 AND version_id > 0
	`)
	if err != nil {
		return nil, fmt.Errorf("read applied sqlite migrations: %w", err)
	}
	defer func() {
		retErr = joinCloseError(retErr, rows.Close(), "close applied sqlite migrations")
	}()

	versions := make(map[int64]struct{})
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied sqlite migration: %w", err)
		}
		versions[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied sqlite migrations: %w", err)
	}
	return versions, nil
}

func joinCloseError(cause, closeErr error, operation string) error {
	return errors.Join(cause, wrapError(closeErr, operation))
}

func wrapError(err error, operation string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
