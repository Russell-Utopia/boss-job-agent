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
    canonical_url = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.canonical_url ELSE platform_jobs.canonical_url END,
    job_title = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.job_title ELSE platform_jobs.job_title END,
    company_name = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.company_name ELSE platform_jobs.company_name END,
    city_text = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.city_text ELSE platform_jobs.city_text END,
    salary_text = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.salary_text ELSE platform_jobs.salary_text END,
    jd_json = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.jd_json ELSE platform_jobs.jd_json END,
    jd_hash = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.jd_hash ELSE platform_jobs.jd_hash END,
    platform_status = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.platform_status ELSE platform_jobs.platform_status END,
    platform_closed_reason = CASE WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at THEN excluded.platform_closed_reason ELSE platform_jobs.platform_closed_reason END,
    platform_status_checked_at = MAX(platform_jobs.platform_status_checked_at, excluded.platform_status_checked_at),
    assessment_status = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN CASE
            WHEN platform_jobs.assessment_status IN ('pending', 'processing', 'failed')
            THEN 'pending'
            ELSE 'not_queued'
        END
        ELSE platform_jobs.assessment_status
    END,
    assessment_resume_version_id = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.assessment_resume_version_id END,
    assessment_jd_hash = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.assessment_jd_hash END,
    assessment_policy_version_id = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.assessment_policy_version_id END,
    evaluator_version = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.evaluator_version END,
    assessment_consecutive_failure_count = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN 0 ELSE platform_jobs.assessment_consecutive_failure_count END,
    assessment_reason = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.assessment_reason END,
    assessment_evidence_json = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.assessment_evidence_json END,
    assessment_retry_at = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.assessment_retry_at END,
    assessed_at = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
        THEN NULL ELSE platform_jobs.assessed_at END,
    outreach_status = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND platform_jobs.outreach_status = 'pending'
         AND (excluded.jd_hash <> platform_jobs.jd_hash OR excluded.platform_status = 'closed')
        THEN 'not_queued'
        ELSE platform_jobs.outreach_status
    END,
    outreach_greeting_text = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND platform_jobs.outreach_status = 'pending'
         AND (excluded.jd_hash <> platform_jobs.jd_hash OR excluded.platform_status = 'closed')
        THEN NULL
        ELSE platform_jobs.outreach_greeting_text
    END,
    lease_stage = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
         AND platform_jobs.lease_stage = 'assessment'
        THEN NULL ELSE platform_jobs.lease_stage END,
    lease_owner = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
         AND platform_jobs.lease_stage = 'assessment'
        THEN NULL ELSE platform_jobs.lease_owner END,
    lease_until = CASE
        WHEN excluded.platform_status_checked_at >= platform_jobs.platform_status_checked_at
         AND excluded.jd_hash <> platform_jobs.jd_hash
         AND platform_jobs.outreach_status <> 'contacted'
         AND platform_jobs.lease_stage = 'assessment'
        THEN NULL ELSE platform_jobs.lease_until END,
    last_seen_at = MAX(platform_jobs.last_seen_at, excluded.last_seen_at),
    updated_at = MAX(platform_jobs.updated_at, excluded.updated_at)
RETURNING *;

-- name: GetPlatformJob :one
SELECT *
FROM platform_jobs
WHERE id = sqlc.arg(job_id);

-- name: ReviewPlatformJob :one
UPDATE platform_jobs
SET human_verdict = sqlc.arg(human_verdict),
    human_reviewed_jd_hash = jd_hash,
    human_reviewed_at = sqlc.arg(reviewed_at),
    human_review_note = sqlc.narg(review_note),
    outreach_status = CASE
        WHEN sqlc.arg(human_verdict) = 'unsuitable' AND outreach_status = 'pending' THEN 'not_queued'
        ELSE outreach_status
    END,
    outreach_greeting_text = CASE
        WHEN sqlc.arg(human_verdict) = 'unsuitable' AND outreach_status = 'pending' THEN NULL
        ELSE outreach_greeting_text
    END,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(job_id)
RETURNING *;

-- name: QueueAuthorizedOutreach :one
UPDATE platform_jobs
SET outreach_status = 'pending',
    outreach_greeting_text = sqlc.arg(greeting_text),
    outreach_retry_at = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(job_id)
  AND platform_status = 'open'
  AND outreach_status = 'not_queued'
  AND (
      (
          human_verdict = 'suitable'
          AND human_reviewed_jd_hash = jd_hash
      )
      OR (
          human_verdict IS NULL
          AND assessment_status = 'suitable'
      )
  )
RETURNING id;

-- name: QueueAssessment :one
UPDATE platform_jobs
SET assessment_status = 'pending',
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(job_id)
  AND platform_status = 'open'
  AND outreach_status <> 'contacted'
  AND assessment_status = 'not_queued'
RETURNING id;

-- name: RetryAssessmentFailure :one
UPDATE platform_jobs
SET assessment_status = 'pending',
    assessment_resume_version_id = NULL,
    assessment_jd_hash = NULL,
    assessment_policy_version_id = NULL,
    evaluator_version = NULL,
    assessment_consecutive_failure_count = 0,
    assessment_reason = NULL,
    assessment_evidence_json = NULL,
    assessment_retry_at = NULL,
    assessed_at = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(job_id)
  AND platform_status = 'open'
  AND outreach_status <> 'contacted'
  AND assessment_status = 'failed'
  AND lease_stage IS NULL
RETURNING id;

-- name: RetryOutreachFailure :one
UPDATE platform_jobs
SET outreach_status = 'pending',
    outreach_consecutive_failure_count = 0,
    outreach_retry_at = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(job_id)
  AND platform_status = 'open'
  AND outreach_status = 'failed'
  AND lease_stage IS NULL
  AND (
      (human_verdict = 'suitable' AND human_reviewed_jd_hash = jd_hash)
      OR (human_verdict IS NULL AND assessment_status = 'suitable')
  )
RETURNING id;

-- name: AdmitAssessments :execrows
UPDATE platform_jobs
SET assessment_status = 'pending',
    updated_at = sqlc.arg(updated_at)
WHERE id IN (
    SELECT candidate.id
    FROM platform_jobs AS candidate
    WHERE candidate.platform_status = 'open'
      AND candidate.outreach_status <> 'contacted'
      AND candidate.assessment_status = 'not_queued'
    ORDER BY candidate.first_seen_at, candidate.id
    LIMIT sqlc.arg(admit_limit)
);

-- name: AdmitOutreach :execrows
UPDATE platform_jobs
SET outreach_status = 'pending',
    outreach_greeting_text = sqlc.arg(greeting_text),
    outreach_retry_at = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id IN (
    SELECT candidate.id
    FROM platform_jobs AS candidate
    WHERE candidate.platform_status = 'open'
      AND candidate.outreach_status = 'not_queued'
      AND (
          (
              candidate.human_verdict = 'suitable'
              AND candidate.human_reviewed_jd_hash = candidate.jd_hash
          )
          OR (
              candidate.human_verdict IS NULL
              AND candidate.assessment_status = 'suitable'
          )
      )
    ORDER BY candidate.first_seen_at, candidate.id
    LIMIT sqlc.arg(admit_limit)
);

-- name: ExpireAssessmentLeases :execrows
UPDATE platform_jobs
SET assessment_status = 'failed',
    assessment_consecutive_failure_count = assessment_consecutive_failure_count + 1,
    assessment_reason = sqlc.arg(reason),
    assessment_evidence_json = sqlc.arg(evidence_json),
    assessment_retry_at = sqlc.arg(expired_at),
    lease_stage = NULL,
    lease_owner = NULL,
    lease_until = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE assessment_status = 'processing'
  AND lease_stage = 'assessment'
  AND lease_until <= sqlc.arg(expired_at);

-- name: StopAssessmentRetriesAtLimit :execrows
UPDATE platform_jobs
SET assessment_retry_at = NULL
WHERE assessment_status = 'failed'
  AND assessment_consecutive_failure_count >= sqlc.arg(failure_limit)
  AND assessment_retry_at IS NOT NULL;

-- name: ClaimAssessments :many
UPDATE platform_jobs
SET assessment_status = 'processing',
    assessment_resume_version_id = sqlc.arg(resume_version_id),
    assessment_jd_hash = jd_hash,
    assessment_policy_version_id = sqlc.arg(policy_version_id),
    evaluator_version = sqlc.arg(evaluator_version),
    assessment_attempt_no = assessment_attempt_no + 1,
    assessment_consecutive_failure_count = CASE
        WHEN assessment_status = 'failed'
         AND assessment_resume_version_id = sqlc.arg(resume_version_id)
         AND assessment_jd_hash = jd_hash
         AND assessment_policy_version_id = sqlc.arg(policy_version_id)
         AND evaluator_version = sqlc.arg(evaluator_version)
        THEN assessment_consecutive_failure_count
        ELSE 0
    END,
    assessment_reason = NULL,
    assessment_evidence_json = NULL,
    assessment_retry_at = NULL,
    assessed_at = NULL,
    lease_stage = 'assessment',
    lease_owner = sqlc.arg(worker),
    lease_until = sqlc.arg(lease_until),
    updated_at = sqlc.arg(updated_at)
WHERE id IN (
    SELECT candidate.id
    FROM platform_jobs AS candidate
    WHERE candidate.platform_status = 'open'
      AND candidate.outreach_status <> 'contacted'
      AND (
          (
              candidate.lease_stage IS NULL
              AND candidate.assessment_status = 'pending'
          )
          OR (
              candidate.lease_stage IS NULL
              AND
              candidate.assessment_status = 'failed'
              AND candidate.assessment_retry_at IS NOT NULL
              AND candidate.assessment_retry_at <= sqlc.arg(claimed_at)
              AND candidate.assessment_consecutive_failure_count < sqlc.arg(failure_limit)
          )
      )
    ORDER BY
        CASE candidate.assessment_status WHEN 'pending' THEN 0 ELSE 1 END,
        candidate.first_seen_at,
        candidate.id
    LIMIT sqlc.arg(claim_limit)
)
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
    assessment_resume_version_id,
    assessment_policy_version_id,
    evaluator_version,
    assessment_attempt_no,
    lease_until;

-- name: FinishAssessment :one
UPDATE platform_jobs
SET assessment_status = sqlc.arg(result_status),
    assessment_consecutive_failure_count = CASE
        WHEN sqlc.arg(result_status) = 'failed'
        THEN assessment_consecutive_failure_count + 1
        ELSE 0
    END,
    assessment_reason = sqlc.arg(reason),
    assessment_evidence_json = sqlc.arg(evidence_json),
    assessment_retry_at = CASE
        WHEN sqlc.arg(result_status) = 'failed' THEN sqlc.narg(retry_at)
        ELSE NULL
    END,
    assessed_at = CASE
        WHEN sqlc.arg(result_status) = 'suitable'
          OR sqlc.arg(result_status) = 'unsuitable'
          OR sqlc.arg(result_status) = 'needs_user_confirmation'
        THEN sqlc.arg(completed_at)
        ELSE NULL
    END,
    lease_stage = NULL,
    lease_owner = NULL,
    lease_until = NULL,
    updated_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(job_id)
  AND assessment_status = 'processing'
  AND assessment_attempt_no = sqlc.arg(attempt_no)
  AND lease_stage = 'assessment'
RETURNING id;

-- name: ExpireOutreachLeases :execrows
UPDATE platform_jobs
SET outreach_status = 'possibly_contacted',
    outreach_retry_at = NULL,
    lease_stage = NULL,
    lease_owner = NULL,
    lease_until = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE outreach_status = 'processing'
  AND lease_stage = 'outreach'
  AND lease_until <= sqlc.arg(expired_at);

-- name: GetOutreachClaimCandidate :one
SELECT id, outreach_status
FROM platform_jobs
WHERE lease_stage IS NULL
  AND (
      outreach_status = 'possibly_contacted'
      OR (
          outreach_status = 'pending'
          AND platform_status = 'open'
          AND (
              (human_verdict = 'suitable' AND human_reviewed_jd_hash = jd_hash)
              OR (human_verdict IS NULL AND assessment_status = 'suitable')
          )
      )
      OR (
          outreach_status = 'failed'
          AND outreach_retry_at IS NOT NULL
          AND outreach_retry_at <= sqlc.arg(claimed_at)
          AND outreach_consecutive_failure_count < sqlc.arg(failure_limit)
          AND platform_status = 'open'
          AND (
              (human_verdict = 'suitable' AND human_reviewed_jd_hash = jd_hash)
              OR (human_verdict IS NULL AND assessment_status = 'suitable')
          )
      )
  )
ORDER BY
    CASE outreach_status WHEN 'possibly_contacted' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END,
    first_seen_at,
    id
LIMIT 1;

-- name: ClaimOutreachWork :one
UPDATE platform_jobs
SET outreach_status = 'processing',
    outreach_attempt_no = outreach_attempt_no + 1,
    outreach_last_attempt_at = sqlc.arg(claimed_at),
    outreach_retry_at = NULL,
    lease_stage = 'outreach',
    lease_owner = sqlc.arg(worker),
    lease_until = sqlc.arg(lease_until),
    updated_at = sqlc.arg(claimed_at)
WHERE id = sqlc.arg(job_id)
  AND outreach_status = sqlc.arg(expected_status)
  AND lease_stage IS NULL
  AND (
      sqlc.arg(expected_status) = 'possibly_contacted'
      OR (
          platform_status = 'open'
          AND (
              (human_verdict = 'suitable' AND human_reviewed_jd_hash = jd_hash)
              OR (human_verdict IS NULL AND assessment_status = 'suitable')
          )
      )
  )
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
    outreach_greeting_text,
    outreach_attempt_no,
    lease_until;

-- name: FinishOutreachWork :one
UPDATE platform_jobs
SET outreach_status = sqlc.arg(result_status),
    outreach_consecutive_failure_count = CASE
        WHEN sqlc.arg(result_status) = 'failed' THEN outreach_consecutive_failure_count + 1
        WHEN sqlc.arg(result_status) = 'contacted' THEN 0
        ELSE outreach_consecutive_failure_count
    END,
    outreach_retry_at = CASE
        WHEN sqlc.arg(result_status) = 'failed' THEN sqlc.narg(retry_at)
        ELSE NULL
    END,
    outreach_evidence_json = sqlc.arg(evidence_json),
    contact_source = CASE
        WHEN sqlc.arg(result_status) = 'contacted' THEN sqlc.arg(contact_source)
        ELSE NULL
    END,
    contacted_at = CASE
        WHEN sqlc.arg(result_status) = 'contacted' THEN sqlc.arg(completed_at)
        ELSE NULL
    END,
    lease_stage = NULL,
    lease_owner = NULL,
    lease_until = NULL,
    updated_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(job_id)
  AND outreach_status = 'processing'
  AND outreach_attempt_no = sqlc.arg(attempt_no)
  AND lease_stage = 'outreach'
RETURNING id;

-- name: StopOutreachRetriesAtLimit :execrows
UPDATE platform_jobs
SET outreach_retry_at = NULL
WHERE outreach_status = 'failed'
  AND outreach_consecutive_failure_count >= sqlc.arg(failure_limit)
  AND outreach_retry_at IS NOT NULL;

-- name: ListPlatformJobs :many
SELECT *
FROM platform_jobs
ORDER BY first_seen_at, id;
