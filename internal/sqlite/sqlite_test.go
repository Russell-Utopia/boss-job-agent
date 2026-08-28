package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestV1SchemaHasExactlyFiveBusinessTablesAndEnforcesForeignKeys(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), ":memory:", time.Now())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.Query(`
		SELECT name
		FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		t.Fatalf("list business tables: %v", err)
	}
	defer rows.Close()

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

	_, err = db.Exec(`
		INSERT INTO discovery_runs (
			resume_version_id, status, attempt_no, consecutive_failure_count,
			created_at, updated_at
		) VALUES (999, 'preparing', 0, 0, 1, 1)
	`)
	if err == nil {
		t.Fatal("orphan discovery run was accepted, want foreign key rejection")
	}
}

func TestRestartPreservesSavedSettingsAndPolicyVersions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	first, err := Open(context.Background(), path, time.UnixMilli(1000))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	tx, err := first.Begin()
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	if _, err := tx.Exec(`UPDATE assessment_policy_versions SET is_active = 0 WHERE version_no = 1`); err != nil {
		t.Fatalf("deactivate default policy: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (2, '{"rules":["用户保存的策略"]}', 1, '用户采用', 2000)
	`); err != nil {
		t.Fatalf("insert saved policy: %v", err)
	}
	if _, err := tx.Exec(`
		UPDATE automation_settings
		SET automatic_assessment_enabled = 1,
			assessment_processing_limit = 12,
			automatic_outreach_enabled = 1,
			automatic_outreach_mode = 'real',
			outreach_greeting_text = '您好，想和您聊聊这个岗位',
			outreach_time_windows_json = '[{"start":"10:00","end":"12:00"}]',
			updated_at = 2000
		WHERE id = 1
	`); err != nil {
		t.Fatalf("save custom settings: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fixture transaction: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first database: %v", err)
	}

	second, err := Open(context.Background(), path, time.UnixMilli(3000))
	if err != nil {
		t.Fatalf("restart open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	assertSingleInt(t, second, `SELECT count(*) FROM assessment_policy_versions`, 2)
	assertSingleInt(t, second, `SELECT version_no FROM assessment_policy_versions WHERE is_active = 1`, 2)
	assertSingleInt(t, second, `SELECT assessment_processing_limit FROM automation_settings WHERE id = 1`, 12)
	assertSingleInt(t, second, `SELECT count(*) FROM automation_settings`, 1)

	var mode, greeting, windows string
	if err := second.QueryRow(`
		SELECT automatic_outreach_mode, outreach_greeting_text, outreach_time_windows_json
		FROM automation_settings WHERE id = 1
	`).Scan(&mode, &greeting, &windows); err != nil {
		t.Fatalf("read preserved outreach settings: %v", err)
	}
	if mode != "real" || greeting != "您好，想和您聊聊这个岗位" || windows != `[{"start":"10:00","end":"12:00"}]` {
		t.Errorf("saved outreach settings were overwritten: mode=%q greeting=%q windows=%q", mode, greeting, windows)
	}
}

func assertSingleInt(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("query value: %v", err)
	}
	if got != want {
		t.Fatalf("value = %d, want %d", got, want)
	}
}
