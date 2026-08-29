-- BOSS Job Agent SQLite 逻辑 DDL 草案
--
-- 本文件用于评审数据模型，不是已经投入使用的 migration。
-- v1 只使用五张 SQLite 业务表：
--   1. online_resume_versions      用户手动刷新取得的不可变在线简历版本
--   2. assessment_policy_versions  已经由求职者确认保存的不可变岗位鉴定策略版本
--   3. discovery_runs              用户可见、可暂停和恢复的岗位发现运行
--   4. platform_jobs               全局岗位池及鉴定、人工复核和首次打招呼状态
--   5. automation_settings         整个本地实例当前采用的自动化设置
--
-- 不建立 task_jobs：岗位不属于某次发现运行，运行与岗位之间的历史关联
-- 不参与产品判断、恢复或展示，必要时只写普通运行日志。
-- 不建立 search_progress：每个发现运行只有一个当前搜索位置，直接保存在运行本身。
--
-- 所有业务实体 id 都使用 SQLite 的 INTEGER PRIMARY KEY 自动生成；
-- 唯一例外是没有实体身份的单行 automation_settings，固定使用 id=1。
-- 所有时间都使用 Unix 毫秒，由 Go 程序写入。
-- JSON 字段只保存结构化业务数据，不保存电话、微信等无关敏感信息。
-- 技术错误、暂停原因和提前结束原因写入持久化结构化运行日志，
-- 以 trace_id、业务实体标识和尝试号检索，不在业务表中重复保存错误文本。

PRAGMA foreign_keys = ON;

-- ============================================================
-- 1. 在线简历版本
--
-- 在线简历是搜索条件和 AI 鉴定资料的唯一来源。
-- 只有求职者显式刷新时才访问 BOSS；发现和鉴定过程都不会自动刷新。
-- 发现运行和实际鉴定分别记录所采用的版本，因而可以还原当时的完整输入。
-- ============================================================
CREATE TABLE online_resume_versions (
    -- SQLite 自动生成的内部主键；不在界面展示。
    id INTEGER PRIMARY KEY,

    -- 面向用户展示的递增版本号，例如 1、2、3。
    version_no INTEGER NOT NULL UNIQUE
        CHECK (version_no > 0),

    -- 参与搜索和鉴定的在线简历结构化内容。
    -- 包括求职条件、工作经历、项目经历、教育经历和技能，
    -- 不保存电话、微信等不参与判断的敏感信息。
    resume_json TEXT NOT NULL
        CHECK (
            json_valid(resume_json)
            AND json_type(resume_json) = 'object'
        ),

    -- 规范化 resume_json 的内容哈希。
    -- 与当前版本内容相同时刷新不新增版本；从其他内容改回旧内容时仍新增版本，
    -- 以保留用户实际确认变更的先后顺序。
    resume_hash TEXT NOT NULL,

    -- 是否为求职者最近一次成功刷新并确认的当前在线简历版本。
    -- 待鉴定岗位真正开始执行时采用当前版本；已经开始的鉴定不随它变化。
    is_current INTEGER NOT NULL DEFAULT 0
        CHECK (is_current IN (0, 1)),

    -- 该版本由求职者成功刷新并保存的时间。
    created_at INTEGER NOT NULL
);

-- 整个本地实例最多只有一个当前在线简历版本；首次手动刷新前可以没有。
CREATE UNIQUE INDEX idx_online_resume_versions_one_current
    ON online_resume_versions(is_current)
    WHERE is_current = 1;

CREATE INDEX idx_online_resume_versions_hash
    ON online_resume_versions(resume_hash);


-- ============================================================
-- 2. 岗位鉴定策略版本
--
-- 用户接受一次策略修改时新增一行；rules_json 创建后不再修改。
-- 平台岗位的当前 AI 结论继续标明实际采用的版本。
-- 模型生成的策略候选稿只存在于当次界面交互，不写入 SQLite；
-- 只有求职者确认采用后的完整内容才成为本表中的新版本。
-- 新实例初始化时必须同时写入并启用默认第 1 版；因此正常业务状态下始终有当前策略。
-- 默认版也是真实的不可变版本，不使用空值、代码分支或额外字段表达。
-- ============================================================
CREATE TABLE assessment_policy_versions (
    -- SQLite 自动生成的本地主键。
    id INTEGER PRIMARY KEY,

    -- 面向用户展示的递增版本号，例如 1、2、3。
    version_no INTEGER NOT NULL UNIQUE
        CHECK (version_no > 0),

    -- 用户可理解、可配置的鉴定规则。
    -- 例如“拒绝明确要求纯 Java 的岗位，允许未声明技术栈的后端岗位”。
    rules_json TEXT NOT NULL
        CHECK (json_valid(rules_json)),

    -- 是否为新的鉴定执行默认采用的当前策略。
    -- 接受新策略时，应在同一事务中停用旧版本并启用新版本。
    is_active INTEGER NOT NULL DEFAULT 0
        CHECK (is_active IN (0, 1)),

    -- 用户为什么创建或接受这一版策略，便于后续回顾。
    change_note TEXT,

    -- 策略版本创建时间。
    created_at INTEGER NOT NULL
);

-- 整个本地实例最多只能有一个当前启用策略。
CREATE UNIQUE INDEX idx_policy_versions_one_active
    ON assessment_policy_versions(is_active)
    WHERE is_active = 1;

-- 首次初始化写入的默认第 1 版采用保守判据：
--   1. 只依据本次实际采用的在线简历和 JD，不猜测未提供的经历；
--   2. 有明确且重要的不匹配证据时判为 unsuitable；
--   3. 有明确匹配证据时判为 suitable；
--   4. 信息不足或证据冲突时判为 needs_user_confirmation。
-- 它的 is_active=1、change_note='系统默认策略'；created_at 由初始化程序写入。
-- 后续调整只能新增并启用更高版本，不能覆盖本行。


-- ============================================================
-- 3. 岗位发现运行
--
-- 一行代表一次用户可见的岗位搜索过程。
-- 它只拥有本次采用的搜索输入、当前位置和恢复状态，不拥有发现的岗位。
-- Web 关闭不改变本行；后端仍在运行时继续推进。
-- 后端中断后依据 Worker 租约识别失联运行，再由用户选择继续或提前结束。
-- ============================================================
CREATE TABLE discovery_runs (
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

    -- 离开 preparing 后，用于搜索的在线简历版本和准备时间必须完整；
    -- 但准备阶段本身也可能失败，此时 failed 允许尚无完整输入。
    CHECK (
        status IN ('preparing', 'failed')
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


-- ============================================================
-- 4. 全局平台岗位
--
-- 一行代表 BOSS 上一个稳定 platform_job_id。
-- 同一公司重新发布相同或相似 JD 但产生新 platform_job_id 时，必须新增一行；
-- 不按公司名、岗位名或 jd_hash 合并，也不从旧行复制鉴定、人工结论或打招呼状态。
-- 当前 JD、平台岗位状态、AI 鉴定、人工结论和首次打招呼都只保存在本行。
-- 多次岗位发现运行只插入或更新这一行，不产生运行与岗位关联记录。
-- platform_status=open、outreach_status<>contacted 且 assessment_status=pending
-- 共同构成全局鉴定逻辑队列；同一平台岗位不会因再次发现产生第二份工作。
-- outreach_status=pending 构成全局打招呼逻辑队列。
-- ============================================================
CREATE TABLE platform_jobs (
    -- SQLite 自动生成的本地主键。
    id INTEGER PRIMARY KEY,

    -- BOSS 返回的稳定岗位标识，例如 encryptJobId。
    -- 新标识始终表示新的平台岗位，即使同公司的 JD 判断内容与旧岗位完全相同。
    platform_job_id TEXT NOT NULL UNIQUE,

    -- 标准岗位详情 URL，例如 /job_detail/{encryptJobId}.html。
    canonical_url TEXT NOT NULL UNIQUE,

    -- 当前岗位名称，用于全局岗位列表展示。
    job_title TEXT NOT NULL,

    -- 当前公司名称。
    company_name TEXT,

    -- BOSS 当前展示的城市文本。
    city_text TEXT,

    -- BOSS 当前展示的薪资文本。
    salary_text TEXT,

    -- 最近一次从 BOSS 读取的完整结构化 JD。
    -- 新 JD 覆盖本字段。未打招呼岗位的旧 AI 结论随判断内容变化而清除；
    -- 旧人工结论继续用自己的哈希标明原始依据，已打招呼岗位则保留原 AI 与人工结论供查看。
    jd_json TEXT NOT NULL
        CHECK (json_valid(jd_json)),

    -- 当前“岗位判断内容”的稳定哈希，不是 jd_json 原始字节的哈希。
    -- 本字段只判断同一 platform_job_id 的 JD 是否变化，不能作为跨岗位身份或去重键。
    -- 输入包括 job_title、company_name、city_text、salary_text，以及 jd_json 中的职责和要求；
    -- 计算前采用固定字段顺序、规范化 JSON 表示、统一换行并去除无意义的首尾空白。
    -- 排除 JSON 对象键顺序、页面排版、抓取时间、展示元数据和招聘状态等非判断内容。
    jd_hash TEXT NOT NULL,

    -- -------------------- 平台岗位状态 --------------------

    -- BOSS 最近一次可靠证据是否允许对该岗位执行新的 AI 鉴定或首次打招呼。
    -- 状态变化不改变 jd_hash，也不删除已有 AI 或人工结论。
    -- 页面读取失败属于发现或打招呼操作错误，不产生第三种平台岗位状态。
    platform_status TEXT NOT NULL
        CHECK (
            platform_status IN (
                'open',    -- 最近一次可靠证据表明可沟通
                'closed'   -- BOSS 明确表明岗位已关闭或不再招聘
            )
        ),

    -- 已关闭时向求职者展示的 BOSS 证据或原因；可沟通时为空。
    platform_closed_reason TEXT,

    -- 最近一次取得可靠平台岗位状态证据的时间。
    -- 检查失败不会更新本字段，技术细节只写结构化运行日志。
    platform_status_checked_at INTEGER NOT NULL,

    -- -------------------- 当前 AI 鉴定 --------------------

    -- 当前 AI 鉴定状态；只有平台岗位可沟通且尚未确认已打招呼时，
    -- pending 才进入全局鉴定逻辑队列。
    -- 自动鉴定关闭时，新出现或失效的鉴定先停留在 not_queued；
    -- 已经是 pending/processing 的工作继续消费，不被开关撤回。
    -- 当前在线简历或策略后来变化，不会自动使已有成功结论失效；
    -- 结论继续保留实际采用的版本，直到新鉴定覆盖；未打招呼岗位的 JD 变化会清除旧结论，
    -- 已打招呼岗位则保留当时结论供查看。
    assessment_status TEXT NOT NULL DEFAULT 'not_queued'
        CHECK (
            assessment_status IN (
                'not_queued',               -- 缺少当前有效结论，但尚未加入鉴定队列
                'pending',                  -- 已加入鉴定队列，等待 AdviceService 领取
                'processing',               -- 已推送给 Pi，等待有效 MCP 回调
                'suitable',                 -- AI 建议适合
                'unsuitable',               -- AI 建议不适合
                'needs_user_confirmation',  -- AI 无法可靠判断
                'failed'                    -- 当前鉴定明确失败
            )
        ),

    -- 当前正在执行、最近失败或当前有效 AI 结论实际采用的在线简历版本。
    -- pending 只表示排队，因此仍为空；进入 processing 时才记录当前已保存版本。
    assessment_resume_version_id INTEGER,

    -- 当前正在执行、最近失败或当前有效 AI 结论实际采用的 JD 内容哈希。
    -- pending 岗位被领取为 processing 时才记录届时的当前 JD。
    assessment_jd_hash TEXT,

    -- 当前正在执行、最近失败或当前有效 AI 结论实际采用的策略版本。
    -- 发现和入队都不设置本字段；进入 processing 时记录届时启用的版本。
    assessment_policy_version_id INTEGER,

    -- 产生当前调用或当前结果的鉴定器版本。
    -- 更换鉴定器不会自动使已有成功结果失效。
    evaluator_version INTEGER
        CHECK (evaluator_version IS NULL OR evaluator_version > 0),

    -- 该平台岗位生命周期内已经向 Pi 推送过多少次。
    -- 本字段只递增、不归零；MCP 必须带回相同的 platform_jobs.id 和 attempt_no，
    -- 因而旧鉴定依据或旧尝试的迟到结果会被拒绝。
    assessment_attempt_no INTEGER NOT NULL DEFAULT 0
        CHECK (assessment_attempt_no >= 0),

    -- 当前鉴定依据下、无人干预执行周期内的连续失败次数。
    -- 自动重试不会归零；有效结果落库、鉴定依据变化或求职者显式重新开始后归零。
    assessment_consecutive_failure_count INTEGER NOT NULL DEFAULT 0
        CHECK (assessment_consecutive_failure_count >= 0),

    -- 当前 AI 结论的用户可见解释。
    assessment_reason TEXT,

    -- 当前 AI 结论的结构化证据。
    assessment_evidence_json TEXT
        CHECK (
            assessment_evidence_json IS NULL
            OR json_valid(assessment_evidence_json)
        ),

    -- 鉴定失败后最早允许自动重试的时间；为空表示必须人工处理或已达上限。
    -- v1 默认一个无人干预周期总共最多尝试三次（含首次），上限由程序配置。
    -- 到期后仍保持 failed，下一次领取直接按届时当前输入进入 processing；
    -- 输入未变化时保留连续失败次数，输入变化时开启新的失败周期。
    assessment_retry_at INTEGER,

    -- 当前有效 AI 结论形成的时间。
    assessed_at INTEGER,

    -- -------------------- 全局人工结论 --------------------

    -- 求职者最近一次认为该平台岗位是否适合自己。
    -- 这是全局唯一人工结论；再次复核只覆盖当前结论，不建立历史表。
    human_verdict TEXT
        CHECK (
            human_verdict IS NULL
            OR human_verdict IN ('suitable', 'unsuitable')
        ),

    -- 当前人工结论实际依据的 JD 内容哈希。
    -- 等于 jd_hash 时人工结论生效并优先于 AI；不等时保留展示但处于待复核状态。
    human_reviewed_jd_hash TEXT,

    -- 当前人工结论形成的时间。
    human_reviewed_at INTEGER,

    -- 当前人工结论的可选说明。
    human_review_note TEXT,

    -- -------------------- 全局首次打招呼 --------------------

    -- 平台岗位当前首次打招呼状态，同时构成全局打招呼逻辑队列。
    -- 正常打招呼取得 BOSS 成功证据后直接进入 contacted；possibly_contacted
    -- 只表示外部动作可能发生、但确认或本地落库窗口中断。
    outreach_status TEXT NOT NULL DEFAULT 'not_queued'
        CHECK (
            outreach_status IN (
                'not_queued',      -- 尚未请求打招呼
                'pending',         -- 等待 PostService 领取
                'processing',      -- 正在操作 BOSS
                'contacted',       -- 已确认真实打招呼，以后不再重复打招呼
                'simulated',       -- 本轮模拟完成；仍可重新以 real 模式入队
                'possibly_contacted', -- 可能已打招呼，对账前禁止重试
                'failed'              -- 明确没有打招呼成功
            )
        ),

    -- 当前或最近一轮已入队动作采用 simulation 还是真实 real 模式。
    -- 它是本轮执行方式，不是岗位的永久结果；在入队时冻结，不能中途切换。
    -- simulated 岗位以后可以重新以 real 模式入队，并用 real 覆盖本字段。
    outreach_mode TEXT
        CHECK (
            outreach_mode IS NULL
            OR outreach_mode IN ('simulation', 'real')
        ),

    -- 本次已入队或已执行动作使用的固定招呼语。
    -- 在入队时冻结，保证恢复后招呼内容不变。
    outreach_greeting_text TEXT,

    -- 该平台岗位生命周期内已经开始过多少次首次打招呼尝试；只递增、不归零。
    outreach_attempt_no INTEGER NOT NULL DEFAULT 0
        CHECK (outreach_attempt_no >= 0),

    -- 当前无人干预执行周期内，能够确认没有打招呼成功的连续失败次数。
    -- 自动重试不会归零；确认成功、模拟完成或求职者显式重新开始后归零。
    -- possibly_contacted 尚未确认失败，不在对账得出 failed 前递增本字段。
    outreach_consecutive_failure_count INTEGER NOT NULL DEFAULT 0
        CHECK (outreach_consecutive_failure_count >= 0),

    -- 最近一次开始操作 BOSS 的时间。
    outreach_last_attempt_at INTEGER,

    -- 明确打招呼失败后最早允许自动重试的时间；为空表示必须人工处理或已达上限。
    -- v1 默认一个无人干预周期总共最多尝试三次（含首次），上限由程序配置。
    -- 调度器到时将 failed 改回 pending 并清空本字段，但不清零连续失败次数。
    outreach_retry_at INTEGER,

    -- 最近一次用于确认成功、失败或可能已打招呼的页面证据。
    outreach_evidence_json TEXT
        CHECK (
            outreach_evidence_json IS NULL
            OR json_valid(outreach_evidence_json)
        ),

    -- contacted 状态的来源。
    -- agent 表示本程序确认打招呼成功；boss_existing 表示 BOSS 原本已沟通。
    contact_source TEXT
        CHECK (
            contact_source IS NULL
            OR contact_source IN ('agent', 'boss_existing')
        ),

    -- 确认岗位已打招呼的时间。
    contacted_at INTEGER,

    -- -------------------- 两个全局队列共用的领取控制 --------------------

    -- 当前租约属于 assessment 还是 outreach；未被领取时为空。
    lease_stage TEXT
        CHECK (
            lease_stage IS NULL
            OR lease_stage IN ('assessment', 'outreach')
        ),

    -- 当前领取该平台岗位的 Worker。
    lease_owner TEXT,

    -- 当前租约到期时间。
    lease_until INTEGER,

    -- 第一次在任意岗位发现运行中看到该岗位的时间。
    first_seen_at INTEGER NOT NULL,

    -- 最近一次从 BOSS 搜索结果或详情中看到该岗位的时间。
    -- 新运行再次发现同一岗位时更新本字段；若 JD 判断内容未变化，
    -- 不因此改变已有鉴定状态或要求使用新简历版本再次进入鉴定。
    last_seen_at INTEGER NOT NULL,

    -- 本行最后更新时间。
    updated_at INTEGER NOT NULL,

    FOREIGN KEY (assessment_resume_version_id)
        REFERENCES online_resume_versions(id)
        ON DELETE RESTRICT,

    FOREIGN KEY (assessment_policy_version_id)
        REFERENCES assessment_policy_versions(id)
        ON DELETE RESTRICT,

    -- 可沟通状态不能遗留关闭原因；已关闭必须解释原因。
    CHECK (
        (
            platform_status = 'open'
            AND platform_closed_reason IS NULL
        )
        OR
        (
            platform_status = 'closed'
            AND platform_closed_reason IS NOT NULL
        )
    ),

    -- 未打招呼岗位的成功 AI 结论必须对应当前 JD；已打招呼岗位不再参与鉴定或打招呼，
    -- 因此 JD 后来变化时允许保留当时结论及其旧哈希供查看。
    -- 所有成功结论都必须说明实际采用的版本、理由和时间。
    CHECK (
        assessment_status NOT IN (
            'suitable',
            'unsuitable',
            'needs_user_confirmation'
        )
        OR (
            (
                assessment_jd_hash = jd_hash
                OR outreach_status = 'contacted'
            )
            AND evaluator_version IS NOT NULL
            AND assessment_reason IS NOT NULL
            AND assessed_at IS NOT NULL
        )
    ),

    -- 未入队或待鉴定表示没有当前 AI 结论，因此不能残留旧结论或旧依据；
    -- 一旦开始执行、失败或形成结论，在线简历版本、JD 哈希和策略版本必须同时存在。
    CHECK (
        (
            assessment_status IN ('not_queued', 'pending')
            AND assessment_resume_version_id IS NULL
            AND assessment_jd_hash IS NULL
            AND assessment_policy_version_id IS NULL
            AND evaluator_version IS NULL
            AND assessment_reason IS NULL
            AND assessment_evidence_json IS NULL
            AND assessment_retry_at IS NULL
            AND assessed_at IS NULL
        )
        OR
        (
            assessment_status NOT IN ('not_queued', 'pending')
            AND assessment_resume_version_id IS NOT NULL
            AND assessment_jd_hash IS NOT NULL
            AND assessment_policy_version_id IS NOT NULL
        )
    ),

    -- 正在处理或已失败的鉴定也必须说明实际使用的鉴定器版本。
    CHECK (
        assessment_status NOT IN ('processing', 'failed')
        OR evaluator_version IS NOT NULL
    ),

    -- 连续鉴定失败次数不能超过生命周期尝试数；failed 至少对应一次失败。
    CHECK (
        assessment_consecutive_failure_count <= assessment_attempt_no
        AND (
            assessment_status <> 'failed'
            OR assessment_consecutive_failure_count > 0
        )
    ),

    -- 只有明确鉴定失败可以等待自动重试；其他状态不能遗留重试时间。
    CHECK (
        assessment_status = 'failed'
        OR assessment_retry_at IS NULL
    ),

    -- 人工结论、所依据的 JD 哈希和复核时间必须同时存在或同时为空；
    -- 无结论时不能留说明。哈希可以与当前 jd_hash 不同，用来表达“人工结论待复核”。
    CHECK (
        (
            human_verdict IS NULL
            AND human_reviewed_jd_hash IS NULL
            AND human_reviewed_at IS NULL
            AND human_review_note IS NULL
        )
        OR
        (
            human_verdict IS NOT NULL
            AND human_reviewed_jd_hash IS NOT NULL
            AND human_reviewed_at IS NOT NULL
        )
    ),

    -- 进入待打招呼时，平台岗位必须明确可沟通且当前判断适合。
    -- 人工结论存在但 JD 哈希不一致时，不回退使用 AI 结论，必须等待重新复核。
    -- processing 可能已经触发外部动作，不受此静态约束；PostService 在实际打招呼前重查，
    -- 如果动作已经发生则只能继续记录 contacted、possibly_contacted 或明确失败。
    CHECK (
        outreach_status <> 'pending'
        OR (
            platform_status = 'open'
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
        )
    ),

    -- 已排队或已由本程序执行的动作必须冻结模式和招呼语。
    CHECK (
        outreach_status NOT IN (
            'pending',
            'processing',
            'simulated',
            'possibly_contacted',
            'failed'
        )
        OR (
            outreach_mode IS NOT NULL
            AND outreach_greeting_text IS NOT NULL
        )
    ),

    -- simulated 只是 simulation 模式这一轮的完成结果，不代表真实打招呼成功。
    -- 它之后仍可重新变为 pending，并在真实入队时把模式冻结为 real。
    CHECK (
        outreach_status <> 'simulated'
        OR outreach_mode = 'simulation'
    ),

    -- 只有真实外部动作才可能出现“可能已打招呼”；模拟不会产生这个状态。
    CHECK (
        outreach_status <> 'possibly_contacted'
        OR outreach_mode = 'real'
    ),

    -- 明确打招呼失败或可能已打招呼都必须记录尝试时间；技术错误只写运行日志。
    -- failed 可以按规则重试，possibly_contacted 必须先对账。
    CHECK (
        outreach_status NOT IN ('failed', 'possibly_contacted')
        OR outreach_last_attempt_at IS NOT NULL
    ),

    -- 连续打招呼失败次数不能超过生命周期尝试数；failed 至少对应一次明确失败。
    CHECK (
        outreach_consecutive_failure_count <= outreach_attempt_no
        AND (
            outreach_status <> 'failed'
            OR outreach_consecutive_failure_count > 0
        )
    ),

    -- 只有明确打招呼失败可以等待自动重试；可能已打招呼永远不能设置重试时间。
    CHECK (
        outreach_status = 'failed'
        OR outreach_retry_at IS NULL
    ),

    -- 只有正在处理的阶段允许保存完整租约。
    CHECK (
        (
            lease_stage IS NULL
            AND lease_owner IS NULL
            AND lease_until IS NULL
        )
        OR
        (
            lease_stage = 'assessment'
            AND assessment_status = 'processing'
            AND lease_owner IS NOT NULL
            AND lease_until IS NOT NULL
        )
        OR
        (
            lease_stage = 'outreach'
            AND outreach_status = 'processing'
            AND lease_owner IS NOT NULL
            AND lease_until IS NOT NULL
        )
    ),

    -- 已确认打招呼必须说明时间、来源和 BOSS 页面证据。
    -- 本程序产生的打招呼只能来自 real 模式；BOSS 原本已沟通时没有本轮执行模式。
    CHECK (
        outreach_status <> 'contacted'
        OR (
            contacted_at IS NOT NULL
            AND contact_source IS NOT NULL
            AND outreach_evidence_json IS NOT NULL
            AND (
                (
                    contact_source = 'agent'
                    AND outreach_mode = 'real'
                )
                OR (
                    contact_source = 'boss_existing'
                    AND outreach_mode IS NULL
                )
            )
        )
    )
);

-- AdviceService 的全局鉴定逻辑队列；已关闭或已确认打招呼的岗位不会被领取。
CREATE INDEX idx_platform_jobs_assessment_queue
    ON platform_jobs(
        platform_status,
        outreach_status,
        assessment_status,
        assessment_retry_at,
        first_seen_at
    );

-- PostService 的全局打招呼逻辑队列及可能已打招呼对账查询。
CREATE INDEX idx_platform_jobs_outreach_queue
    ON platform_jobs(outreach_status, outreach_retry_at, first_seen_at);

-- 全局岗位列表按人工结论、AI 结论和打招呼状态筛选。
CREATE INDEX idx_platform_jobs_review
    ON platform_jobs(human_verdict, assessment_status, outreach_status);


-- ============================================================
-- 5. 自动化设置
--
-- 整个本地个人实例只有一行，固定使用 id=1。
-- 它保存后台重启后仍要恢复的当前运行设置，不保存岗位状态或发现运行状态。
-- 已经入队岗位采用的 outreach_mode 和招呼语仍冻结在 platform_jobs。
-- ============================================================
CREATE TABLE automation_settings (
    -- 单行配置的固定主键；应用初始化时插入 id=1，此后只更新该行。
    id INTEGER PRIMARY KEY
        CHECK (id = 1),

    -- 是否把新出现、缺少有效结论的岗位自动加入鉴定队列；首次初始化默认关闭。
    -- 关闭只停止新增自动入队，不撤回已有 pending 或 processing。
    automatic_assessment_enabled INTEGER NOT NULL DEFAULT 0
        CHECK (automatic_assessment_enabled IN (0, 1)),

    -- 同时最多允许多少个平台岗位处于 assessment_status=processing。
    -- v1 首次初始化默认 5，但 5 不是配置上限；任何正整数都可以保存。
    -- AdviceService 每次领取数量仍不得超过当前配置留下的空闲名额。
    assessment_processing_limit INTEGER NOT NULL DEFAULT 5
        CHECK (assessment_processing_limit > 0),

    -- 是否把新的合适岗位自动加入首次打招呼队列；首次初始化默认关闭。
    -- 关闭只停止新增自动入队，不撤回已有 pending 或 processing。
    automatic_outreach_enabled INTEGER NOT NULL DEFAULT 0
        CHECK (automatic_outreach_enabled IN (0, 1)),

    -- 自动入队的新岗位采用 simulation 还是真实 real 模式；首次初始化默认 simulation。
    -- 只影响之后的新入队；已经入队岗位继续使用自己冻结的 outreach_mode。
    automatic_outreach_mode TEXT NOT NULL DEFAULT 'simulation'
        CHECK (automatic_outreach_mode IN ('simulation', 'real')),

    -- 以后新加入首次打招呼队列的岗位使用的当前固定招呼语。
    -- 修改本字段只影响之后入队的岗位；入队时实际内容复制到
    -- platform_jobs.outreach_greeting_text，之后不再跟随本字段变化。
    -- 为空表示尚未配置，因此不能开启自动打招呼，
    -- 也不能手工加入模拟队列或真实打招呼队列。
    outreach_greeting_text TEXT
        CHECK (
            outreach_greeting_text IS NULL
            OR length(trim(outreach_greeting_text)) > 0
        ),

    -- 允许开始真实首次打招呼的每日时间窗，按 Asia/Shanghai 解释。
    -- 结构示例：[{"start":"10:00","end":"12:00"},{"start":"14:00","end":"16:00"}]。
    -- 设置模块负责校验 HH:MM、同日 start < end、排序且互不重叠。
    -- 首次初始化为空数组，表示不限制打招呼时间，任何时间都允许开始真实打招呼。
    outreach_time_windows_json TEXT NOT NULL DEFAULT '[]'
        CHECK (
            json_valid(outreach_time_windows_json)
            AND json_type(outreach_time_windows_json) = 'array'
        ),

    -- 最近一次修改任一自动化设置的时间。
    updated_at INTEGER NOT NULL,

    -- 无论 simulation 还是真实 real，自动入队前都必须知道原本要发送什么招呼语。
    CHECK (
        automatic_outreach_enabled = 0
        OR outreach_greeting_text IS NOT NULL
    )
);
