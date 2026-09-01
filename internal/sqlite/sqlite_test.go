package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

func TestOpenAppliesGooseMigrationOnceAcrossRestarts(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open new database: %v", err)
	}
	assertGooseVersion(t, db, 3)
	if err := db.Close(); err != nil {
		t.Fatalf("close new database: %v", err)
	}

	restarted, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	assertGooseVersion(t, restarted, 3)
	assertBackupCount(t, path, 0)
}

func TestDefaultPathUsesApplicationSupportLayout(t *testing.T) {
	configurationDirectory, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("resolve user configuration directory: %v", err)
	}
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("resolve default sqlite path: %v", err)
	}
	want := filepath.Join(
		configurationDirectory,
		"boss-job-agent",
		"data",
		"boss-job-agent.db",
	)
	if path != want {
		t.Fatalf("default sqlite path = %q, want %q", path, want)
	}
	wantBackups := filepath.Join(configurationDirectory, "boss-job-agent", "backups")
	if got := backupDirectory(path); got != wantBackups {
		t.Fatalf("default sqlite backup directory = %q, want %q", got, wantBackups)
	}
}

func TestOpenUpgradesCandidateAndPreservesExistingData(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "boss-job-agent.db")
	createV1DatabaseWithRepresentativeData(t, path)

	upgraded, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	assertGooseVersion(t, upgraded, 3)

	assertRepresentativeDataPreserved(t, upgraded)
	assertNoCandidateFiles(t, path)
	assertNoSQLiteSidecars(t, path)

	backups := assertBackupCount(t, path, 1)
	backupInfo, err := os.Stat(backups[0])
	if err != nil {
		t.Fatalf("stat upgrade backup: %v", err)
	}
	if backupInfo.Mode().Perm()&0o222 != 0 {
		t.Fatalf("upgrade backup mode = %o, want no write bits", backupInfo.Mode().Perm())
	}
}

func TestOpenUpgradesAnUnpreparedRunSoItCanEndEarly(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	v1, err := openWithMigrations(t.Context(), path, initialMigrations(t))
	if err != nil {
		t.Fatalf("open v1 database: %v", err)
	}
	if _, err := v1.ExecContext(t.Context(), `
		INSERT INTO discovery_runs (status, attempt_no, created_at, updated_at)
		VALUES ('preparing', 0, 1000, 1000)
	`); err != nil {
		_ = v1.Close()
		t.Fatalf("seed unprepared v1 discovery: %v", err)
	}
	if err := v1.Close(); err != nil {
		t.Fatalf("close v1 database: %v", err)
	}

	upgraded, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("upgrade unprepared discovery database: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	if _, err := upgraded.ExecContext(t.Context(), `
		UPDATE discovery_runs
		SET status = 'ended_early', finished_at = 2000, updated_at = 2000
		WHERE status = 'preparing'
	`); err != nil {
		t.Fatalf("end upgraded unprepared discovery: %v", err)
	}
	assertGooseVersion(t, upgraded, 3)
	assertBackupCount(t, path, 1)
}

func TestOpenUpgradesV2WithAnEmptyPerJobCheckpointAndPreservesData(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	createDatabaseWithRepresentativeData(t, path, migrationsThroughV2(t))

	upgraded, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("upgrade v2 database: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	assertGooseVersion(t, upgraded, 3)
	assertRepresentativeDataPreserved(t, upgraded)
	var jobIDs sql.NullString
	var hasMore, ordinal sql.NullInt64
	if err := upgraded.QueryRowContext(t.Context(), `
		SELECT current_page_job_ids_json, current_page_has_more, next_job_ordinal
		FROM discovery_runs
		WHERE name = 'representative discovery'
	`).Scan(&jobIDs, &hasMore, &ordinal); err != nil {
		t.Fatalf("read upgraded page checkpoint: %v", err)
	}
	if jobIDs.Valid || hasMore.Valid || ordinal.Valid {
		t.Fatalf("upgraded v2 page checkpoint = %#v/%#v/%#v, want all empty", jobIDs, hasMore, ordinal)
	}
	assertBackupCount(t, path, 1)
}

func TestBackupFailureLeavesFormalDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "boss-job-agent.db")
	createV1DatabaseWithRepresentativeData(t, path)
	if err := os.WriteFile(filepath.Join(directory, "backups"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocked backup path: %v", err)
	}

	_, err := openWithMigrations(t.Context(), path, testMigrations(t, map[string]string{
		"00004_valid.sql": "-- +goose Up\nCREATE INDEX idx_resume_hash_created ON online_resume_versions(resume_hash, created_at);\n",
	}))
	if err == nil {
		t.Fatal("upgrade succeeded with an unusable backup directory")
	}

	assertFormalDatabaseIsStillV1(t, path)
	assertNoCandidateFiles(t, path)
}

func TestCandidateMigrationFailureLeavesFormalDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	createV1DatabaseWithRepresentativeData(t, path)

	_, err := openWithMigrations(t.Context(), path, testMigrations(t, map[string]string{
		"00004_broken.sql": "-- +goose Up\nTHIS IS NOT VALID SQL;\n",
	}))
	if err == nil {
		t.Fatal("invalid candidate migration succeeded")
	}

	assertFormalDatabaseIsStillV1(t, path)
	assertBackupCount(t, path, 1)
	assertNoCandidateFiles(t, path)
}

func TestCandidateValidationFailureLeavesFormalDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	createV1DatabaseWithRepresentativeData(t, path)

	_, err := openWithMigrations(t.Context(), path, testMigrations(t, map[string]string{
		"00004_invalid_schema.sql": "-- +goose Up\nDROP TABLE automation_settings;\n",
	}))
	if err == nil {
		t.Fatal("candidate with a missing business table passed validation")
	}

	assertFormalDatabaseIsStillV1(t, path)
	assertBackupCount(t, path, 1)
	assertNoCandidateFiles(t, path)
}

func TestCandidateKeyDataChangeLeavesFormalDatabaseUnchanged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	createV1DatabaseWithRepresentativeData(t, path)

	_, err := openWithMigrations(t.Context(), path, testMigrations(t, map[string]string{
		"00004_tamper_resume.sql": `-- +goose Up
UPDATE online_resume_versions
SET resume_json = '{"intent":"tampered"}'
WHERE version_no = 1;
`,
	}))
	if err == nil {
		t.Fatal("candidate that changed key business data passed validation")
	}

	assertFormalDatabaseIsStillV1(t, path)
	assertBackupCount(t, path, 1)
	assertNoCandidateFiles(t, path)
}

func TestNewDatabaseMigrationFailureLeavesNoFormalDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	_, err := openWithMigrations(t.Context(), path, testMigrations(t, map[string]string{
		"00004_broken.sql": "-- +goose Up\nTHIS IS NOT VALID SQL;\n",
	}))
	if err == nil {
		t.Fatal("new database succeeded with an invalid migration")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("formal database exists after failed creation: %v", err)
	}
	assertNoCandidateFiles(t, path)
}

func createV1DatabaseWithRepresentativeData(t *testing.T, path string) {
	t.Helper()
	createDatabaseWithRepresentativeData(t, path, initialMigrations(t))
}

func createDatabaseWithRepresentativeData(t *testing.T, path string, migrations fs.FS) {
	t.Helper()

	db, err := openWithMigrations(t.Context(), path, migrations)
	if err != nil {
		t.Fatalf("open v1 database: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (1, '{"intent":"Go backend"}', 'resume-v1', 1, 1000)
		;
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (1, '{"rules":["keep evidence"]}', 1, 'representative policy', 1001)
		;
		INSERT INTO discovery_runs (
			name, resume_version_id, current_role, current_city, next_page,
			status, attempt_no, consecutive_failure_count,
			created_at, prepared_at, updated_at
		) VALUES (
			'representative discovery', 1, 'Go backend', 'Shanghai', 2,
			'paused', 1, 0,
			1002, 1003, 1004
		)
		;
		INSERT INTO platform_jobs (
			platform_job_id, canonical_url, job_title,
			company_name, city_text, salary_text,
			jd_json, jd_hash, platform_status, platform_status_checked_at,
			first_seen_at, last_seen_at, updated_at
		) VALUES (
			'boss-job-1', 'https://www.zhipin.com/job_detail/boss-job-1.html', 'Go engineer',
			'Example Co', 'Shanghai', '20-30K',
			'{"description":"build services"}', 'jd-v1', 'open', 1005,
			1006, 1007, 1008
		)
		;
		INSERT INTO automation_settings (
			id, automatic_assessment_enabled, assessment_processing_limit,
			automatic_outreach_enabled, outreach_greeting_text,
			outreach_time_windows_json, updated_at
		) VALUES (1, 0, 5, 0, NULL, '[]', 1009)
	`); err != nil {
		_ = db.Close()
		t.Fatalf("insert representative v1 business data: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 database: %v", err)
	}
}

func assertFormalDatabaseIsStillV1(t *testing.T, path string) {
	t.Helper()

	db, err := openWithMigrations(t.Context(), path, initialMigrations(t))
	if err != nil {
		t.Fatalf("reopen formal database after failed upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assertGooseVersion(t, db, 1)
	assertRepresentativeDataPreserved(t, db)
}

func assertRepresentativeDataPreserved(t *testing.T, db *sql.DB) {
	t.Helper()

	var resumeJSON string
	var policyRules string
	var discoveryStatus string
	var platformJobID string
	var processingLimit int
	if err := db.QueryRowContext(t.Context(), `
		SELECT
			(SELECT resume_json FROM online_resume_versions WHERE version_no = 1),
			(SELECT rules_json FROM assessment_policy_versions WHERE version_no = 1),
			(SELECT status FROM discovery_runs WHERE name = 'representative discovery'),
			(SELECT platform_job_id FROM platform_jobs WHERE jd_hash = 'jd-v1'),
			(SELECT assessment_processing_limit FROM automation_settings WHERE id = 1)
	`).Scan(
		&resumeJSON,
		&policyRules,
		&discoveryStatus,
		&platformJobID,
		&processingLimit,
	); err != nil {
		t.Fatalf("read representative business data: %v", err)
	}
	if resumeJSON != `{"intent":"Go backend"}` {
		t.Fatalf("resume = %q, want preserved v1 data", resumeJSON)
	}
	if policyRules != `{"rules":["keep evidence"]}` {
		t.Fatalf("policy rules = %q, want preserved v1 data", policyRules)
	}
	if discoveryStatus != "paused" {
		t.Fatalf("discovery status = %q, want paused", discoveryStatus)
	}
	if platformJobID != "boss-job-1" {
		t.Fatalf("platform job ID = %q, want boss-job-1", platformJobID)
	}
	if processingLimit != 5 {
		t.Fatalf("assessment processing limit = %d, want 5", processingLimit)
	}
}

func assertBackupCount(t *testing.T, path string, want int) []string {
	t.Helper()

	backups, err := filepath.Glob(filepath.Join(
		filepath.Dir(path),
		"backups",
		filepath.Base(path)+".*.backup",
	))
	if err != nil {
		t.Fatalf("list upgrade backups: %v", err)
	}
	if len(backups) != want {
		t.Fatalf("upgrade backups = %v, want %d", backups, want)
	}
	return backups
}

func assertNoCandidateFiles(t *testing.T, path string) {
	t.Helper()

	candidates, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".candidate-*"))
	if err != nil {
		t.Fatalf("list sqlite candidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("sqlite candidates remain after failure: %v", candidates)
	}
}

func assertNoSQLiteSidecars(t *testing.T, path string) {
	t.Helper()

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("sqlite sidecar remains after replacement: %s", path+suffix)
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect sqlite sidecar %s: %v", suffix, err)
		}
	}
}

func testMigrations(t *testing.T, additional map[string]string) fs.FS {
	t.Helper()

	migrations := initialMigrations(t)
	second, err := fs.ReadFile(embeddedMigrations, "migrations/00002_allow_unprepared_discovery_termination.sql")
	if err != nil {
		t.Fatalf("read embedded second migration: %v", err)
	}
	migrations["00002_allow_unprepared_discovery_termination.sql"] = &fstest.MapFile{Data: second}
	third, err := fs.ReadFile(embeddedMigrations, "migrations/00003_add_per_job_discovery_checkpoint.sql")
	if err != nil {
		t.Fatalf("read embedded third migration: %v", err)
	}
	migrations["00003_add_per_job_discovery_checkpoint.sql"] = &fstest.MapFile{Data: third}
	for name, contents := range additional {
		migrations[name] = &fstest.MapFile{Data: []byte(contents)}
	}
	return migrations
}

func migrationsThroughV2(t *testing.T) fstest.MapFS {
	t.Helper()
	migrations := initialMigrations(t)
	second, err := fs.ReadFile(embeddedMigrations, "migrations/00002_allow_unprepared_discovery_termination.sql")
	if err != nil {
		t.Fatalf("read embedded second migration: %v", err)
	}
	migrations["00002_allow_unprepared_discovery_termination.sql"] = &fstest.MapFile{Data: second}
	return migrations
}

func initialMigrations(t *testing.T) fstest.MapFS {
	t.Helper()

	initial, err := fs.ReadFile(embeddedMigrations, "migrations/00001_initial.sql")
	if err != nil {
		t.Fatalf("read embedded initial migration: %v", err)
	}
	return fstest.MapFS{
		"00001_initial.sql": &fstest.MapFile{Data: initial},
	}
}

func assertGooseVersion(t *testing.T, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, want int64) {
	t.Helper()

	var got int64
	if err := db.QueryRowContext(t.Context(), `
		SELECT max(version_id)
		FROM goose_db_version
		WHERE is_applied = 1
	`).Scan(&got); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if got != want {
		t.Fatalf("goose version = %d, want %d", got, want)
	}
}

func TestV1SchemaHasExactlyFiveBusinessTablesAndEnforcesForeignKeys(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.QueryContext(t.Context(), `
			SELECT name
			FROM sqlite_master
			WHERE type = 'table'
			  AND name NOT LIKE 'sqlite_%'
			  AND name <> 'goose_db_version'
			ORDER BY name
		`)
	if err != nil {
		t.Fatalf("list business tables: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close table rows: %v", err)
		}
	}()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table names: %v", err)
	}
	wantTables := []string{
		"assessment_policy_versions",
		"automation_settings",
		"discovery_runs",
		"online_resume_versions",
		"platform_jobs",
	}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("business tables = %#v, want %#v", tables, wantTables)
	}

	_, err = db.ExecContext(t.Context(), `
		INSERT INTO discovery_runs (
			resume_version_id, status, attempt_no, consecutive_failure_count,
			created_at, updated_at
		) VALUES (999, 'preparing', 0, 0, 1, 1)
	`)
	if err == nil {
		t.Fatal("orphan discovery run was accepted, want foreign key rejection")
	}
}

func TestPerJobDiscoveryCheckpointFieldsAreCollectivelyValid(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (1, '{}', 'resume-v1', 1, 1000);
		INSERT INTO discovery_runs (
			resume_version_id, current_role, current_city, next_page,
			status, attempt_no, worker_owner, worker_lease_until,
			created_at, prepared_at, updated_at
		) VALUES (1, 'Go 后端工程师', '福州', 1, 'running', 1, 'worker-1', 10000, 1000, 1000, 1000);
	`); err != nil {
		t.Fatalf("seed running discovery: %v", err)
	}
	for name, update := range map[string]string{
		"partial checkpoint": `
			UPDATE discovery_runs
			SET current_page_job_ids_json = '["boss-job-1"]'
		`,
		"duplicate stable IDs": `
			UPDATE discovery_runs
			SET current_page_job_ids_json = '["boss-job-1","boss-job-1"]',
			    current_page_has_more = 0,
			    next_job_ordinal = 0
		`,
		"ordinal after page end": `
			UPDATE discovery_runs
			SET current_page_job_ids_json = '["boss-job-1"]',
			    current_page_has_more = 0,
			    next_job_ordinal = 2
		`,
	} {
		if _, err := db.ExecContext(t.Context(), update); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := db.ExecContext(t.Context(), `
		UPDATE discovery_runs
		SET current_page_job_ids_json = '["boss-job-1","boss-job-2"]',
		    current_page_has_more = 0,
		    next_job_ordinal = 1
	`); err != nil {
		t.Fatalf("save valid page checkpoint: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		UPDATE discovery_runs
		SET status = 'completed',
		    worker_owner = NULL,
		    worker_lease_until = NULL,
		    finished_at = 2000,
		    updated_at = 2000
	`); err == nil {
		t.Fatal("completed run retained a page checkpoint")
	}
}
