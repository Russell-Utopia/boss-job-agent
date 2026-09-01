-- name: GetActiveDiscoveryResumeUse :one
SELECT
    discovery_runs.id AS discovery_run_id,
    COALESCE(online_resume_versions.version_no, 0) AS resume_version_no
FROM discovery_runs
LEFT JOIN online_resume_versions
    ON online_resume_versions.id = discovery_runs.resume_version_id
WHERE discovery_runs.status IN ('preparing', 'running', 'paused', 'failed')
ORDER BY discovery_runs.id DESC
LIMIT 1;

-- name: CreateDiscoveryRun :one
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
) VALUES (
    sqlc.arg(resume_version_id),
    sqlc.arg(current_role),
    sqlc.arg(current_city),
    1,
    'running',
    1,
    sqlc.arg(worker_owner),
    sqlc.arg(worker_lease_until),
    sqlc.arg(created_at),
    sqlc.arg(prepared_at),
    sqlc.arg(updated_at)
)
RETURNING id;

-- name: GetLatestDiscoveryRun :one
SELECT
    discovery_runs.id,
    discovery_runs.current_role,
    discovery_runs.current_city,
    discovery_runs.next_page,
    discovery_runs.status,
    discovery_runs.attempt_no,
    discovery_runs.consecutive_failure_count,
    discovery_runs.retry_at,
    discovery_runs.worker_owner,
    discovery_runs.worker_lease_until,
    discovery_runs.current_page_job_ids_json,
    discovery_runs.current_page_has_more,
    discovery_runs.next_job_ordinal,
    COALESCE(online_resume_versions.version_no, 0) AS resume_version_no,
    COALESCE(online_resume_versions.resume_json, '') AS resume_json
FROM discovery_runs
LEFT JOIN online_resume_versions
    ON online_resume_versions.id = discovery_runs.resume_version_id
ORDER BY discovery_runs.id DESC
LIMIT 1;

-- name: FreezeDiscoveryPage :one
UPDATE discovery_runs
SET current_page_job_ids_json = sqlc.arg(job_ids_json),
    current_page_has_more = sqlc.arg(has_more),
    next_job_ordinal = 0,
    consecutive_failure_count = 0,
    last_progress_at = sqlc.arg(progress_at),
    worker_lease_until = sqlc.arg(worker_lease_until),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND attempt_no = sqlc.arg(attempt_no)
  AND worker_owner = sqlc.arg(worker_owner)
  AND current_role = sqlc.arg(current_role)
  AND current_city = sqlc.arg(current_city)
  AND next_page = sqlc.arg(next_page)
  AND current_page_job_ids_json IS NULL
  AND current_page_has_more IS NULL
  AND next_job_ordinal IS NULL
RETURNING id;

-- name: LockCurrentDiscoveryJob :one
UPDATE discovery_runs
SET worker_lease_until = worker_lease_until
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND attempt_no = sqlc.arg(attempt_no)
  AND worker_owner = sqlc.arg(worker_owner)
  AND current_role = sqlc.arg(current_role)
  AND current_city = sqlc.arg(current_city)
  AND next_page = sqlc.arg(next_page)
  AND current_page_job_ids_json = sqlc.arg(job_ids_json)
  AND current_page_has_more = sqlc.arg(has_more)
  AND next_job_ordinal = sqlc.arg(job_ordinal)
  AND json_extract(
        current_page_job_ids_json,
        '$[' || next_job_ordinal || ']'
      ) = sqlc.arg(platform_job_id)
RETURNING id;

-- name: AdvanceDiscoveryJob :one
UPDATE discovery_runs
SET next_job_ordinal = sqlc.arg(next_job_ordinal),
    consecutive_failure_count = 0,
    last_progress_at = sqlc.arg(progress_at),
    worker_lease_until = sqlc.arg(worker_lease_until),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND attempt_no = sqlc.arg(attempt_no)
  AND worker_owner = sqlc.arg(worker_owner)
  AND current_role = sqlc.arg(current_role)
  AND current_city = sqlc.arg(current_city)
  AND next_page = sqlc.arg(current_page)
  AND current_page_job_ids_json = sqlc.arg(job_ids_json)
  AND current_page_has_more = sqlc.arg(has_more)
  AND next_job_ordinal = sqlc.arg(current_job_ordinal)
  AND json_extract(
        current_page_job_ids_json,
        '$[' || next_job_ordinal || ']'
      ) = sqlc.arg(platform_job_id)
RETURNING id;

-- name: SwitchDiscoveryRange :one
UPDATE discovery_runs
SET current_role = sqlc.arg(next_role),
    current_city = sqlc.arg(next_city),
    next_page = 1,
    current_page_job_ids_json = NULL,
    current_page_has_more = NULL,
    next_job_ordinal = NULL,
    consecutive_failure_count = 0,
    last_progress_at = sqlc.arg(progress_at),
    worker_lease_until = sqlc.arg(worker_lease_until),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND attempt_no = sqlc.arg(attempt_no)
  AND worker_owner = sqlc.arg(worker_owner)
  AND current_role = sqlc.arg(current_role)
  AND current_city = sqlc.arg(current_city)
  AND next_page = sqlc.arg(current_page)
  AND current_page_job_ids_json IS NOT NULL
  AND next_job_ordinal = json_array_length(current_page_job_ids_json)
RETURNING id;

-- name: AdvanceDiscoveryPage :one
UPDATE discovery_runs
SET next_page = sqlc.arg(next_page),
    current_page_job_ids_json = NULL,
    current_page_has_more = NULL,
    next_job_ordinal = NULL,
    consecutive_failure_count = 0,
    last_progress_at = sqlc.arg(progress_at),
    worker_lease_until = sqlc.arg(worker_lease_until),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND attempt_no = sqlc.arg(attempt_no)
  AND worker_owner = sqlc.arg(worker_owner)
  AND current_role = sqlc.arg(current_role)
  AND current_city = sqlc.arg(current_city)
  AND next_page = sqlc.arg(current_page)
  AND current_page_job_ids_json IS NOT NULL
  AND next_job_ordinal = json_array_length(current_page_job_ids_json)
RETURNING id;

-- name: CompleteDiscoveryRun :one
UPDATE discovery_runs
SET status = 'completed',
    current_page_job_ids_json = NULL,
    current_page_has_more = NULL,
    next_job_ordinal = NULL,
    consecutive_failure_count = 0,
    retry_at = NULL,
    worker_owner = NULL,
    worker_lease_until = NULL,
    last_progress_at = sqlc.arg(progress_at),
    finished_at = sqlc.arg(finished_at),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND attempt_no = sqlc.arg(attempt_no)
  AND worker_owner = sqlc.arg(worker_owner)
  AND current_role = sqlc.arg(current_role)
  AND current_city = sqlc.arg(current_city)
  AND next_page = sqlc.arg(current_page)
  AND current_page_job_ids_json IS NOT NULL
  AND next_job_ordinal = json_array_length(current_page_job_ids_json)
RETURNING id;

-- name: FailDiscoveryRun :one
UPDATE discovery_runs
SET status = 'failed',
    consecutive_failure_count = consecutive_failure_count + 1,
    retry_at = sqlc.narg(retry_at),
    worker_owner = NULL,
    worker_lease_until = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND attempt_no = sqlc.arg(attempt_no)
  AND worker_owner = sqlc.arg(worker_owner)
RETURNING id;

-- name: PauseDiscoveryRun :one
UPDATE discovery_runs
SET status = 'paused',
    retry_at = NULL,
    worker_owner = NULL,
    worker_lease_until = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
RETURNING id;

-- name: ContinueDiscoveryRun :one
UPDATE discovery_runs
SET status = 'running',
    attempt_no = attempt_no + 1,
    consecutive_failure_count = 0,
    retry_at = NULL,
    worker_owner = sqlc.arg(worker_owner),
    worker_lease_until = sqlc.arg(worker_lease_until),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status IN ('paused', 'failed')
  AND resume_version_id IS NOT NULL
  AND current_role IS NOT NULL
  AND current_city IS NOT NULL
  AND next_page IS NOT NULL
RETURNING attempt_no;

-- name: ClaimDueDiscoveryRetry :one
UPDATE discovery_runs
SET status = 'running',
    attempt_no = attempt_no + 1,
    retry_at = NULL,
    worker_owner = sqlc.arg(worker_owner),
    worker_lease_until = sqlc.arg(worker_lease_until),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'failed'
  AND retry_at IS NOT NULL
  AND retry_at <= sqlc.arg(now)
RETURNING attempt_no;

-- name: ExpireDiscoveryWorker :one
UPDATE discovery_runs
SET status = 'failed',
    consecutive_failure_count = consecutive_failure_count + 1,
    retry_at = NULL,
    worker_owner = NULL,
    worker_lease_until = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status = 'running'
  AND worker_owner <> sqlc.arg(current_worker_owner)
  AND worker_lease_until IS NOT NULL
  AND worker_lease_until <= sqlc.arg(now)
RETURNING id;

-- name: EndDiscoveryRunEarly :one
UPDATE discovery_runs
SET status = 'ended_early',
    current_page_job_ids_json = NULL,
    current_page_has_more = NULL,
    next_job_ordinal = NULL,
    retry_at = NULL,
    worker_owner = NULL,
    worker_lease_until = NULL,
    finished_at = sqlc.arg(finished_at),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(run_id)
  AND status IN ('preparing', 'running', 'paused', 'failed')
RETURNING id;
