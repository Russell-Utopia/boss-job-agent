# SQLite 数据访问与 migration 技术栈研究

## 结论

本项目不应引入 GORM、Ent 或 Bun 作为主数据访问层。推荐组合是：

```text
modernc.org/sqlite
        ↓
database/sql + sqlc 生成的静态查询
        ↓
调用这些查询的业务模块（platform_jobs 的写入只在 JobPool）

版本化 SQL migration（DDL 权威）
        ↓
goose Provider + 同一个 *sql.DB + embed.FS
```

- **DDL 权威**：把当前五表 DDL 迁到不可变的 `00001_initial.sql`，后续每次变更新增顺序 migration。migration 目录是唯一 schema 权威；不要再让 Go struct、`AutoMigrate` 或 `docs` 包生成数据库结构。
- **查询权威**：业务状态迁移继续显式写 SQL。sqlc 只把已评审 SQL 生成带参数和结果类型的 Go 方法，不替代 `JobPool` 的业务接口，也不是 ORM。
- **事务边界**：`JobPool` 通过 sqlc 生成的 `WithTx`/`DBTX` 在短 `sql.Tx` 中组合查询；Pi 和浏览器调用仍必须在事务外。Go 官方文档明确说明 `sql.Tx` 将一组操作作为一次原子变更提交，并警告事务中不要混用事务外 `sql.DB` 方法。[Go transactions](https://go.dev/doc/database/execute-transactions)
- **逃生口**：sqlc SQLite 支持当前仍标记为 Beta，且内建分析器不能理解所有复杂查询。因此约定“sqlc 能解析的静态查询默认用 sqlc；解析不了的少数查询留在所属模块内手写 `database/sql`，并用真实 SQLite 集成测试覆盖”，而不是为了生成器改写正确 SQL。[sqlc language support](https://docs.sqlc.dev/en/stable/reference/language-support.html)；[sqlc generate](https://docs.sqlc.dev/en/latest/howto/generate.html)

如果仓库继续使用 Go 1.23，应先固定 `sqlc v1.30.0` 和 `goose v3.26.0`：两者的 `go.mod` 都声明 Go 1.23；当前最新版 `sqlc v1.31.1` 和 `goose v3.27.3` 分别要求 Go 1.26 与 Go 1.25.7，不能被当前本机 `GOTOOLCHAIN=local` 的 Go 1.23 直接运行。[sqlc v1.30.0 go.mod](https://github.com/sqlc-dev/sqlc/blob/v1.30.0/go.mod)；[sqlc v1.31.1 go.mod](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/go.mod)；[goose v3.26.0 go.mod](https://github.com/pressly/goose/blob/v3.26.0/go.mod)；[goose v3.27.3 go.mod](https://github.com/pressly/goose/blob/v3.27.3/go.mod)

## 当前仓库真正需要解决的问题

当前数据库不是普通 CRUD：

- [五表 DDL](../sqlite-schema.sql) 有 JSON `CHECK`、跨字段状态约束、partial unique index 和 Trigger；
- [JobPool 决策](../adr/0018-centralize-platform-job-transitions-in-job-pool.md)规定 `platform_jobs` 的所有写入只能通过业务意图推进，不能暴露通用 Repository 或字段 setter；
- 鉴定和沟通采用 `Claim → 事务外 Execute → Finish`，领取与完成都依赖状态、尝试号和租约的条件更新；
- 当前 [SQLite 启动代码](../../internal/sqlite/sqlite.go)把 DDL 从 `docs` 包嵌入，使用 `PRAGMA user_version`，只处理空库 `0 → 1`，遇到任何其他版本直接失败；DDL 自己也明确写着“不是已经投入使用的 migration”。

因此选型标准依次是：

1. 能否让现有 SQL DDL 继续做权威；
2. 能否原样表达并测试原子条件更新；
3. 能否减少参数、列顺序和 `Scan` 类型错误；
4. 是否继续复用当前 pure-Go `modernc.org/sqlite`；
5. 引入的生成代码、运行时机制和第二份 schema 是否值得。

## 真实 JobPool 条件更新样例

下面是从 `pending` 原子领取一个鉴定工作的代表性 SQL。资格条件与状态写入在同一条语句中；如果页面状态已过期、岗位已沟通或已被其他 Worker 领取，`RETURNING` 不返回行，模块把 `sql.ErrNoRows` 转成业务上的“本次未领取”。SQLite 官方说明 `RETURNING` 只为实际被 `INSERT`、`UPDATE` 或 `DELETE` 的行返回结果。[SQLite RETURNING](https://www.sqlite.org/lang_returning.html)

```sql
-- name: ClaimAssessment :one
UPDATE platform_jobs
SET
    assessment_status = 'processing',
    assessment_resume_version_id = CAST(sqlc.arg(resume_version_id) AS INTEGER),
    assessment_jd_hash = jd_hash,
    assessment_policy_version_id = CAST(sqlc.arg(policy_version_id) AS INTEGER),
    evaluator_version = CAST(sqlc.arg(evaluator_version) AS INTEGER),
    assessment_attempt_no = assessment_attempt_no + 1,
    assessment_last_error = NULL,
    assessment_retry_at = NULL,
    lease_stage = 'assessment',
    lease_owner = CAST(sqlc.arg(worker_owner) AS TEXT),
    lease_until = CAST(sqlc.arg(lease_until) AS INTEGER),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(job_id)
  AND platform_status = 'open'
  AND outreach_status <> 'contacted'
  AND assessment_status = 'pending'
  AND lease_stage IS NULL
RETURNING
    id,
    platform_job_id,
    jd_json,
    jd_hash,
    assessment_attempt_no,
    lease_until;
```

`CAST` 不是运行时校验的替代品；它在这里还向 sqlc 明确表示这些调用参数不可空。因为数据库列在其他状态允许 `NULL`，不写 `CAST` 时，实测 sqlc 会把这些必填领取参数生成为 `sql.NullInt64`/`sql.NullString`。生成包仍应被 `JobPool` 包住：Web、Worker 和其他模块只看 `JobPool.ClaimAssessments` 的业务输入输出，不能直接拿 `sqlcgen.Queries`。

实测 `sqlc v1.30.0` 能解析完整当前 DDL（包括 JSON `CHECK`、partial index 和 Trigger）与这条 SQLite `UPDATE ... RETURNING`，生成：

- 7 个非空 Go 参数，不再手排 `ExecContext` 参数；
- 一个类型化结果行和固定 `Scan` 顺序；
- `*sql.DB` 与 `*sql.Tx` 都满足的 `DBTX` 接口；
- 当前五表模型、DBTX 支持和这一条查询共 219 行生成代码。

使用完整 DDL、真实 `modernc.org/sqlite` 内存库的测试中，第一次领取成功且尝试号变为 1；第二次领取同一行返回 `sql.ErrNoRows`。这说明它能表达本项目的关键写法，但不能说明所有未来批量 CTE 都一定可解析，所以仍需保留上述逃生口。

## 数据访问候选比较

| 方案 | 对当前 DDL/条件更新的适配 | 类型安全与复杂度 | 结论 |
| --- | --- | --- | --- |
| `database/sql` | 完全保留现有 SQL、Trigger、`CHECK` 和短事务；Go 标准库直接支持 `Exec`、`Query`、`sql.Tx`。[Go transactions](https://go.dev/doc/database/execute-transactions) | 无生成和额外运行时；但 SQL 语法、参数个数、列顺序和 `Scan` 类型主要到测试/运行时才暴露。 | 保留为底座和复杂查询逃生口，不建议所有查询继续纯手写。 |
| **sqlc** | schema 与查询仍是 SQL；能直接从 goose migration 目录解析 Up 方向，自己不执行 migration。[sqlc DDL/migrations](https://docs.sqlc.dev/en/stable/howto/ddl.html) | 生成静态参数/结果类型，没有 ORM 运行时；本次 5 表 + 1 查询生成量可控。SQLite 支持为 Beta，复杂查询分析有限。 | **推荐作为默认查询层**，必须固定版本并在 CI 执行 `sqlc generate` 后检查无 diff、再跑真实 SQLite 测试。 |
| GORM | 条件更新、`RowsAffected` 和 raw `UPDATE ... RETURNING` 都能表达。[GORM Update](https://gorm.io/docs/update.html)；[GORM Raw SQL](https://gorm.io/docs/sql_builder.html) | struct/tag、hooks、约定和运行时 query builder 增加第二套模型；复杂状态迁移最终仍会落回 raw SQL。`AutoMigrate` 会主动创建/修改表且不会删除废弃列，不适合作为精确 DDL 权威。[GORM migration](https://gorm.io/docs/migration.html) | 不采用。官方 SQLite 路径基于 CGO `mattn/go-sqlite3`，也会偏离当前 pure-Go driver。[GORM SQLite](https://gorm.io/docs/connecting_to_the_database.html) |
| Ent | 生成 builder、实体和 predicate，支持 `CHECK` 与 partial index；复杂 SQL可自定义 predicate。[Ent codegen](https://entgo.io/docs/code-gen/)；[Ent checks](https://entgo.io/docs/faq/)；[Ent indexes](https://entgo.io/docs/schema-indexes/) | 必须再维护一套 Go schema 并生成较大 API；Trigger 的组合 schema 路径依赖 Atlas，官方 Trigger 指南标注为 Atlas Pro 功能。[Ent triggers](https://entgo.io/he/docs/migration/triggers) | 不采用。它适合以实体/关系 schema 为中心的系统，不适合这套手工 SQL 状态机和小型五表模型。 |
| Bun | 建在 `database/sql` 上，SQL-first，能包现有连接；SQLite shim支持 modernc。[Bun queries](https://bun.uptrace.dev/guide/queries.html)；[Bun SQLite](https://bun.uptrace.dev/guide/drivers.html) | struct/tag + 运行时 builder；字符串条件和扫描仍主要由测试发现，无法提供 sqlc 的静态查询签名。硬查询继续写 raw SQL时收益有限。 | 不采用主数据层；当前没有足够的动态查询需求证明这层运行时抽象值得。 |

这里的“类型安全”只指 Go 方法签名、参数和结果扫描，不代表 sqlc 能证明业务状态转换正确。真正的正确性仍来自：DDL 约束、`WHERE` 资格条件、短事务、受影响行/`RETURNING` 判断，以及用同一份 migration 跑的 SQLite 集成测试。

## migration 候选比较

### 推荐：goose + 顺序 SQL migration

goose 的 Provider 接受调用方已有的 `*sql.DB`，明确不关心具体 driver，并从 `fs.FS`（包括 `embed.FS`）读取 migration；这与当前单二进制和 `modernc.org/sqlite` 完全匹配。[goose Provider](https://pressly.github.io/goose/documentation/provider/) migration 文件支持顺序号、Up/Down 和默认事务；包含 Trigger `BEGIN ... END` 这类内部分号的语句要使用 `StatementBegin`/`StatementEnd`。[goose annotations](https://pressly.github.io/goose/documentation/annotations/)

建议目录：

```text
internal/storage/sqlite/
  migrations/
    00001_initial.sql        # 当前五表完整、带字段注释的 DDL
    00002_<change>.sql       # 以后只追加，不修改已经发布的 migration
  migrations.go             # go:embed migrations/*.sql；启动 Worker 前 provider.Up
  open.go                    # modernc DSN、连接数、PRAGMA 验证
  sqlcgen/                   # 生成代码，不承载业务规则
sqlc.yaml                    # schema 指向 migrations 目录
```

`goose_db_version` 是 migration 工具的技术元数据表，不是第六张业务表。五表业务模型仍不增加业务实体。

### 1 → 2 样例

为了只验证 migration 机制而不偷渡新的业务字段，本次原型用一个**仅作实验、不主张直接合入产品**的 partial index 作为 v2：

```sql
-- +goose Up
CREATE INDEX idx_platform_jobs_outreach_reconcile
    ON platform_jobs(outreach_status, updated_at)
    WHERE outreach_status = 'possibly_contacted';

-- +goose Down
DROP INDEX idx_platform_jobs_outreach_reconcile;
```

使用 `goose v3.26.0`、当前完整五表 DDL、真实 `modernc.org/sqlite v1.38.2` 和内存数据库实测：

1. `UpByOne` 建立 v1；
2. 写入一条 v1 在线简历数据；
3. `UpByOne` 完成 v1 → v2，原数据仍在且新 index 存在；
4. `Down` 回到 v1，原数据仍在且 index 已删除。

测试通过。把当前 DDL 放入首个 goose migration 时，需要用 `StatementBegin`/`StatementEnd` 包住 Trigger（或整个 Up 段），否则 goose 会把 Trigger 内部的分号误切为多条语句。

每个正式 migration 至少应在 CI 验证：

- 空库全部 Up；
- 从上一版本带代表性数据 Up；
- `PRAGMA foreign_key_check` 无结果；
- 五表 `CHECK`、Trigger、partial index 仍存在；
- 可逆变更执行 Down → Up；不可安全回滚的数据变更可以省略 Down，但必须在文件中说明原因和恢复方式。

SQLite 明确规定 `PRAGMA foreign_keys` 在事务内设置是 no-op，所以不能依赖 migration 文件开头的 `PRAGMA foreign_keys = ON`；应继续在连接 DSN/打开阶段为连接启用并在 migration 后验证。[SQLite PRAGMA](https://www.sqlite.org/pragma.html)

### 其他 migration 方案为何不选

- **继续自制 `PRAGMA user_version`**：SQLite 把它定义为留给应用随意使用的整数，SQLite 自己不解释。[SQLite PRAGMA user_version](https://www.sqlite.org/pragma.html) 当前实现只有 `0 → 1`；继续扩展等于自己补做顺序发现、状态记录、嵌入、Up/Down、错误上下文和测试工具。规模已经越过“比依赖更简单”的界线。
- **golang-migrate**：它有清晰的 Up/Down 与 dirty-state 恢复模型，[官方 FAQ](https://github.com/golang-migrate/migrate/blob/master/FAQ.md)很成熟；但官方 SQLite3 driver 源码直接导入 CGO 的 `mattn/go-sqlite3`，[sqlite3 driver source](https://github.com/golang-migrate/migrate/blob/master/database/sqlite3/sqlite3.go)与本项目 `modernc` 选择冲突。goose Provider 可直接复用现有 `*sql.DB`，因此更贴合。
- **Atlas**：能从 SQL desired state 为 SQLite 生成 diff，versioned migration 还有 checksum 与 lint，能力最强。[Atlas SQLite](https://atlasgo.io/getting-started/sqlite-declarative-sql)；[Atlas versioned migrations](https://entgo.io/docs/versioned-migrations/) 但本项目只有五张手工约束表，当前需求是可靠执行已评审 SQL，不是再引入 dev database、diff 计划和第二套 schema 工作流。未来 migration 数量和多人并发显著增长时可重新评估 Atlas lint。
- **GORM/Ent AutoMigration、Bun migrate**：会把 migration 生命周期绑定到相应 ORM；既然主数据访问层不采用它们，就没有单独引入的理由。GORM 文档也建议复杂阶段转向 versioned migrations。[GORM migration](https://gorm.io/docs/migration.html)

## 落地约束

后续实施应一次只迁一个模块，并保持每一步可编译、测试通过：

1. 先把当前五表 DDL原样转成 `00001_initial.sql`，用 goose 替换自制 `user_version`，验证现有数据库的基线/迁移策略；
2. 引入固定版本的 sqlc 配置与生成检查；
3. 先迁移 `JobPool` 的一个 Claim/Finish 垂直切片，证明事务、迟到结果拒绝和状态冲突映射；
4. 再迁移其他模块查询；
5. 删除 `internal/application` 中已经迁走的 SQL，最终让 `docs` 只保存说明，不被生产代码导入。

必须保持以下边界：

- `sqlcgen` 只提供数据库形状，不提供 `UpdateJob` 或业务 Repository；
- `platform_jobs` 写查询只能由 `JobPool` 调用；
- Web 不导入 `sqlcgen`、不持有 `*sql.DB`；
- 生成 struct 不是领域实体，不从业务 API 返回；
- migration 只在后台 Worker 启动前执行，不与 BOSS/Pi 外部动作并发；
- 所有 migration 与 JobPool 写查询使用真实 SQLite 和同一 migration 目录测试，不 mock 数据库。

## 研究范围与版本

研究日期：2026-08-29。仓库基线：`origin/main` 的 `6784527`。比较依据只使用当前仓库事实以及 Go、SQLite、sqlc、GORM、Ent、Bun、goose、golang-migrate、Atlas 的官方文档/官方源码；没有把博客排名或二手基准作为决策依据。
