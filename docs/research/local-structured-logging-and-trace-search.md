# 本地结构化日志与 trace 检索研究

研究日期：2026-08-30

仓库基线：`issue-26` 的 `5a80d66a58be3ad7f5885015d328209282f6b5c9`

对应工单：[确定本地持久化结构化日志与 trace 检索方案](https://github.com/Russell-Utopia/boss-job-agent/issues/26)

## 结论

v1 采用 **Go 标准库 `log/slog.JSONHandler` + 固定版本 `gopkg.in/natefinch/lumberjack.v2 v2.2.1`**：

```text
SearchService / AdviceService / PostService
                 ↓ 只提交规范化的尝试事件
        具体 runlog 模块（无公共 Go interface）
          ├─ trace ID 与字段约束
          ├─ error tree 快照与脱敏
          ├─ slog.JSONHandler（JSONL）
          ├─ 写入错误上报与健康状态
          └─ lumberjack（同进程轮转）
                 ↓
       boss-job-agent*.jsonl
```

- **格式**：UTF-8、每行一个完整 JSON 对象，文件扩展名 `.jsonl`；不采用 JSON 数组、纯文本或二进制格式。Go 1.23.3 的 `JSONHandler` 明确定义输出 line-delimited JSON，且每次 `Handle` 对底层 writer 只进行一次串行 `Write`，因此一条失败记录不会被并发日志交叉，也不会被轮转拆到两个文件。[Go 1.23.3 JSONHandler 文档与源码](https://github.com/golang/go/blob/go1.23.3/src/log/slog/json_handler.go#L21-L90)
- **轮转 writer**：固定 `lumberjack v2.2.1`。它是并发安全的 `io.WriteCloser`，在一次写入将超过 `MaxSize` 时先轮转再完整写入，并明确假设只有一个进程写这组文件，正好匹配本项目的单一 Go 后台进程。[lumberjack v2.2.1 文档](https://github.com/natefinch/lumberjack/blob/v2.2.1/README.md)；[Write/rotate 源码](https://github.com/natefinch/lumberjack/blob/v2.2.1/lumberjack.go#L130-L202)
- **固定策略**：`MaxSize=10 MB`、`MaxBackups=9`、`MaxAge=30`、`LocalTime=false`、`Compress=false`。当前文件加最多九个备份的名义未压缩上限约 100 MB；旧备份在首次写入和轮转触发的清理中同时受数量和 24 小时制天数约束。清理在同进程 goroutine 中执行且库会忽略清理错误，所以 100 MB 与 30 天都是正常文件系统下的目标边界，不是磁盘配额或硬性删除 SLA；`runlog` 启动检查必须把超界报告为 degraded。[lumberjack 清理规则](https://github.com/natefinch/lumberjack/blob/v2.2.1/README.md#cleaning-up-old-log-files)；[首次写入触发源码](https://github.com/natefinch/lumberjack/blob/v2.2.1/lumberjack.go#L261-L277)；[异步清理与错误处理源码](https://github.com/natefinch/lumberjack/blob/v2.2.1/lumberjack.go#L376-L395)
- **不压缩**：100 MB 已被数量/大小双重约束；保持所有文件为原生 JSONL，检索不需要 gzip 分支、异步解压或额外命令，故 v1 明确 `Compress=false`。
- **目录**：按当前用户、当前操作系统写到应用安装目录之外；目录预建为 Unix `0700`、文件保持 `0600`。`lumberjack` 自身会用 `0755` 创建缺失目录、首次创建文件用 `0600`，所以必须由 `runlog` 在构造 writer 前先创建私有目录。[lumberjack `openNew` 源码](https://github.com/natefinch/lumberjack/blob/v2.2.1/lumberjack.go#L208-L238)
- **检索**：同一个二进制提供按需的只读 `logs find` 能力，逐个流式解码当前文件和全部轮转文件并精确匹配字段；它不是常驻进程、云服务、日志表或第二个后台。
- **持久化失败不可静默**：`slog.Logger` 在 Go 1.23.3 中会丢弃 `Handler.Handle` 的返回错误，不能把“用了 slog”当成持久化保证。[Go 1.23.3 `Logger.log` 源码](https://github.com/golang/go/blob/go1.23.3/src/log/slog/logger.go#L241-L277) `runlog` 必须在 `JSONHandler` 外包一层具体 Handler，同步捕获写入错误、向 `stderr` 发最小告警并锁存可查询的 degraded 健康状态；Worker 启动前还要真实创建目录/文件并写一条启动记录。预检失败时不得开始新的 BOSS/Pi 外部尝试。

这项研究只提供后续实现约束；不新增日志业务表，不修改当前 `CONTEXT.md`、ADR、DDL、模块规格或生产代码。

## 为什么是这组实现

当前仓库使用 Go 1.23.3，`slog` 已在标准库中提供结构化属性、按级别过滤、`Logger.With` 和 JSONL 输出。本项目是本地个人实例，外部操作耗时远高于日志编码，不存在为微秒级日志吞吐额外引入整套 logger API 的证据。

| 候选 | 官方能力与缺口 | v1 结论 |
| --- | --- | --- |
| **`slog.JSONHandler` + lumberjack v2.2.1** | 标准库直接产出 JSONL；lumberjack 只补文件轮转，`go.mod` 声明 Go 1.13，当前 Go 1.23.3 可用。[lumberjack go.mod](https://github.com/natefinch/lumberjack/blob/v2.2.1/go.mod) | **采用**。只有一个第三方运行时依赖，字段和错误树仍由本项目控制。 |
| zap + lumberjack | zap 提供高性能结构化编码，但官方明确不内建轮转，仍建议接 lumberjack；生产配置还可能采样重复日志。[zap FAQ](https://github.com/uber-go/zap/blob/master/FAQ.md#does-zap-support-log-rotation) | 不采用。没有消除轮转依赖，却增加另一套字段/级别/采样语义；外部尝试失败记录不能被采样丢弃。 |
| zerolog + lumberjack | zerolog 主打低/零分配 JSON、采样和异步 writer，也支持 slog Handler。[zerolog 官方 README](https://github.com/rs/zerolog/blob/master/README.md) | 不采用。当前没有性能证据需要它，异步或采样反而增加丢失关键尝试记录的路径，轮转仍需另解。 |
| OS `logrotate`、journald、Unified Logging、Windows Event Log | 需要按平台分别设计写入、轮转和查询契约。 | 不采用。无法形成一个可在三平台用同一实现复核的本地文件契约。 |
| JSON 数组 / SQLite 日志表 / 云日志 | JSON 数组追加时不能始终保持完整文档；SQLite/云端会把技术证据重新变成业务持久化或外部平台。 | 不采用。JSONL 可以逐条追加、流式解码和按文件轮转。 |

`runlog` 是一个**具体深模块**：它把目录、权限、字段 schema、错误树、轮转、检索和健康上报藏在一个小接口后面。只有一个生产实现，不建立 `Logger` port、公共 `ports` 包或跨模块日志 interface；测试直接使用临时目录、真实 `JSONHandler` 和真实轮转 writer。删除这个模块会让同一组易错规则重新散到三个执行模块，所以它有实际深度，而不是 pass-through。

## 跨平台写入目录

日志是需要跨重启保留、但不属于用户文档的本地运行历史；不能放工作目录、安装目录或临时目录。

| 系统 | v1 目录 | 一手依据 |
| --- | --- | --- |
| macOS | `~/Library/Logs/boss-job-agent/` | Apple 把 `Library/Logs` 定义为日志文件所在目录。[macOS Library Directory Details](https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/FileSystemProgrammingGuide/MacOSXDirectories/MacOSXDirectories.html) |
| Linux / 其他遵循 XDG 的 Unix | `${XDG_STATE_HOME}/boss-job-agent/logs/`；变量为空时 `~/.local/state/boss-job-agent/logs/` | XDG 0.8 把 logs/history 明确列为 `$XDG_STATE_HOME` 的内容，默认根是 `$HOME/.local/state`；相对环境变量路径必须视为无效。[XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/latest/) |
| Windows | `%LOCALAPPDATA%\boss-job-agent\Logs\` | Windows `FOLDERID_LocalAppData` 是每用户、本机范围的 `%LOCALAPPDATA%`（默认 `%USERPROFILE%\AppData\Local`）。[Microsoft Known Folders](https://learn.microsoft.com/en-us/windows/win32/shell/knownfolderid#folderid_localappdata) |

实现时使用 `runtime.GOOS` 选择规则、`os.UserHomeDir`/环境变量解析根目录并用 `filepath.IsAbs` 拒绝相对 XDG 路径。三个目录最终文件都叫 `boss-job-agent.jsonl`。不在目录解析失败时回退到当前目录或 `os.TempDir`，因为这会把“持久化证据”悄悄降级成不可预测或会被清理的位置。

启动顺序必须是：解析绝对路径 → `MkdirAll(0700)` → 检查现有目录不是符号链接到意外位置 → 创建/追加文件并确认权限 → 写启动记录 → 才启动三个 Worker。Windows 的 ACL 由用户目录提供，Unix 额外检查目录不比 `0700`、文件不比 `0600` 更宽；已有过宽权限应收紧或拒绝启动外部 Worker，而不是继续泄漏错误内容。

## JSONL 字段约定

所有机器检索字段使用固定的 snake_case 英文名；`msg` 只作人读摘要，不参与判定。`schema_version=1` 允许后续检索器同时理解旧文件。

| 字段 | 类型 | 约束 / 解决的问题 |
| --- | --- | --- |
| `time` | RFC 3339 Nano 字符串 | 统一写 UTC；用于跨文件排序，不作为唯一键。 |
| `level` / `msg` | 字符串 | slog 内建的人读字段，不作为检索契约。Go 1.23.3 的 `INFO` 等于零值，内建 Handler 会省略其 `level`；失败记录仍使用 `ERROR`。[Go 1.23.3 JSONHandler 源码](https://github.com/golang/go/blob/go1.23.3/src/log/slog/json_handler.go#L49-L67) |
| `schema_version` | 整数 | v1 固定为 `1`。 |
| `event` | 枚举字符串 | 机器字段，至少有 `external_attempt_started`、`external_attempt_finished`；检索不解析 `msg`。 |
| `outcome` | 枚举字符串 | `external_attempt_finished` 必填，值为 `succeeded` 或 `failed`；打招呼的外部影响仍由独立的 `outreach_effect` 表达。 |
| `trace_id` | 字符串 | 32 个小写十六进制字符、不得全零；一次 Claim → Execute → Finish 业务尝试内的相关记录共用。该形状与 W3C 的 16 字节 trace-id 一致，未来可传播而无需改格式。[W3C Trace Context](https://www.w3.org/TR/trace-context/#trace-id) 由 `crypto/rand.Read` 取得 16 字节后十六进制编码；Go 将其定义为密码学安全随机源。[Go `crypto/rand`](https://pkg.go.dev/crypto/rand) |
| `flow` | 枚举字符串 | `discovery`、`assessment`、`outreach`，分别对应岗位发现、岗位鉴定、打招呼三条流程。 |
| `operation` | 枚举字符串 | 至少有 `list_page`、`read_job`、`submit_assessment`、`confirm_assessment_results`、`check_contact_status`、`send_first_contact`；区分同一次业务尝试里的不同外部调用。 |
| `discovery_run_id` | 整数 | 只用于岗位发现；等于 `discovery_runs.id`。 |
| `platform_job_id` | 字符串 | 只用于岗位鉴定/打招呼；使用 BOSS 稳定平台岗位标识，作为平台岗位的领域身份。必要时另带诊断用 `platform_job_row_id`，但检索契约不依赖数据库行号。 |
| `attempt_no` | 整数 | 原样记录所属流程已经持久化的生命周期尝试号：发现用 `attempt_no`，鉴定用 `assessment_attempt_no`，打招呼用 `outreach_attempt_no`；不新造或重置另一套计数。 |
| `page_no` / `job_ordinal` / `job_id_fingerprint` | 整数 / 整数 / 64 位十六进制字符串 | `list_page` 记录页码；`read_job` 另记录从 1 开始的页内序号与稳定 ID 的 SHA-256 指纹。发现日志不记录搜索条件、原始稳定 ID、请求或响应材料。 |
| `error_category` | 枚举字符串 | 失败必填。基础值：`transient`、`authentication_expired`、`verification_required`、`platform_limited`、`invalid_response`、`invalid_protocol`、`unknown`。稳定分类用于检索；三个模块仍各自拥有错误类型和重试决策，不建立公共错误包。 |
| `error_chain` | 对象数组 | 失败必填；保存从外层语境到底层原因的完整错误树快照，每个节点有 `path`、`type`、`message`。只用于诊断，不从 `type/message` 反推重试。 |
| `outreach_effect` | 枚举字符串 | 仅打招呼写动作：`confirmed_sent`、`confirmed_no_effect`、`possibly_effective`；它独立于技术错误分类，不能用 `error_category` 猜测外部影响。 |

一次 Pi 请求可能包含多个平台岗位。请求级记录共用一个 `trace_id`，但成功/失败终态要按平台岗位各写一条自包含记录，分别带该岗位的 `platform_job_id + attempt_no`；最多五项的少量重复换来替代键可直接精确匹配，不把检索规则藏进数组。`batch_size` 和 `batch_item_index` 可作为附加诊断字段。

示例失败记录：

```json
{"time":"2026-09-01T07:58:40.716Z","level":"ERROR","msg":"external attempt failed","schema_version":1,"event":"external_attempt_finished","outcome":"failed","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","flow":"discovery","operation":"read_job","discovery_run_id":42,"attempt_no":7,"page_no":3,"job_ordinal":4,"job_id_fingerprint":"9542806604c794eebc1517859836f31a3cf607ba0363d16be187120bb497c5fb","error_category":"transient","error_chain":[{"path":"0","type":"*fmt.wrapError","message":"read job: context deadline exceeded"},{"path":"0.0","type":"context.deadlineExceededError","message":"context deadline exceeded"}]}
```

## 完整错误树，不只是 `err.Error()`

`slog.Any("error", err)` 不够：`JSONHandler` 对 error 属性只调用 `Error()` 输出一个字符串，不保留每一层的类型或分支。[Go 1.23.3 JSONHandler error 编码](https://github.com/golang/go/blob/go1.23.3/src/log/slog/json_handler.go#L69-L87)

Go 的错误不一定是单链。错误可以实现 `Unwrap() error` 或 `Unwrap() []error`；`errors.Is/As` 把它视为树并按先序深度优先遍历。`fmt.Errorf` 的一个 `%w` 产生单子节点，多个 `%w` 产生 `[]error`。[Go `errors` 包](https://pkg.go.dev/errors)；[Go `fmt.Errorf`](https://pkg.go.dev/fmt#Errorf) 因此 `runlog` 必须：

1. 从外层错误开始记录 `path="0"`、具体 `%T` 和已脱敏 `Error()`；
2. 优先识别 `interface{ Unwrap() []error }`，按返回顺序递归为 `0.0`、`0.1`；否则识别 `interface{ Unwrap() error }` 并递归为 `0.0`；
3. 把所有节点放在**同一条** `event=external_attempt_finished, outcome=failed` JSONL 记录内；不能每层一行，否则轮转或进程中断会留下不完整链；
4. 保留每个节点但对单条 message 做脱敏和长度上限，另记 `message_truncated=true`；岗位发现的 `ReadJob` 还必须在每个错误树节点中精确替换当前原始稳定 ID，只保留 attempt 上的不可逆 fingerprint；禁止记录 Cookie、Authorization、完整简历、完整 JD、招呼语、原始页面 HTML、完整 Prompt 或模型原文；
5. `error_category` 在写日志前由所属模块根据自己的类型/`errors.Is`/`errors.As` 映射。日志中的 `type` 和 `message` 是诊断快照，不是业务分支输入。
6. 对非标准的循环 `Unwrap` 或异常深树设置节点/深度上限，并显式写 `error_chain_truncated=true`；标准库包装和项目 Adapter 返回的正常无环错误树必须完整，不能静默截断。

## 跨轮转文件稳定检索

首选查询是 `trace_id`。不知道 trace 时使用精确替代键：

```text
岗位发现：flow=discovery
        + discovery_run_id
        + attempt_no
        + operation
        + page_no
        + job_ordinal + job_id_fingerprint（定位具体 ReadJob 时）

岗位鉴定：flow=assessment + platform_job_id + attempt_no [+ operation]

打招呼：flow=outreach + platform_job_id + attempt_no [+ operation]
```

同一个二进制的按需查询实现应：

1. 只枚举日志目录中严格匹配 `boss-job-agent.jsonl` 和 lumberjack `boss-job-agent-<UTC timestamp>.jsonl` 的普通文件；不跟随目录外符号链接；
2. 对每个文件逐行流式 `json.Decoder`/扩容后的 `bufio.Scanner`，按 JSON 字段类型做**精确相等**，不能用 substring grep；
3. 校验 `schema_version`，把命中按记录 `time`、文件名、行号稳定排序；
4. 返回每个命中的完整事件，失败记录直接包含完整 `error_category + error_chain`；
5. 任一文件打不开、JSON 损坏或行超过上限时返回“查询不完整”错误并列出文件，不能把它降级为“没有结果”；
6. 没有命中只表示“当前保留文件内未找到”，不能据此修改业务状态；记录可能已超过保留边界。

人工排障可用同一查询模块的 CLI 输出 JSON；Web 若以后需要展示，只调用这个只读查询，不读取目录或复制过滤逻辑。整个过程是调用时运行、结束即退出的同一可执行文件子命令，不产生常驻日志索引、SQLite 表或后台进程。

## 可复核小型验证

研究期间在临时目录用 Go 1.23.3、真实 `slog.JSONHandler` 和 `lumberjack v2.2.1` 做了小型测试，验证代码未保留在仓库：

- 写入 651 条 JSONL 记录并触发 1 MB 测试阈值的尺寸轮转，最终得到 4 个文件；逐行解码全部成功，没有记录跨文件拆分；
- 目标发现失败记录已进入轮转文件；按 `trace_id` 与按 `flow + discovery_run_id + attempt_no + operation + search_role/search_city/page_no` 两种查询得到同一条记录；
- 错误使用外层 `%w` 包裹 `errors.Join` 的两个分支，落盘后完整得到 6 个带 path/type/message 的节点；
- 用返回“simulated disk full”的 writer 证明普通 `slog.Logger.Error` 不把错误交给调用者，但外包的 reporting Handler 能在返回前捕获并锁存该写入故障。

复核场景固定为 `MaxSize=1 MB`、`MaxBackups=9`、`MaxAge=30`、`Compress=false`：先写一条带上述复合键和 6 节点错误树的目标失败记录，再写 650 条约 6 KiB 的填充记录触发轮转；最后枚举严格匹配的四个文件、逐行解码并分别执行 trace 查询和复合键查询。这个缩小场景只把生产尺寸阈值从 10 MB 调成 1 MB，不替换 Handler、writer、文件格式或查询算法。

后续实现至少保留以下真实测试：尺寸轮转、九备份/30 天清理、并发写 JSON 有效性、trace 与替代键等价、`Unwrap() error`/`[]error` 错误树、写入失败健康降级、目录权限和三平台路径解析。测试使用临时目录和真实 handler/writer，不为文件系统再暴露公共 adapter seam。

## 后续实施边界

本研究已经决定技术方向和可检索字段，但没有授权立即改生产规格。后续架构/实现票仍需决定：

1. `runlog` 在最终 Go 包目录中的具体位置，以及启动健康状态如何投影到 Web；
2. 当前在途外部尝试遇到日志磁盘写入失败时，安全完成业务状态与停止新尝试的精确顺序；
3. `logs find` 的正式命令参数和面向用户的保留期提示；
4. 每个模块更细的内部错误类型到稳定 `error_category` 的映射表；
5. 依赖升级门禁如何固定并审计 lumberjack v2.2.1。

这些后续项都不得改变既有事实：业务表只保存恢复所需状态与真实业务证据；技术错误文本、错误链和运行历史只在轮转 JSONL；`SearchService`、`AdviceService`、`PostService` 各自拥有错误分类与重试，不引入公共 Retry/Worker/错误包、日志业务表、云日志平台或第二个后台进程。
