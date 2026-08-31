-- +goose Up
-- +goose StatementBegin

-- 00001 已正式采用，只通过前向 migration 放宽未完成准备运行的提前结束。
-- defer_foreign_keys 让引用 discovery_runs 的平台岗位在同一事务内等待父表重建完成。
PRAGMA defer_foreign_keys = ON;

CREATE TABLE new_discovery_runs (
    -- SQLite 自动生成的运行主键。
    id INTEGER PRIMARY KEY,

    -- 用户可见名称；为空时可由程序根据开始时间生成。
    name TEXT,

    -- 本次运行全部搜索范围共同使用的在线简历版本。
    -- preparing 状态下允许为空；首次写入后不可修改或清空，恢复同一运行仍引用它。
    -- 求职者手动刷新出的新当前版本，只供新运行和尚未开始的鉴定使用。
    -- 它不决定任何岗位使用哪个鉴定策略版本。
    resume_version_id INTEGER,

    -- 当前正在搜索的意向岗位原文。
    -- 准备完成后与 current_city、next_page 一起构成唯一恢复检查点。
    current_role TEXT,

    -- 当前正在搜索的目标城市。
    current_city TEXT,

    -- 下次应读取的 BOSS 搜索页，从 1 开始。
    -- 只有当前页全部岗位都已写入 platform_jobs 后才允许递增。
    next_page INTEGER
        CHECK (next_page IS NULL OR next_page > 0),

    -- 发现运行状态；它只描述岗位搜索，不等待全局鉴定或打招呼队列收口。
    status TEXT NOT NULL DEFAULT 'preparing'
        CHECK (
            status IN (
                'preparing',   -- 正在读取并记录用于搜索的在线简历
                'running',     -- 后端 Worker 正在推进搜索
                'paused',      -- 求职者主动暂停，可恢复同一运行
                'failed',      -- 发现流程明确失败
                'completed',   -- 所有搜索范围都取得明确耗尽证据
                'ended_early'  -- 至少一个搜索范围尚未确认耗尽
            )
        ),

    -- 该岗位发现运行生命周期内已经开始过多少次执行尝试。
    -- 每次开始或恢复执行时递增且永不归零，用于定位当前执行和拒绝旧 Worker 写入。
    attempt_no INTEGER NOT NULL DEFAULT 0
        CHECK (attempt_no >= 0),

    -- 当前无人干预执行周期内的连续失败次数，不等于生命周期尝试编号。
    -- 自动重试不会归零；完成准备、成功推进搜索检查点或求职者显式重新开始后归零。
    consecutive_failure_count INTEGER NOT NULL DEFAULT 0
        CHECK (consecutive_failure_count >= 0),

    -- failed 后最早允许自动重试的时间；为空表示必须由求职者处理或已达重试上限。
    -- v1 默认一个无人干预周期总共最多尝试三次（含首次），上限由程序配置而非 DDL 写死。
    -- 自动重试开始时清空本字段并递增 attempt_no，但不清零连续失败次数。
    retry_at INTEGER,

    -- 当前拥有该运行的发现 Worker；只有 running 状态保存。
    worker_owner TEXT,

    -- 当前发现 Worker 的租约截止时间。
    -- 界面关闭不会清除租约；后端崩溃后租约过期用于识别失联运行。
    worker_lease_until INTEGER,

    -- 运行创建时间。
    created_at INTEGER NOT NULL,

    -- 用于搜索的在线简历版本成功记录的时间。
    prepared_at INTEGER,

    -- 最近一次成功写入岗位或推进搜索位置的时间。
    last_progress_at INTEGER,

    -- 进入 completed 或 ended_early 的时间。
    finished_at INTEGER,

    -- 本行最后更新时间。
    updated_at INTEGER NOT NULL,

    FOREIGN KEY (resume_version_id)
        REFERENCES online_resume_versions(id)
        ON DELETE RESTRICT,

    -- 搜索检查点必须全部为空，或者全部存在。
    CHECK (
        (
            current_role IS NULL
            AND current_city IS NULL
            AND next_page IS NULL
        )
        OR
        (
            current_role IS NOT NULL
            AND current_city IS NOT NULL
            AND next_page IS NOT NULL
        )
    ),

    -- 执行过搜索的运行必须冻结在线简历版本；未完成准备就提前结束时不伪造输入。
    CHECK (
        status IN ('preparing', 'failed')
        OR (
            status = 'ended_early'
            AND resume_version_id IS NULL
            AND prepared_at IS NULL
        )
        OR (
            resume_version_id IS NOT NULL
            AND prepared_at IS NOT NULL
        )
    ),

    -- 只有 running 状态允许保存发现 Worker 租约。
    CHECK (
        (
            status = 'running'
            AND worker_owner IS NOT NULL
            AND worker_lease_until IS NOT NULL
        )
        OR
        (
            status <> 'running'
            AND worker_owner IS NULL
            AND worker_lease_until IS NULL
        )
    ),

    -- 只有终态保存结束时间。
    CHECK (
        (
            status IN ('completed', 'ended_early')
            AND finished_at IS NOT NULL
        )
        OR
        (
            status NOT IN ('completed', 'ended_early')
            AND finished_at IS NULL
        )
    ),

    -- 连续失败次数不能超过已经开始的生命周期尝试数；失败状态至少发生过一次失败。
    CHECK (
        consecutive_failure_count <= attempt_no
        AND (
            status <> 'failed'
            OR consecutive_failure_count > 0
        )
    ),

    -- 只有失败状态可以等待自动重试；其他状态不能遗留旧重试时间。
    CHECK (
        status = 'failed'
        OR retry_at IS NULL
    )
);

INSERT INTO new_discovery_runs (
    id,
    name,
    resume_version_id,
    current_role,
    current_city,
    next_page,
    status,
    attempt_no,
    consecutive_failure_count,
    retry_at,
    worker_owner,
    worker_lease_until,
    created_at,
    prepared_at,
    last_progress_at,
    finished_at,
    updated_at
)
SELECT
    id,
    name,
    resume_version_id,
    current_role,
    current_city,
    next_page,
    status,
    attempt_no,
    consecutive_failure_count,
    retry_at,
    worker_owner,
    worker_lease_until,
    created_at,
    prepared_at,
    last_progress_at,
    finished_at,
    updated_at
FROM discovery_runs;

DROP TABLE discovery_runs;
ALTER TABLE new_discovery_runs RENAME TO discovery_runs;

-- v1 同时最多只有一个准备中、运行中、暂停中或失败待处理的岗位发现运行。
-- 用户必须继续它，或明确提前结束后才能创建新运行。
-- completed/ended_early 不受此限制；全局鉴定或打招呼队列仍有工作时也可新建。
CREATE UNIQUE INDEX idx_discovery_runs_one_unfinished
    ON discovery_runs((1))
    WHERE status IN ('preparing', 'running', 'paused', 'failed');

CREATE INDEX idx_discovery_runs_status
    ON discovery_runs(status, created_at);

-- 一轮发现必须始终使用同一在线简历版本。
-- 允许 preparing 首次从空值写入版本；一旦写入，任何后续状态都不能切换或清空。
CREATE TRIGGER trg_discovery_runs_keep_resume_version
BEFORE UPDATE OF resume_version_id ON discovery_runs
WHEN OLD.resume_version_id IS NOT NULL
     AND NEW.resume_version_id IS NOT OLD.resume_version_id
BEGIN
    SELECT RAISE(ABORT, 'discovery run resume version is immutable');
END;

-- +goose StatementEnd

