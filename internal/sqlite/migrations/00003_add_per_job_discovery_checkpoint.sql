-- +goose Up
-- +goose StatementBegin

ALTER TABLE discovery_runs
ADD COLUMN current_page_job_ids_json TEXT;

ALTER TABLE discovery_runs
ADD COLUMN current_page_has_more INTEGER
    CHECK (current_page_has_more IS NULL OR current_page_has_more IN (0, 1));

ALTER TABLE discovery_runs
ADD COLUMN next_job_ordinal INTEGER
    CHECK (next_job_ordinal IS NULL OR next_job_ordinal >= 0);

CREATE TRIGGER trg_discovery_runs_validate_page_checkpoint_insert
BEFORE INSERT ON discovery_runs
WHEN NOT (
    (
        NEW.current_page_job_ids_json IS NULL
        AND NEW.current_page_has_more IS NULL
        AND NEW.next_job_ordinal IS NULL
    )
    OR
    (
        NEW.current_page_job_ids_json IS NOT NULL
        AND NEW.current_page_has_more IS NOT NULL
        AND NEW.next_job_ordinal IS NOT NULL
        AND NEW.status IN ('running', 'paused', 'failed')
        AND NEW.current_role IS NOT NULL
        AND NEW.current_city IS NOT NULL
        AND NEW.next_page IS NOT NULL
        AND CASE
            WHEN json_valid(NEW.current_page_job_ids_json) THEN
                json_type(NEW.current_page_job_ids_json) = 'array'
                AND NEW.next_job_ordinal <= json_array_length(NEW.current_page_job_ids_json)
                AND NOT EXISTS (
                    SELECT 1
                    FROM json_each(NEW.current_page_job_ids_json)
                    WHERE type <> 'text' OR trim(CAST(value AS TEXT)) = ''
                )
                AND (
                    SELECT count(*) = count(DISTINCT CAST(value AS TEXT))
                    FROM json_each(NEW.current_page_job_ids_json)
                )
            ELSE 0
        END
    )
)
BEGIN
    SELECT RAISE(ABORT, 'invalid discovery page checkpoint');
END;

CREATE TRIGGER trg_discovery_runs_validate_page_checkpoint_update
BEFORE UPDATE OF
    current_page_job_ids_json,
    current_page_has_more,
    next_job_ordinal,
    status,
    current_role,
    current_city,
    next_page
ON discovery_runs
WHEN NOT (
    (
        NEW.current_page_job_ids_json IS NULL
        AND NEW.current_page_has_more IS NULL
        AND NEW.next_job_ordinal IS NULL
    )
    OR
    (
        NEW.current_page_job_ids_json IS NOT NULL
        AND NEW.current_page_has_more IS NOT NULL
        AND NEW.next_job_ordinal IS NOT NULL
        AND NEW.status IN ('running', 'paused', 'failed')
        AND NEW.current_role IS NOT NULL
        AND NEW.current_city IS NOT NULL
        AND NEW.next_page IS NOT NULL
        AND CASE
            WHEN json_valid(NEW.current_page_job_ids_json) THEN
                json_type(NEW.current_page_job_ids_json) = 'array'
                AND NEW.next_job_ordinal <= json_array_length(NEW.current_page_job_ids_json)
                AND NOT EXISTS (
                    SELECT 1
                    FROM json_each(NEW.current_page_job_ids_json)
                    WHERE type <> 'text' OR trim(CAST(value AS TEXT)) = ''
                )
                AND (
                    SELECT count(*) = count(DISTINCT CAST(value AS TEXT))
                    FROM json_each(NEW.current_page_job_ids_json)
                )
            ELSE 0
        END
    )
)
BEGIN
    SELECT RAISE(ABORT, 'invalid discovery page checkpoint');
END;

-- +goose StatementEnd
