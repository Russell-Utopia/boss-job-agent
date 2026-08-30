# 按业务深模块组织并在 app 装配 Go 应用

BOSS Job Agent 的长期 Go 代码按 `onlineresume`、`discovery`、`jobpool`、`assessment`、`outreach` 和 `automationsettings` 六个业务深模块组织；不采用横向 `domain`/`application`/`infrastructure` 分层、共享业务模型或 Repository。`internal/app` 是唯一生产 composition root，只负责初始化顺序与进程生命周期；Web 直接读取各模块 View，并把每个写请求交给唯一业务所有者，三个执行模块分别拥有自己的 Worker、重试和外部 Adapter 生命周期。

每个有 SQLite 状态的业务模块把 sqlc 查询与生成代码放在自己的 `internal/sqlitedb`，共享由 `internal/sqlite` 完成 migration 后的一个 `*sql.DB`；`internal/sqlite/migrations` 是唯一可执行 DDL 权威。生产 Adapter 位于 `internal/adapters/boss` 和 `internal/adapters/pi`，只实现调用模块拥有的外部接口。详细目录、允许的 import 方向、状态所有权、装配顺序和迁移切片见 [Go 工程架构](../architecture.md)。

当前未投入使用的 `internal/application` 不形成兼容合同：模块切换时由 Web 直接改用业务模块，并在同一切片删除它与 `internal/advice`。架构门禁只机械检查长期稳定的禁止依赖；有意改变架构时必须同时更新决策与检查，但不使用完整 import 白名单冻结未来设计。
