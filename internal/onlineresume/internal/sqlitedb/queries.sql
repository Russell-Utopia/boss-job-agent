-- name: GetCurrentOnlineResume :one
SELECT id, version_no, resume_json, resume_hash, created_at
FROM online_resume_versions
WHERE is_current = 1;

-- name: GetNextOnlineResumeVersionNumber :one
SELECT COALESCE(MAX(version_no), 0) + 1
FROM online_resume_versions;

-- name: ClearCurrentOnlineResume :exec
UPDATE online_resume_versions
SET is_current = 0
WHERE is_current = 1;

-- name: CreateCurrentOnlineResume :one
INSERT INTO online_resume_versions (
    version_no,
    resume_json,
    resume_hash,
    is_current,
    created_at
) VALUES (?, ?, ?, 1, ?)
RETURNING id, version_no, resume_json, resume_hash, created_at;
