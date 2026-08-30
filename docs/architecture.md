# Go 工程架构

本文是 BOSS Job Agent 最终 Go 包结构、依赖方向和应用装配的入口。业务语义与接口合同继续由 [v1 后台模块与接口](./application-modules.md) 约束，SQLite 状态模型由各模块使用的顺序 migration 表达；本文描述的是目标结构，不表示当前原型已经完成迁移。

## 设计原则

- 按业务能力组织深模块，不建立横向的 `domain`、`application`、`infrastructure`、Repository 或公共业务模型层。
- 每个模块在包根暴露一个主要类型：`onlineresume.Versions`、`discovery.Service`、`jobpool.Pool`、`assessment.Service`、`outreach.Service` 和 `automationsettings.Settings`。调用方通过各包的 `New(...)` 构造实例，再调用实例方法。
- `internal/app` 只负责生产装配和进程生命周期，不提供业务查询、命令或流程编排接口。岗位发现、岗位鉴定和打招呼分别拥有自己的循环、Worker、重试和恢复。
- 外部接口由调用它的业务模块定义；生产 Adapter 适配这些接口，但单次调用不循环重试、不休眠，也不拥有业务状态。
- 模块拥有自己的输入、返回值和 SQLite 查询类型。不得建立 `common`、`utils`、共享 `domain/models` 或跨模块 Repository 来消除依赖；发生循环时应重新塑造调用关系。

## 目标目录

```text
cmd/boss-job-agent/
  main.go
  serve.go
  logs.go

internal/
  app/
    config.go
    assemble.go
    run.go

  sqlite/
    open.go
    paths.go
    upgrade.go
    backup.go
    validate.go
    migrations/
      00001_initial.sql

  runlog/
    log.go
    health.go
    recheck.go
    find.go
    paths.go

  onlineresume/
    versions.go
    model.go
    view.go
    internal/sqlitedb/
      queries.sql
      <sqlc 生成文件>

  discovery/
    service.go
    worker.go
    model.go
    view.go
    internal/sqlitedb/
      queries.sql
      <sqlc 生成文件>

  jobpool/
    pool.go
    model.go
    view.go
    assessment.go
    outreach.go
    internal/sqlitedb/
      queries.sql
      <sqlc 生成文件>

  assessment/
    service.go
    worker.go
    policy.go
    model.go
    view.go
    paths.go
    internal/sqlitedb/
      queries.sql
      <sqlc 生成文件>

  outreach/
    service.go
    worker.go
    model.go

  automationsettings/
    settings.go
    model.go
    view.go
    internal/sqlitedb/
      queries.sql
      <sqlc 生成文件>

  adapters/
    boss/
      online_resume.go
      job_discovery.go
      outreach.go
    pi/
      assessment.go
      policy.go

  webui/
    webui.go
    dependencies.go
    jobs.go
    assessments.go
    outreach.go
    resume.go
    health.go
    templates/
      layout.gohtml
      jobs.gohtml
      assessments.gohtml
      outreach.gohtml
      resume.gohtml
    assets/
      app.css
      app.js

sqlc.yaml
```

测试与所属包放在一起。只有相应业务行为进入实施切片时才创建包和 Adapter，不预先提交空壳。前端属于 `internal/webui`，不建立 React、TypeScript、Vite、Node 或第二个前端工程；浏览器脚本只管理批量选择、页面内策略候选稿与验收报告、短暂提示和健康状态刷新。

## 依赖方向

允许的生产 import 方向如下：

```text
cmd/boss-job-agent -> app
cmd/boss-job-agent -> runlog                 // 仅 logs find

app -> sqlite、runlog、webui、全部业务模块、adapters/boss、adapters/pi

webui -> onlineresume、discovery、jobpool、assessment、automationsettings、runlog

onlineresume -> runlog
discovery -> onlineresume、jobpool、runlog
automationsettings -> jobpool
assessment -> onlineresume、jobpool、automationsettings、runlog
outreach -> jobpool、automationsettings、runlog

adapters/boss -> onlineresume、discovery、outreach
adapters/pi -> assessment
```

以下边界是长期禁令：

- `jobpool` 不 import 其他业务模块；`sqlite` 和 `runlog` 不 import 业务模块。
- 业务模块不 import `app`、`webui` 或生产 Adapter；涉及数据访问时，只 import 自己的 `internal/sqlitedb`，不能越过边界使用其他模块的 sqlc 包。
- `webui` 不 import `app`、`sqlite`、生产 Adapter 或 `outreach`，不直接执行 SQL。
- 生产代码不 import `docs`，不建立 `common`、`utils`、共享业务模型或通用 Repository 包。
- 当前允许边的完整集合只作为设计和评审依据。仓库拥有的轻量架构检查只执行上述稳定禁令；未来有意调整架构时，在同一变更中更新架构决策和相应检查，而不是让完整白名单冻结设计。

## 应用装配与生命周期

`cmd/boss-job-agent` 只解析 `serve` 或 `logs find`、开发测试覆盖项及进程信号。生产服务统一进入 `app.Run(ctx, config)`；`logs find` 只打开 `runlog` 的只读检索能力，不打开 SQLite、HTTP、业务模块、Worker 或生产 Adapter。

`internal/app` 是唯一生产 composition root。构造关系采用普通 Go 构造函数和具体根类型，不使用 DI 框架、Service Locator 或全局单例：

```go
pool := jobpool.New(db) // *jobpool.Pool
settings := automationsettings.New(db, pool) // *automationsettings.Settings
resumeVersions := onlineresume.New(db, resumeAdapter, logs) // *onlineresume.Versions
discoveryService := discovery.New(db, resumeVersions, pool, discoveryAdapter, logs) // *discovery.Service
assessmentService := assessment.New(db, resumeVersions, pool, settings, assessmentAdapter, policyAdapter, logs) // *assessment.Service
outreachService := outreach.New(pool, settings, outreachAdapter, logs) // *outreach.Service

handler := webui.New(webui.Dependencies{
    Resume: resumeVersions,
    Discovery: discoveryService,
    Jobs: pool,
    Assessment: assessmentService,
    Settings: settings,
    Runlog: logs,
})
```

上例说明包、根类型和实例调用形式，不固定构造函数的参数排列。业务调用使用 `pool.Review(...)`、`discoveryService.Start(...)` 这样的实例方法，不把 `包.类型.方法` 写成运行时调用。

启动顺序固定为：

1. 初始化 runlog；不可写时建立降级健康状态，并继续尝试提供 Web 恢复入口。
2. 打开 SQLite，执行备份、候选库 migration、验证和替换；失败则不启动 HTTP 或 Worker。
3. 由 `assessment` 和 `automationsettings` 幂等初始化默认策略与安全设置；失败则拒绝启动。
4. 创建业务模块及其专属生产 Adapter。
5. 先绑定 HTTP listener；绑定失败时不启动 Worker。
6. runlog 健康时启动岗位发现、岗位鉴定和打招呼三个模块；runlog 降级时只启动 Web 和日志恢复检查，不发起新的外部尝试。
7. 启动 HTTP Server。

停止时先拒绝新的 HTTP 请求并让在途请求收尾，再停止三个模块领取新工作并等待已经开始的外部动作安全记录结果；模块关闭自己的 Worker 与专属 Adapter，`app` 最后关闭 runlog 和 SQLite。

runlog 降级后由后台周期自动重新检查，求职者也可以在 Web 中立即触发一次检查。程序自动修复能够安全修复的目录或文件问题；清理或隔离现有日志必须先在 Web 中取得确认。普通求职者不需要输入 `chmod`、`rm`、`df` 等终端命令，也不依赖重启应用才能恢复。SQLite 健康时 Web 保持可用，并清楚显示新外部动作仍被关闭。

## Web 调用边界

`webui` 是独立的 HTTP 表现 Adapter，不属于应用装配或业务流程编排。一个 GET 可以组合多个模块的只读 View；一个写请求只能调用一个拥有该业务意图的模块：

| Web 操作 | 调用 |
| --- | --- |
| 岗位页 | `discovery` 与 `jobpool` 的只读 View |
| 开始或恢复发现 | `discovery.Service` 的实例方法 |
| 人工复核、加入鉴定队列 | `jobpool.Pool` 的实例方法 |
| 手工加入真实打招呼队列 | `automationsettings.Settings` 校验当前招呼语和时间窗后调用 `jobpool.Pool` |
| 刷新在线简历 | `onlineresume.Versions` 的实例方法 |
| 策略优化与采用 | `assessment.Service` 的实例方法 |
| 自动化设置 | `automationsettings.Settings` 的实例方法 |
| 日志健康与立即重查 | `runlog` 的健康接口 |

Web 不调用 `outreach.Service`；后者只消费已经授权并由 `jobpool.Pool` 持久化的打招呼工作。`internal/app` 也不转发这些调用或协调页面动作。

## 状态所有权

| 所有者 | 持久状态 | 说明 |
| --- | --- | --- |
| `onlineresume.Versions` | `online_resume_versions` | 仅响应求职者显式刷新写入；每次 BOSS 读取都写 runlog，但不建立业务尝试表 |
| `discovery.Service` | `discovery_runs` | 只推进发现运行；岗位观察通过 `jobpool.Pool` 提交 |
| `jobpool.Pool` | `platform_jobs` | 该表唯一写入者，统一维护鉴定、人工复核和打招呼状态机 |
| `assessment.Service` | `assessment_policy_versions`、受管 Pi 临时标记 | 鉴定结果只能通过 `jobpool.Pool` 落到平台岗位 |
| `automationsettings.Settings` | `automation_settings` | 校验设置与手工真实打招呼授权；不拥有岗位状态 |
| `outreach.Service` | 无独立业务表 | 通过 `jobpool.Pool` 领取并完成打招呼或对账工作 |
| `runlog` | 轮转 JSONL 和日志健康状态 | 技术错误、trace 和外部尝试；不是业务历史表 |
| `sqlite` | migration 元数据、候选库和升级前备份 | 不直接写任何业务行 |
| `webui` | 仅浏览器页面会话状态 | 策略候选稿、生成样本选择和验收报告不持久化 |
| `app` | 无 | 只装配并管理生命周期 |

## SQLite 与 sqlc

```text
sqlc.yaml
internal/sqlite/migrations/00001_initial.sql
internal/onlineresume/internal/sqlitedb/{queries.sql,<generated>.go}
internal/discovery/internal/sqlitedb/{queries.sql,<generated>.go}
internal/jobpool/internal/sqlitedb/{queries.sql,<generated>.go}
internal/assessment/internal/sqlitedb/{queries.sql,<generated>.go}
internal/automationsettings/internal/sqlitedb/{queries.sql,<generated>.go}
```

`internal/sqlite/migrations` 是唯一可执行 DDL 权威。goose 记录 migration 技术版本；sqlc 从 migration 的 Up DDL 与各模块私有查询生成代码，生成文件提交 Git。所有模块共享 migration 完成后的一个 `*sql.DB`，直接使用 `database/sql` 和自己的 sqlc 包；不引入数据库接口或 Repository。`outreach` 没有 SQL 包，因为平台岗位状态由 `jobpool` 持有。

正式升级先验证磁盘空间和升级前备份，再从健康快照创建候选库，在候选库上执行全部待执行 migration，并验证目标版本、完整性、外键和关键数据；成功后关闭连接、同步文件和目录并在同一文件系统原子替换，最后重新打开验证。升级失败不修改正式库或唯一备份。默认 migration 使用事务且只支持向前升级。

完成 migration 后再由状态所有者幂等写入默认策略和安全自动化设置。最终删除 `docs/schema.go` 和重复保存完整 DDL 的 `docs/sqlite-schema.sql`；业务文档只解释表语义和所有者并链接 migration。

## 本地持久化路径

正常求职者不配置路径。Mac 默认路径由各所有者模块自己解析，不建立通用 `localpaths` 或文件系统包；只有开发和测试可以覆盖：

```text
~/Library/Application Support/boss-job-agent/
  data/boss-job-agent.db
  backups/
  run/

~/Library/Logs/boss-job-agent/
  boss-job-agent.jsonl
```

`sqlite` 拥有数据库、候选库和备份路径，`runlog` 拥有日志路径，`assessment` 拥有受管 Pi 标记路径。当前工作目录下的 `./var` 开发数据库没有正式用户数据，不建立兼容迁移或继续作为默认位置。

## 渐进迁移切片

每个切片独立保持应用可启动，并通过当时完整的 `make check`：

| 切片 | 变更 | 独立验证 |
| --- | --- | --- |
| 1. 质量门禁基础 | 固定 Go 与工具版本，落地 lint、复杂度、覆盖率、Race Detector、漏洞、sqlc 生成一致性及少量稳定架构禁令；可丢弃 Web 对比原型继续与根 Go Module 隔离 | 当前原型在本地和 CI 运行同一个 `make check`，原型目录不进入长期应用门禁 |
| 2. SQLite 权威切换 | 建立 goose migration、升级前备份、候选库验证与 sqlc 配置；把生产 DDL 从 `docs/schema.go` 移到 `00001_initial.sql`，删除两个旧 schema 文件；Web 业务行为保持不变 | 新库、重复启动、带代表性数据的升级、备份失败、候选 migration 失败和替换后重开均由真实 SQLite 集成测试覆盖，失败场景不修改正式库 |
| 3. 直接完成模块切换 | 把现有查询、默认初始化和命令一次分配给 `onlineresume`、`discovery`、`jobpool`、`assessment` 和 `automationsettings`；加入 `internal/app`，让 `webui` 直接持有模块依赖，并在同一切片删除 `internal/application` 与 `internal/advice` | 现有 Web HTTP 场景改由新模块配真实内存 SQLite 通过；生产 import 中不存在 `internal/application`、`internal/advice` 或 `docs` |
| 4. runlog 与恢复门禁 | 加入持久化日志、健康状态、后台自动复查、Web 立即复查和外部动作 fail-closed | 覆盖不可写路径、可自动修复、需确认清理、修复后自动恢复，以及 SQLite 正常时 Web 持续可用且不开始新外部动作 |
| 5. 外部能力垂直接入 | 依次实现在线简历刷新、岗位发现、岗位鉴定与策略优化、首次打招呼；每个行为与所属 Adapter 同切片进入 | 每个子切片用受控 Adapter 和真实 SQLite 离线验证业务状态与调用次数；生产 Adapter 另有显式 `live` 测试，默认测试和 CI 不连接 BOSS、Pi 或真实发送 |

第三个切片是一次直接替换，不把 `internal/application` 改造成临时代理，也不保留其测试接口。后续垂直切片只在真实行为进入时创建对应包和 seam，避免为未来假设预建空结构。
