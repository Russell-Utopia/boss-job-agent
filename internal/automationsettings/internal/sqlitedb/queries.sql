-- name: EnsureSafeDefaults :exec
INSERT OR IGNORE INTO automation_settings (
  id,
  automatic_assessment_enabled,
  assessment_processing_limit,
  automatic_outreach_enabled,
  outreach_greeting_text,
  outreach_time_windows_json,
  updated_at
) VALUES (1, 0, 5, 0, NULL, '[]', sqlc.arg(updated_at));

-- name: GetAutomationSettings :one
SELECT
  automatic_assessment_enabled,
  assessment_processing_limit,
  automatic_outreach_enabled,
  outreach_greeting_text,
  outreach_time_windows_json
FROM automation_settings
WHERE id = 1;

-- name: ConfigureAssessment :execrows
UPDATE automation_settings
SET automatic_assessment_enabled = sqlc.arg(automatic_assessment_enabled),
    assessment_processing_limit = sqlc.arg(assessment_processing_limit),
    updated_at = sqlc.arg(updated_at)
WHERE id = 1;
