package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Russell-Utopia/boss-job-agent/internal/sqlite/internal/sqlitedb"
)

func openWithMigrations(ctx context.Context, path string, migrations fs.FS) (*sql.DB, error) {
	if path == ":memory:" {
		return openMigratedDatabase(ctx, path, migrations)
	}
	if path == "" {
		return nil, errors.New("database path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	_, err = os.Stat(absolutePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return createDatabase(ctx, absolutePath, migrations)
	case err != nil:
		return nil, fmt.Errorf("inspect sqlite database: %w", err)
	default:
		return openExistingDatabase(ctx, absolutePath, migrations)
	}
}

func openMigratedDatabase(ctx context.Context, path string, migrations fs.FS) (*sql.DB, error) {
	db, err := openConnection(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, migrations); err != nil {
		return nil, closeDatabaseAfterError(db, err)
	}
	if _, err := verifyDatabase(ctx, db, migrations); err != nil {
		return nil, closeDatabaseAfterError(db, err)
	}
	return db, nil
}

func createDatabase(ctx context.Context, path string, migrations fs.FS) (*sql.DB, error) {
	candidate, err := reserveCandidatePath(path)
	if err != nil {
		return nil, err
	}
	defer removeSQLiteFiles(candidate)

	db, err := openMigratedDatabase(ctx, candidate, migrations)
	if err != nil {
		return nil, err
	}
	if err := checkpointAndClose(ctx, db, candidate); err != nil {
		return nil, err
	}
	return replaceAndReopen(ctx, candidate, path, "", migrations)
}

func openExistingDatabase(ctx context.Context, path string, migrations fs.FS) (*sql.DB, error) {
	live, err := openConnection(ctx, path)
	if err != nil {
		return nil, err
	}
	pending, err := hasPendingMigrations(ctx, live, migrations)
	if err != nil {
		return nil, closeDatabaseAfterError(live, err)
	}
	if !pending {
		if _, err := verifyDatabase(ctx, live, migrations); err != nil {
			return nil, closeDatabaseAfterError(live, err)
		}
		return live, nil
	}
	return upgradeDatabase(ctx, path, live, migrations)
}

func upgradeDatabase(ctx context.Context, path string, live *sql.DB, migrations fs.FS) (*sql.DB, error) {
	liveCounts, err := sqlitedb.New(live).CountBusinessRows(ctx)
	if err != nil {
		return nil, closeDatabaseAfterError(live, fmt.Errorf("count live sqlite business rows: %w", err))
	}
	if err := ensureUpgradeSpace(path); err != nil {
		return nil, closeDatabaseAfterError(live, err)
	}
	backup, err := createBackup(ctx, live, path, liveCounts)
	if err != nil {
		return nil, closeDatabaseAfterError(live, err)
	}
	candidate, err := copyBackupToCandidate(backup, path)
	if err != nil {
		return nil, closeDatabaseAfterError(live, err)
	}
	defer removeSQLiteFiles(candidate)

	candidateDB, err := openMigratedDatabase(ctx, candidate, migrations)
	if err != nil {
		return nil, closeDatabaseAfterError(live, err)
	}
	candidateCounts, err := sqlitedb.New(candidateDB).CountBusinessRows(ctx)
	if err != nil {
		cause := closeDatabaseAfterError(candidateDB, fmt.Errorf("count candidate sqlite business rows: %w", err))
		return nil, closeDatabaseAfterError(live, cause)
	}
	if candidateCounts != liveCounts {
		cause := closeDatabaseAfterError(candidateDB, errors.New("candidate sqlite migration changed business row counts"))
		return nil, closeDatabaseAfterError(live, cause)
	}
	if err := checkpointAndClose(ctx, candidateDB, candidate); err != nil {
		return nil, closeDatabaseAfterError(live, err)
	}
	if err := checkpointAndClose(ctx, live, path); err != nil {
		return nil, err
	}
	return replaceAndReopen(ctx, candidate, path, backup, migrations)
}

func createBackup(
	ctx context.Context,
	live *sql.DB,
	path string,
	liveCounts sqlitedb.CountBusinessRowsRow,
) (_ string, retErr error) {
	directory := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create sqlite backup directory: %w", err)
	}
	backup, err := reserveBackupPath(directory, filepath.Base(path))
	if err != nil {
		return "", err
	}
	keepBackup := false
	defer func() {
		if !keepBackup {
			removeSQLiteFiles(backup)
		}
	}()
	if _, err := live.ExecContext(ctx, "VACUUM main INTO ?", backup); err != nil {
		return "", fmt.Errorf("create sqlite upgrade backup: %w", err)
	}
	if err := verifyUpgradeBackup(ctx, backup, liveCounts); err != nil {
		return "", err
	}
	if err := os.Chmod(backup, 0o400); err != nil {
		return "", fmt.Errorf("make sqlite upgrade backup read-only: %w", err)
	}
	if err := syncFile(backup); err != nil {
		return "", err
	}
	if err := syncDirectory(directory); err != nil {
		return "", err
	}
	keepBackup = true
	return backup, nil
}

func reserveBackupPath(directory, databaseName string) (string, error) {
	placeholder, err := os.CreateTemp(directory, databaseName+".*.backup")
	if err != nil {
		return "", fmt.Errorf("reserve sqlite backup path: %w", err)
	}
	backup := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(backup)
		return "", fmt.Errorf("close sqlite backup placeholder: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		_ = os.Remove(backup)
		return "", fmt.Errorf("prepare sqlite backup path: %w", err)
	}
	return backup, nil
}

func verifyUpgradeBackup(
	ctx context.Context,
	backup string,
	liveCounts sqlitedb.CountBusinessRowsRow,
) error {
	if err := syncFile(backup); err != nil {
		return err
	}
	backupDB, err := openConnection(ctx, backup)
	if err != nil {
		return fmt.Errorf("open sqlite upgrade backup: %w", err)
	}
	backupCounts, verifyErr := verifyDatabaseHealth(ctx, backupDB)
	closeErr := backupDB.Close()
	if verifyErr != nil || closeErr != nil {
		return errors.Join(
			verifyErr,
			joinCloseError(nil, closeErr, "close verified sqlite upgrade backup"),
		)
	}
	if backupCounts != liveCounts {
		return errors.New("sqlite upgrade backup changed business row counts")
	}
	return nil
}

func copyBackupToCandidate(backup, path string) (string, error) {
	candidate, err := reserveCandidatePath(path)
	if err != nil {
		return "", err
	}
	if err := copyFile(backup, candidate); err != nil {
		removeSQLiteFiles(candidate)
		return "", err
	}
	return candidate, nil
}

func reserveCandidatePath(path string) (string, error) {
	placeholder, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".candidate-*")
	if err != nil {
		return "", fmt.Errorf("reserve sqlite candidate path: %w", err)
	}
	candidate := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(candidate)
		return "", fmt.Errorf("close sqlite candidate placeholder: %w", err)
	}
	if err := os.Remove(candidate); err != nil {
		_ = os.Remove(candidate)
		return "", fmt.Errorf("prepare sqlite candidate path: %w", err)
	}
	return candidate, nil
}

func copyFile(source, destination string) (retErr error) {
	//nolint:gosec // Both paths are module-owned backup and candidate paths under the configured database directory.
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open sqlite backup: %w", err)
	}
	defer func() {
		retErr = joinCloseError(retErr, input.Close(), "close sqlite backup")
	}()
	//nolint:gosec // The destination is a reserved candidate path in the configured database directory.
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create sqlite candidate: %w", err)
	}
	defer func() {
		retErr = joinCloseError(retErr, output.Close(), "close sqlite candidate")
	}()
	if _, err := io.Copy(output, input); err != nil {
		return fmt.Errorf("copy sqlite backup to candidate: %w", err)
	}
	if err := output.Sync(); err != nil {
		return fmt.Errorf("sync sqlite candidate: %w", err)
	}
	return nil
}

func checkpointAndClose(ctx context.Context, db *sql.DB, path string) error {
	var busy, logFrames, checkpointedFrames int
	checkpointErr := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(
		&busy,
		&logFrames,
		&checkpointedFrames,
	)
	if checkpointErr == nil && busy != 0 {
		checkpointErr = errors.New("sqlite WAL checkpoint remained busy")
	}
	var result error
	if checkpointErr != nil {
		result = fmt.Errorf("checkpoint sqlite before replacement: %w", checkpointErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		result = errors.Join(result, fmt.Errorf("close sqlite before replacement: %w", closeErr))
	}
	if result != nil {
		return result
	}
	if err := removeSQLiteSidecars(path); err != nil {
		return err
	}
	if err := syncFile(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func replaceAndReopen(
	ctx context.Context,
	candidate string,
	path string,
	backup string,
	migrations fs.FS,
) (*sql.DB, error) {
	if err := os.Rename(candidate, path); err != nil {
		return nil, fmt.Errorf("atomically replace sqlite database: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return nil, restoreAfterReplacementFailure(ctx, backup, path, err)
	}
	reopened, err := openConnection(ctx, path)
	if err != nil {
		return nil, restoreAfterReplacementFailure(
			ctx,
			backup,
			path,
			fmt.Errorf("reopen replaced sqlite database: %w", err),
		)
	}
	if _, err := verifyDatabase(ctx, reopened, migrations); err != nil {
		cause := closeDatabaseAfterError(reopened, fmt.Errorf("verify reopened sqlite database: %w", err))
		return nil, restoreAfterReplacementFailure(ctx, backup, path, cause)
	}
	return reopened, nil
}

func restoreAfterReplacementFailure(ctx context.Context, backup, path string, cause error) error {
	if backup == "" {
		removeSQLiteFiles(path)
		return cause
	}
	rollback, err := copyBackupToCandidate(backup, path)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("prepare sqlite rollback candidate: %w", err))
	}
	defer removeSQLiteFiles(rollback)
	preRenameSyncErr := syncDirectory(filepath.Dir(path))
	if err := os.Rename(rollback, path); err != nil {
		return errors.Join(
			cause,
			wrapError(preRenameSyncErr, "sync sqlite rollback candidate"),
			fmt.Errorf("restore sqlite upgrade backup: %w", err),
		)
	}
	postRenameSyncErr := syncDirectory(filepath.Dir(path))
	if preRenameSyncErr != nil || postRenameSyncErr != nil {
		return errors.Join(
			cause,
			wrapError(preRenameSyncErr, "sync sqlite rollback candidate"),
			wrapError(postRenameSyncErr, "sync restored sqlite database"),
		)
	}
	restored, err := openConnection(ctx, path)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("reopen restored sqlite database: %w", err))
	}
	_, verifyErr := verifyDatabaseHealth(ctx, restored)
	closeErr := restored.Close()
	if verifyErr != nil || closeErr != nil {
		var restoreErr error
		if verifyErr != nil {
			restoreErr = fmt.Errorf("verify restored sqlite database: %w", verifyErr)
		}
		restoreErr = joinCloseError(restoreErr, closeErr, "close restored sqlite database")
		return errors.Join(cause, restoreErr)
	}
	return cause
}

func syncFile(path string) error {
	//nolint:gosec // The path is a validated database, backup, or candidate path owned by this module.
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open sqlite file for sync: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync sqlite file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close synced sqlite file: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	//nolint:gosec // The path is the validated parent directory of a module-owned database file.
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open sqlite directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync sqlite directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close synced sqlite directory: %w", err)
	}
	return nil
}

func removeSQLiteFiles(path string) {
	_ = os.Remove(path)
	_ = removeSQLiteSidecars(path)
}

func removeSQLiteSidecars(path string) error {
	var result error
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove sqlite sidecar %s: %w", suffix, err))
		}
	}
	return result
}
