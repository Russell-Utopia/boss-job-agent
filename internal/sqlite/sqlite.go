package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	schema "github.com/Russell-Utopia/boss-job-agent/docs"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1

var memoryDatabaseID atomic.Uint64

const defaultPolicyJSON = `{"rules":["只依据本次实际采用的在线简历和 JD，不猜测未提供的经历","有明确且重要的不匹配证据时判为不适合","有明确匹配证据时判为适合","信息不足或证据冲突时需要人工确认"]}`

// Open opens one local SQLite database, applies the v1 schema on first use, and
// creates the immutable default policy and singleton safe automation settings.
func Open(ctx context.Context, path string, now time.Time) (*sql.DB, error) {
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
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(ctx, db, now.UnixMilli()); err != nil {
		db.Close()
		return nil, err
	}
	if err := verifyForeignKeys(ctx, db); err != nil {
		db.Close()
		return nil, err
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
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return "", fmt.Errorf("create database directory: %w", err)
	}

	u := &url.URL{Scheme: "file", Path: absolutePath}
	query := u.Query()
	query.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func migrate(ctx context.Context, db *sql.DB, nowMillis int64) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version != 0 {
		return fmt.Errorf("unsupported database schema version %d", version)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()

	var existingTables int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
	`).Scan(&existingTables); err != nil {
		return fmt.Errorf("inspect existing schema: %w", err)
	}
	if existingTables != 0 {
		return errors.New("database has an unversioned business schema")
	}

	if _, err := tx.ExecContext(ctx, schema.SQLite); err != nil {
		return fmt.Errorf("apply v1 schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (1, ?, 1, '系统默认策略', ?)
	`, defaultPolicyJSON, nowMillis); err != nil {
		return fmt.Errorf("create default assessment policy: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO automation_settings (
			id,
			automatic_assessment_enabled,
			assessment_processing_limit,
			automatic_outreach_enabled,
			automatic_outreach_mode,
			outreach_greeting_text,
			outreach_time_windows_json,
			updated_at
		) VALUES (1, 0, 5, 0, 'simulation', NULL, '[]', ?)
	`, nowMillis); err != nil {
		return fmt.Errorf("create safe automation settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
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
