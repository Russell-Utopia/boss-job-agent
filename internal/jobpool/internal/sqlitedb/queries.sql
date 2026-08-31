-- name: ObservePlatformJob :one
INSERT INTO platform_jobs (
    platform_job_id,
    canonical_url,
    job_title,
    company_name,
    city_text,
    salary_text,
    jd_json,
    jd_hash,
    platform_status,
    platform_closed_reason,
    platform_status_checked_at,
    first_seen_at,
    last_seen_at,
    updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(platform_job_id) DO UPDATE SET
    canonical_url = excluded.canonical_url,
    job_title = excluded.job_title,
    company_name = excluded.company_name,
    city_text = excluded.city_text,
    salary_text = excluded.salary_text,
    jd_json = excluded.jd_json,
    jd_hash = excluded.jd_hash,
    platform_status = excluded.platform_status,
    platform_closed_reason = excluded.platform_closed_reason,
    platform_status_checked_at = excluded.platform_status_checked_at,
    last_seen_at = excluded.last_seen_at,
    updated_at = excluded.updated_at
RETURNING
    id,
    platform_job_id,
    canonical_url,
    job_title,
    company_name,
    city_text,
    salary_text,
    jd_json,
    jd_hash,
    platform_status,
    platform_closed_reason,
    platform_status_checked_at,
    first_seen_at,
    last_seen_at,
    updated_at;

-- name: ListPlatformJobs :many
SELECT
    id,
    platform_job_id,
    canonical_url,
    job_title,
    company_name,
    city_text,
    salary_text,
    jd_json,
    jd_hash,
    platform_status,
    platform_closed_reason,
    platform_status_checked_at,
    first_seen_at,
    last_seen_at,
    updated_at
FROM platform_jobs
ORDER BY first_seen_at, id;
