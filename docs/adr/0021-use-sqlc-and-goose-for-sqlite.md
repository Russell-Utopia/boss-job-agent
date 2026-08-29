# 使用 sqlc 与 goose 管理 SQLite

BOSS Job Agent 的 SQLite DDL 包含 Trigger、JSON `CHECK`、partial index 和跨字段状态约束，`JobPool` 又必须以业务意图原子推进平台岗位状态。项目保留 pure-Go 的 `modernc.org/sqlite` 和标准库 `database/sql`，采用 sqlc 生成已评审静态查询的参数与结果类型，并采用 goose 执行顺序 SQL migration；这能继续直接表达 SQL 状态机，又避免引入 ORM 的第二套 schema 和运行时模型。

sqlc 是默认查询生成器，但生成包只是所属业务模块的内部实现，不是 Repository、领域实体或提供给 Web/Worker 的接口。生成代码提交到 Git，CI 重新生成并检查无 diff；只有 sqlc 无法解析的正确 SQL 才允许在所属模块内直接使用 `database/sql`，且必须由使用同一 migration 的真实 SQLite 集成测试覆盖。初始工具版本固定为 sqlc v1.31.1 和 goose v3.27.3，并随 Go 1.27 工具链落地；以后升级工具版本不改变本 ADR 的组合。

`JobPool` 仍是 `platform_jobs` 的唯一写入模块。单个状态迁移使用带资格条件的 `UPDATE ... WHERE ... RETURNING`，无返回行表示状态已经变化或本次没有取得工作；需要组合多个读写时，通过 sqlc 的事务绑定在短 `sql.Tx` 中完成。Pi 和 BOSS 调用永远在事务外，沿用 Claim → Execute → Finish：Claim 写入 `processing`、尝试号与租约，Finish 以岗位 ID、尝试号和当前状态拒绝迟到结果。首次沟通的已确认发送、已确认无外部影响和可能已有外部影响三态沿用 ADR-0018 及相关外部 Adapter 决议，不由数据访问层重新解释。

SQLite storage adapter 所拥有的顺序 migration 目录是唯一可执行 DDL 权威；`docs/schema.go` 和完整文档 DDL 不再作为生产 schema 来源。应用在 HTTP 和三个 Worker 启动前同步执行 migration，失败则拒绝启动业务流程；每份 SQL migration 默认在事务中执行，不使用 `NO TRANSACTION` 绕过原子性，除非未来针对无法事务化的具体变更另作决策。当前没有真实用户数据、只用自制 `PRAGMA user_version=1` 的开发数据库可以丢弃，当前确认的最终 DDL 直接成为新的 `00001_initial.sql`；正式采用 goose 后只追加 migration，并只支持向前升级，不承诺让旧版应用继续使用已经升级的数据库。

正式数据库和升级备份必须位于应用安装目录之外，普通卸载不删除它们；只有求职者明确执行“删除全部本地数据”才允许移除。已有正式数据库存在待执行 schema migration 时，先通过 SQLite 一致性备份能力生成并验证一份不可修改的升级前备份，且至少保留最近一次升级前备份；备份失败时不得创建候选库或修改正式库。然后从健康快照生成候选数据库，在候选库上执行全部待执行 migration，并验证目标版本、完整性、外键和关键数据。候选库失败时正式库与备份保持不变；候选库成功后，关闭所有 SQLite 连接、完成文件与目录同步，再在正式库同一文件系统内原子替换正式数据库。不得先删除正式库，也不得直接迁移唯一备份。修复版本或重新安装后的应用先检查正式库，只有正式库无法继续使用时才从最近一次健康备份复制新的候选库重试。

## 未采用的方案

- 不把全部查询继续纯手写在 `database/sql` 上，因为参数、列顺序和扫描类型错误只能较晚暴露。
- 不采用 GORM、Ent 或 Bun 作为主数据访问层，因为复杂状态迁移仍要回到原始 SQL，同时会增加 struct/schema、生成 API 或运行时 query builder。
- 不继续扩展自制 `PRAGMA user_version`，因为它缺少顺序 migration 的发现、执行记录、错误上下文和验证工具。
- 不采用 golang-migrate，因为其官方 SQLite 路径与当前 pure-Go driver 选择不如 goose Provider 直接复用 `*sql.DB` 合适。
- 不采用 Atlas，因为当前五表模型需要可靠执行已评审 SQL，而不需要 desired-state diff、开发数据库和额外 schema 工作流。
- 不把 Down migration 当作产品能力；升级失败和新版故障通过未改动的正式库、升级前备份以及后续修复版本恢复，而不是让用户日常降级数据库。

## 后续影响

`goose_db_version` 是 migration 技术元数据，不是第六张业务表。实现迁移时需要移除生产代码对 `docs/schema.go` 的依赖、把 sqlc schema 指向 migration 的 Up 方向、为备份和候选库预检查磁盘空间，并用带代表性旧数据的数据库验证每次向前升级。具体包目录、装配顺序和迁移切片由后续架构实施工单决定。
