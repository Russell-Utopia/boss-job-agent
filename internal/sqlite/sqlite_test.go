package sqlite

import (
	"context"
	"reflect"
	"testing"
)

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
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
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
