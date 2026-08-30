# “厘清模拟发送的用户目的与 MVP 去留”交接

## 状态

- 决策票：[厘清模拟发送的用户目的与 MVP 去留](https://github.com/Russell-Utopia/boss-job-agent/issues/28) 已发布 [resolution](https://github.com/Russell-Utopia/boss-job-agent/issues/28#issuecomment-5466300207) 并关闭。
- 父地图：[厘清并迁移 BOSS Job Agent 的 Go 工程架构](https://github.com/Russell-Utopia/boss-job-agent/issues/17) 已追加决策指针。
- 分支：`issue-28`；决策、权威文档和安全壳删改提交为 `fc964f5`，已推送到 `origin/issue-28`。
- 验证：`make check` 通过；本次没有连接 BOSS、调用 Pi 或发送消息。

## 已确认结论

“模拟打招呼”原本解决未稳定编排误触真实外部动作的工程验证问题，不是求职者需要完成并据此作出产品内决定的任务。MVP 因此完整删除 Simulation：不保留 `simulation` 执行模式、`simulated` 岗位状态、模拟队列、模拟命令、自动模式选择、Simulation 到 Real 切换或工作台入口。

产品中的打招呼只表示真实首次沟通。自动真实打招呼默认关闭，开启前展示当前可入队岗位数、完整固定招呼语和时间规则；手工真实打招呼对选中批次展示同样信息。两者都必须明确确认，并继续遵守资格重校验、动作前 BOSS 检查、时间窗、永久去重和 `possibly_contacted` 对账。

测试分为三层：

1. 默认 `go test ./...` 与 CI 使用真实业务模块、真实内存 SQLite 和受控外部 Adapter，完全离线。
2. 每个生产 Adapter 由显式本地 live integration test 连接真实外部数据源，验证自己的接口合同。
3. 接口稳定后运行全部生产 Adapter 组成的完整真实链路。真实 `SendFirstContact` 每次最多自动选择一个当前合格岗位，记录目标、招呼语、证据和 trace，并证明再次运行不会重复打招呼；执行该显式命令就是本次人工授权。

## 仓库改动

- `CONTEXT.md`、`docs/application-modules.md`、`docs/web-mvp.md` 和 `docs/sqlite-schema.sql` 已删除产品模式、状态和入口，并写入测试边界。
- ADR-0007、ADR-0009 和 ADR-0019 已与真实沟通单模式对齐；ADR-0024 记录删除 Simulation 的理由与后果。
- 当前安全 Web 壳已删除模拟路由、应用命令、自动化模式字段和按钮；回归测试证明旧路由为 404，页面不再展示 Simulation。
- `docs/sqlite-schema.sql` 当前仍是评审 DDL，正式 goose `00001_initial.sql` 及安全迁移由后续架构迁移落实。本次没有为尚未发布的旧开发数据库增加正式 migration。
- 已被 ADR-0016 取代的 ADR-0006 和 throwaway 原型保留历史文字，不作为当前实现依据。

## 工单同步

- 父规格与“启动本地 Web MVP 并恢复安全默认状态”“手工真实发送并永久避免重复沟通”“自动真实首次沟通与持续授权”“完成岗位工作台的筛选、分页和批量处理”“验证后台重启恢复与完整 MVP 安全边界”均已按新边界重写。
- [手工模拟首次沟通](https://github.com/Russell-Utopia/boss-job-agent/issues/11#issuecomment-5466298761) 已作为超出 MVP 范围关闭。
- [确定模块目录、依赖方向与应用装配](https://github.com/Russell-Utopia/boss-job-agent/issues/22) 的所有原生阻塞项均已关闭，它现在是父地图唯一开放、未认领的子工单。

## 后续保护线

- 不要把受控 Adapter、fixture、live 配置或测试结果重新建模成 Web 功能、自动化设置或 `platform_jobs` 业务状态。
- 默认测试和 CI 不得依赖 BOSS 登录、真实 Pi 或外部副作用；未来取得 API Key 时仍需重新评估成本、限额、凭证安全和副作用。
- 在生产 Adapter 和强门禁 live test 尚未实现前，不得把离线测试通过描述为已经验证真实 BOSS/Pi 链路。
