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

-- name: GetPolicyVersion :one
SELECT id, version_no, rules_json, change_note
FROM assessment_policy_versions
WHERE id = sqlc.arg(policy_id);

-- name: GetNextPolicyVersionNumber :one
SELECT COALESCE(MAX(version_no), 0) + 1
FROM assessment_policy_versions;

-- name: DeactivatePolicies :exec
UPDATE assessment_policy_versions
SET is_active = 0
WHERE is_active = 1;

-- name: CreatePolicyVersion :one
INSERT INTO assessment_policy_versions (
    version_no, rules_json, is_active, change_note, created_at
) VALUES (
    sqlc.arg(version_no), sqlc.arg(rules_json), 1, sqlc.narg(change_note), sqlc.arg(created_at)
)
RETURNING id, version_no, rules_json, change_note;
