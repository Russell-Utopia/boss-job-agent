-- name: CountBusinessRows :one
SELECT
  (SELECT count(*) FROM online_resume_versions) AS online_resume_versions,
  (SELECT count(*) FROM assessment_policy_versions) AS assessment_policy_versions,
  (SELECT count(*) FROM discovery_runs) AS discovery_runs,
  (SELECT count(*) FROM platform_jobs) AS platform_jobs,
  (SELECT count(*) FROM automation_settings) AS automation_settings;
