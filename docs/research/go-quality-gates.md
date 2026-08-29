# Go 质量门禁研究

研究日期：2026-08-29  
仓库基线：`6784527`  
对应工单：[研究 Go 静态检查、复杂度与安全门禁](https://github.com/Russell-Utopia/boss-job-agent/issues/23)

## 结论

本仓库应建立一套本地 `make check` 与 CI 共用、失败即阻止合并的质量门禁，但落地顺序必须先处理工具链安全：

1. 将 Go 从已停止维护的 `1.23` 升级到 `1.27.0`。Go 官方只维护最近两个大版本；2026-08-29 的受支持版本是 1.27 和 1.26。[Go Release History](https://go.dev/doc/devel/release)
2. 固定 `golangci-lint v2.13.2` 和 `govulncheck v1.7.0`，不使用会随版本变化的隐式 linter 集合。
3. `gofmt`、`go vet`、测试、覆盖率、Race Detector、`govulncheck` 和精选的 golangci-lint 检查全部作为阻断门禁；依赖“有新版本”只产生更新 PR，不直接判失败。
4. 圈复杂度上限用 `cyclop=10`，认知复杂度上限用 `gocognit=20`，可维护性指数下限用 `maintidx=20`。这是工具官方默认值或推荐区间，不是任意数字；现存三个复杂测试函数需要拆分后再启用全仓门禁。
5. 覆盖率采用 `go test -coverpkg=./...` 的全模块语句覆盖率，初始硬下限设为基线下方的 `60.0%`，之后只上调、不下调；关键状态转换仍必须由行为测试覆盖，不能用总百分比替代。

## 当前基线

测量在隔离 worktree 中完成，没有修改主工作区。复杂度使用 `golangci-lint v2.13.2`；漏洞检查使用 `govulncheck v1.7.0`。

| 检查 | 当前结果 | 说明 |
| --- | --- | --- |
| `gofmt -l .` | 0 个文件 | 当前格式一致 |
| `go vet ./...` | 通过 | Go 1.23.3 |
| `go test ./...` | 通过 | 普通测试约 1.7 秒（冷启动测量） |
| `go test -race ./...` | 通过 | 约 3.0 秒（冷启动测量） |
| 普通覆盖率 | 53.6% | `go test -coverprofile=... ./...` |
| 全模块覆盖率 | 60.9% | Go 1.27.0，`-coverpkg=./...`；用于门禁基线 |
| 圈复杂度 | 生产代码最高 10；测试最高 14 | 生产最高是 `sqlite.migrate` |
| 认知复杂度 | 生产代码最高 9；测试最高 30 | 生产最高是 `sqlite.migrate` |
| 可维护性指数 | 生产代码最低 39；测试最低 36 | 越高越好；当前没有低于 20 的函数 |
| 精选 golangci-lint | 30 条 | 12 条 `errcheck`、12 条 `noctx`、3 条复杂度、3 条 `gosec`；同一行去重后计数 |
| `go mod tidy -diff` | 通过 | `go.mod` / `go.sum` 当前整洁 |
| 依赖更新 | 直接依赖 `modernc.org/sqlite v1.38.2` 可更新到 `v1.57.0` | 只说明有新版，不能跳过测试直接升级 |

复杂度阈值按全仓运行时，当前需要拆分：

- `TestFirstStartupRestoresSafeDefaults`：圈复杂度 14；
- `TestFirstUseWebProvidesFourStableEntriesAndSafeState`：圈复杂度 12、认知复杂度 30；
- `TestWebRestoresSavedPolicyAndAutomationSettings`：圈复杂度 11。

这三个都是断言和分支过多的测试函数。应提取场景和断言 helper，而不是永久排除全部 `_test.go`；测试也是后续修改者需要读懂的代码。

### 工具链安全基线

仓库 `go.mod` 声明 `go 1.23.0`，本机实际是 Go 1.23.3。Go 官方的维护政策意味着它已经不再接收安全修复。[Go Release History](https://go.dev/doc/devel/release)

`govulncheck ./...` 在 Go 1.23.3 下报告 **31 个可达的标准库漏洞**，调用路径包含本仓库实际使用的 `html/template`、`net/http` 和 `database/sql`。同一提交不改代码，改用 Go 1.27.0 后：

- `go test ./...` 通过；
- `go test -race ./...` 通过；
- `govulncheck ./...` 报告 0 个可达漏洞；仍有 1 个仅存在于所需模块、但调用图不可达的漏洞。

因此首要修复是升级并固定 Go 工具链，不是为旧 Go 版本加忽略规则。`govulncheck` 会根据调用图筛出实际可达的已知漏洞，比仅按依赖版本报警噪声更低，但反射和 `unsafe` 仍可能造成漏报。[Go Vulnerability Management](https://go.dev/doc/security/vuln/)

## 每项检查解决什么问题

### Go 官方工具

- `gofmt -l`：验证唯一的标准 Go 排版；`-l` 只列出不一致文件，适合只读门禁。`go fmt` 和 `gofmt -w` 会改写文件，应作为开发命令而不是 CI 检查。[gofmt](https://pkg.go.dev/cmd/gofmt)
- `go vet ./...`：发现编译器不会拒绝、但高度可疑的结构。它是启发式分析，不证明程序正确。[go vet](https://pkg.go.dev/cmd/vet)
- `go test ./...`：验证可执行行为。
- `go test -race ./...`：运行时检测并发读写冲突，只能发现测试实际走到的路径；典型代价是 5–10 倍内存和 2–20 倍耗时，适合 CI 独立 job，但本仓库当前仅约 3 秒，应继续阻断。[Race Detector](https://go.dev/doc/articles/race_detector)
- 覆盖率：统计执行过的语句，不是分支覆盖率，也不代表断言正确。`-coverpkg=./...` 可让跨包测试覆盖主模块全部包。[Coverage](https://go.dev/doc/build-cover)
- `go mod tidy -diff`：只读验证模块声明是否需要整理；`go mod verify` 验证下载缓存未被下载后篡改。二者都不检查依赖是否过期或是否有漏洞。[Go Modules Reference](https://go.dev/ref/mod#go-mod-tidy)
- `govulncheck ./...`：阻断调用图可达的已知 Go/依赖漏洞。[govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck)

`go vet` 应显式运行；golangci-lint 中不要再启用 `govet`，避免同一批分析重复执行。格式检查也应直接使用随固定 Go 工具链发布的 `gofmt`，不再通过 golangci-lint formatter 重复执行。

### 精选 golangci-lint 检查器

golangci-lint 是检查器运行器，不是一种复杂度算法。配置应使用 `version: "2"`、`linters.default: none` 并逐项启用；官方警告 `default: all` 会在工具新增或升级 linter 时让构建突然改变。[golangci-lint configuration](https://golangci-lint.run/docs/configuration/file/)

推荐启用：

| 类别 | 检查器 | 解决的问题 |
| --- | --- | --- |
| 正确性 | `errcheck`、`errorlint` | 未检查错误；错误包装和比较错误 |
| 正确性 | `staticcheck`、`ineffassign`、`unused` | 已知错误模式、无效赋值、未使用声明 |
| 资源 | `bodyclose`、`rowserrcheck`、`sqlclosecheck` | HTTP body、SQL rows/stmt 的释放和迭代错误 |
| 取消传播 | `noctx` | 网络与 SQL 调用没有使用 `Context` 版本 |
| 安全 | `gosec` | 文件权限、弱加密、危险转换等源码安全模式 |
| 抑制治理 | `nolintlint` | 无效或缺少理由的 `//nolint` |
| 复杂度 | `cyclop`、`gocognit`、`maintidx` | 路径数、阅读难度、综合可维护性下限 |

当前 30 条候选规则问题不是让规则失效的理由，而是启用门禁前的明确清理清单。需要局部判断的典型项：

- 清理失败路径中的 `Close` / `Rollback` 常常是 best-effort；确实不能覆盖主错误时，用带原因的局部 `//nolint:errcheck`，不能全局排除所有 `Close`。
- `gosec G107` 对测试中的本地动态 URL 是误报，可以只按规则和 `_test.go` 路径排除；`G301` 指出 SQLite 数据目录目前使用 `0755`，对个人简历和沟通状态应收紧到 `0700`，不是误报。
- `noctx` 在测试中也有价值：升级到 Go 1.27 后可使用 `t.Context()`，SQL 使用 `QueryContext` / `ExecContext`，HTTP 测试显式传递请求上下文。

禁止使用永久的“只检查新代码”模式掩盖旧问题。迁移可以分步清理，但架构迁移完成时全仓必须绿色。

## 三种复杂度指标如何配合

- `cyclop` 计算函数和包的圈复杂度，近似独立控制流路径数量；官方默认函数上限是 10。[cyclop](https://golangci-lint.run/docs/linters/configuration/#cyclop)
- `gocognit` 衡量人理解控制流的困难，尤其惩罚嵌套；官方默认报警值是 30，但官方文档建议采用 10–20，本仓库取宽端 20。[gocognit](https://golangci-lint.run/docs/linters/configuration/#gocognit)
- `maintidx` 综合圈复杂度、Halstead volume 和代码行数；官方默认在指数低于 20 时报警。其作者明确称该指数是实验值，因此只适合作为极端大函数的低水位保险，不能用来替代前两项。[maintidx](https://github.com/yagipy/maintidx)

三者有意重叠但回答不同问题：路径是否过多、嵌套是否难读、函数总体是否已经巨大。不要开启 `cyclop.package-average`：小包平均值会被单个合法编排函数明显拉动，而且问题不如函数级报告容易行动。

建议配置核心如下：

```yaml
version: "2"

linters:
  default: none # 规则必须显式、可审计，升级工具不会自动增加规则
  enable:
    - bodyclose
    - cyclop
    - errcheck
    - errorlint
    - gocognit
    - gosec
    - ineffassign
    - maintidx
    - noctx
    - nolintlint
    - rowserrcheck
    - sqlclosecheck
    - staticcheck
    - unused
  settings:
    cyclop:
      max-complexity: 10 # 官方默认；当前生产代码最高为 10
      package-average: 0.0 # 关闭包平均值检查
    gocognit:
      min-complexity: 20 # 官方建议区间 10–20 的宽端
    maintidx:
      under: 20 # 官方默认；指数越高越易维护
```

## 门禁分层与命令

开发者可运行小目标快速反馈，但合并前必须运行完整 `make check`；CI 执行同样的命令集合，只是可以拆成并行 job。

```text
快速本地反馈
  make fmt-check    -> gofmt -l
  make lint         -> go vet + golangci-lint
  make test         -> 普通测试

本地与 CI 的完整阻断门禁
  make check
    -> gofmt -l
    -> go mod tidy -diff
    -> go mod verify
    -> go vet ./...
    -> golangci-lint config verify
    -> golangci-lint run
    -> go test ./...
    -> go test -coverpkg=./...，总语句覆盖率 >= 60.0%
    -> go test -race ./...
    -> govulncheck ./...

低频维护，不直接作为“存在新版即失败”的门禁
  每周 Dependabot 检查 Go Modules，更新 PR 通过完整 make check
  每月检查受支持 Go 版本、golangci-lint 与 govulncheck 新版本
  需要人工盘点时运行 go list -m -u all
```

`govulncheck` 普通文本模式发现漏洞会返回非零；JSON、SARIF 和 OpenVEX 输出即使发现漏洞也可能成功，因此阻断 job 必须使用普通退出状态，结构化报告只能作为附加产物。[govulncheck command](https://github.com/golang/vuln/blob/master/cmd/govulncheck/doc.go)

## 版本固定与依赖更新

- Go：CI 与开发环境固定 `1.27.0`，仓库通过 `go` / `toolchain` 声明表达基线；更新小版本仍需 PR 和完整门禁。
- golangci-lint：固定官方 release `v2.13.2`，下载官方二进制并校验 release checksum；不要使用 `latest`，也不要让 CI 与本地使用不同版本。该版本从 v2.13 起支持 Go 1.27。[golangci-lint changelog](https://golangci-lint.run/docs/product/changelog/)
- govulncheck：固定工具版本 `v1.7.0`，用固定 Go 工具链构建；漏洞数据库保持在线更新，因此相同提交将随新披露漏洞而合理地变红。
- Go 1.24 起支持 `tool` 指令。可用仓库内工具声明固定 Go 工具版本，但 golangci-lint 仍优先使用官方 release 二进制，避免其构建工具链与被检查代码支持范围错位。[Go Modules Reference](https://go.dev/ref/mod#go-tool)
- Dependabot：添加 `package-ecosystem: gomod`、根目录 `/`、每周检查；只创建更新 PR，不自动合并。若后续添加 GitHub Actions，再增加独立的 `github-actions` 条目。所有更新必须经过同一套门禁。[Dependabot version updates](https://docs.github.com/en/code-security/concepts/supply-chain-security/dependabot-version-updates)

当前 `modernc.org/sqlite` 跨越多个版本可升级，不应在本研究工单直接改动；由独立依赖更新 PR 阅读 release notes、执行数据库迁移/恢复测试后决定。

## 实施验收条件

后续质量门禁实施完成时应满足：

1. Go 1.27.0 下 `make check` 与 CI 使用完全相同的版本和规则并全部通过；
2. `.golangci.yml` 校验通过，规则显式列出，没有无理由的全局排除；
3. 当前 30 条 lint 问题逐项修复或用局部、有原因的抑制处理；
4. 全模块语句覆盖率不低于 60.0%，关键的 `JobPool` 状态转换、首次沟通去重和失败恢复另有行为测试；
5. `govulncheck` 无可达漏洞；
6. Dependabot 每周提出 Go 模块更新，更新不自动合并。
