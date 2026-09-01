-- name: EnsureDefaultPolicy :exec
INSERT OR IGNORE INTO assessment_policy_versions (
  version_no, rules_json, is_active, change_note, created_at
) VALUES (
  1, sqlc.arg(rules_json), 1, sqlc.arg(change_note), sqlc.arg(created_at)
);

-- name: GetActivePolicy :one
SELECT id, version_no, rules_json, change_note
FROM assessment_policy_versions
WHERE is_active = 1;
