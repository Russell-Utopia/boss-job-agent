# 统一本地与 CI 的 Go 质量门禁

BOSS Job Agent 使用一个由仓库拥有的 `make check` interface 约束全部长期维护的 Go 应用 Module 和测试；本地完整检查与 CI 必须执行同一组强制门禁，CI 可以把共享 Make target 拆成并行 job，但不能复制或删减规则。MVP 固定 Go 1.27.0，并在后续实施中一次清理全部存量问题后启用全量门禁，不保留旧代码基线、仅检查新代码或长期 warning-only 模式。

## 强制门禁

开发过程中可以运行相关单测、`make test` 和 `make lint` 取得快速反馈；每张实施票准备交付或请求评审前运行完整 `make check`，远端 CI 再执行同一集合，任何一项失败都阻止合并。

| 检查 | 强制行为 | 阻止的实际问题 |
| --- | --- | --- |
| 格式 | 检查 `gofmt -l` 没有输出 | 非标准格式和无意义格式 diff |
| Module 整洁 | `go mod tidy -diff` | 源码使用与 `go.mod`、`go.sum` 声明不一致 |
| Module 缓存 | `go mod verify` | 已下载 Module 内容在本地缓存中被修改 |
| 官方静态分析 | `go vet ./...` | 编译器不拒绝但高度可疑的代码结构 |
| lint 配置 | `golangci-lint config verify` | 配置无效导致预期规则没有真正运行 |
| 精选 lint | `golangci-lint run` | 吞错、错误包装、资源泄漏、缺少取消传播和危险源码模式 |
| 普通测试 | `go test -count=1 ./...` | 已由测试表达的行为发生回归 |
| 覆盖率 | 手写应用代码的全 Module 语句覆盖率不得低于 60.0% | 大片手写代码完全没有测试触达 |
| Race Detector | `go test -count=1 -race ./...` | 测试实际走到的路径存在并发读写冲突 |
| 可达漏洞 | 普通阻断模式运行 `govulncheck ./...` | 调用图可达的已知 Go 或依赖漏洞 |
| 生成一致性 | 用固定 sqlc 版本从当前 migration 和查询重新生成，结果必须与已提交生成文件一致 | SQL 已变化但生成 Go 文件遗漏、被手改或由不同生成器产生 |

覆盖率百分比不能替代关键状态转换、首次沟通去重、失败恢复和真实 SQLite 约束的行为测试。初始下限只允许通过评审后的变更上调，不能用降低阈值修复失败。

## golangci-lint 规则与阈值

配置使用 v2 格式、`linters.default: none`，显式启用：

- 错误处理：`errcheck`、`errorlint`；
- 常见缺陷：`staticcheck`、`ineffassign`、`unused`；
- 资源生命周期：`bodyclose`、`rowserrcheck`、`sqlclosecheck`；
- 取消传播：`noctx`；
- 源码安全：`gosec`；
- 抑制治理：`nolintlint`；
- 复杂度：`cyclop`、`gocognit`、`maintidx`。

`go vet` 由固定 Go 工具链直接运行，不在 golangci-lint 中重复启用。函数圈复杂度最大允许 10，认知复杂度达到 20 即失败，可维护性指数低于 20 即失败；这些阈值同时约束生产代码和测试代码。不开启包平均复杂度检查，因为具体函数报告更容易行动，也不会让小包被一个合法编排函数扭曲。

## 维护范围与例外

可丢弃原型必须与长期应用的根 Go Module 结构性隔离，不进入应用的 lint、复杂度、覆盖率、Race Detector 或漏洞门禁。sqlc 生成文件提交到 Git，但不属于人工维护代码，不进入 golangci-lint、复杂度和覆盖率百分比分母；它们仍必须由固定生成器重新生成一致、正常编译，并通过所属 SQLite Adapter 使用真实 migration 的集成测试。修改 SQLite schema 或查询时，应把 SQL 与随之变化的生成 Go 文件一起提交；“生成一致”不限制 SQL 变化，只防止输入和输出脱节。

静态检查告警默认通过修改代码解决。只有逐项确认属于误报或无法合理处理时，才允许在最小代码位置使用带具体检查器名称和原因的局部 `//nolint`；禁止裸 `//nolint`、全局关闭已选规则、排除全部测试文件或把现存目录整体移出检查。生成文件使用标准生成标记统一识别，不逐行添加抑制。

## 工具与漏洞数据

初始版本固定为 Go 1.27.0、golangci-lint v2.13.2 和 govulncheck v1.7.0。本地和 CI 从仓库中的同一声明取得版本：`go.mod` 声明 Go 与 toolchain，`make check` 验证实际 Go 版本；govulncheck 使用固定的 Go `tool` 声明；golangci-lint 使用固定官方 release 和 checksum，安装到项目工具缓存而不是依赖全局 PATH。GitHub Actions 固定到完整 commit SHA，禁止使用 `latest`。ADR-0021 已固定的 sqlc 与 goose 版本继续由其决策管理。

govulncheck 的扫描器版本固定，但漏洞数据库保持在线更新：调用图可达漏洞阻断，不可达漏洞只报告；网络、数据库或扫描失败表示本次检查没有完成，不能当成通过。Race Detector 与 govulncheck 都进入每次完整本地检查和每次 CI，不降为每日或每周检查。

MVP 不配置普通版本的定期自动更新，也不允许自动合并升级。出现可达漏洞、Go 工具链临近停维或维护者主动维护时，使用独立 PR 更新固定版本并运行完整门禁；存在新版本身不构成失败。以后更新具体工具版本不改变本 ADR 的门禁范围、失败语义和全量全绿原则。

## 后续影响

本 ADR 只确定规则，不实施 Makefile、CI、Go 升级或存量代码清理。父架构地图在模块目录和剩余产品边界确定后安排实施切片；无论拆成一张还是多张前置清理工单，最终启用全部强制门禁时，整个约定范围必须一次达到全绿。
