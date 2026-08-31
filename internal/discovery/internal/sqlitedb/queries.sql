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

-- name: CreateSingleRangeDiscoveryRun :one
INSERT INTO discovery_runs (
    resume_version_id,
    current_role,
    current_city,
    next_page,
    status,
    attempt_no,
    worker_owner,
    worker_lease_until,
    created_at,
    prepared_at,
    updated_at
) VALUES (?, ?, ?, 1, 'running', 1, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetLatestDiscoveryRun :one
SELECT
    discovery_runs.id,
    discovery_runs.current_role,
    discovery_runs.current_city,
    discovery_runs.next_page,
    discovery_runs.status,
    online_resume_versions.version_no AS resume_version_no,
    online_resume_versions.resume_json
FROM discovery_runs
JOIN online_resume_versions
    ON online_resume_versions.id = discovery_runs.resume_version_id
ORDER BY discovery_runs.id DESC
LIMIT 1;

-- name: AdvanceSingleRangeDiscoveryPage :one
UPDATE discovery_runs
SET next_page = ?,
    consecutive_failure_count = 0,
    last_progress_at = ?,
    updated_at = ?
WHERE id = ?
  AND status = 'running'
  AND next_page = ?
RETURNING id;

-- name: CompleteSingleRangeDiscoveryRun :one
UPDATE discovery_runs
SET status = 'completed',
    consecutive_failure_count = 0,
    worker_owner = NULL,
    worker_lease_until = NULL,
    last_progress_at = ?,
    finished_at = ?,
    updated_at = ?
WHERE id = ?
  AND status = 'running'
  AND next_page = ?
RETURNING id;

-- name: FailSingleRangeDiscoveryRun :one
UPDATE discovery_runs
SET status = 'failed',
    consecutive_failure_count = consecutive_failure_count + 1,
    worker_owner = NULL,
    worker_lease_until = NULL,
    updated_at = ?
WHERE id = ?
  AND status = 'running'
RETURNING id;
