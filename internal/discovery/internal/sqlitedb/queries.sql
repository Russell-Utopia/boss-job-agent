-- name: GetActiveDiscoveryResumeUse :one
SELECT
    discovery_runs.id AS discovery_run_id,
    online_resume_versions.version_no AS resume_version_no
FROM discovery_runs
JOIN online_resume_versions
    ON online_resume_versions.id = discovery_runs.resume_version_id
WHERE discovery_runs.status IN ('preparing', 'running', 'paused', 'failed')
ORDER BY discovery_runs.id DESC
LIMIT 1;
