-- name: GetCurrentOnlineResume :one
SELECT id, version_no, created_at
FROM online_resume_versions
WHERE is_current = 1;
