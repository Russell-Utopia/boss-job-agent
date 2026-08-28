package automationsettings

import (
	"context"
	"database/sql"
	"fmt"
)

// EnsureSafeDefaults creates the singleton settings row without changing any
// settings already saved by the job seeker.
func EnsureSafeDefaults(ctx context.Context, db *sql.DB, nowMillis int64) error {
	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO automation_settings (
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
	return nil
}
