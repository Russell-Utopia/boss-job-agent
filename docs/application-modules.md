# v1 后台模块与接口

本文约定实现前的模块 seam、状态所有权和调用语义，不重复展开 Go 包结构或内部函数。业务模块在代码中的包名、根类型与装配方式见 [Go 工程架构](./architecture.md)；例如本文的 `JobPool` 在 Go 中实现为 `jobpool` 包的 `Pool` 根类型，构造后通过实例调用 `pool.Observe(...)`，不是 `包.类型.方法` 形式的运行时调用。

| 本文业务简称 | Go 包与根类型 |
| --- | --- |
| `OnlineResumeVersions` | `onlineresume.Versions` |
| `SearchService` | `discovery.Service` |
| `JobPool` | `jobpool.Pool` |
| `AdviceService` | `assessment.Service` |
| `PostService` | `outreach.Service` |
| `AutomationSettings` | `automationsettings.Settings` |

## 运行结构

一个常驻后台进程启动岗位发现、岗位鉴定和打招呼三个执行模块。三个模块不在内存中互相传递任务，SQLite 中的当前业务状态、在线简历版本和自动化设置是进程重启后仍可恢复的衔接点；MVP Web 只调用下面的业务接口和只读查询，不直接访问 SQLite。具体信息架构、文案和验收场景见 [MVP Web 交互规格](./web-mvp.md)。

三个执行模块采用相同但不共享泛型框架的执行形状：`Run` 在启动时立即执行一轮内部 `runSchedulingCycle(ctx, now)`，之后用一分钟周期继续执行；每轮调度状态判断只读取一次本机 `time.Now()` 并把该值传给本轮扫描。周期扫描同时处理普通可领取工作、已到重试时间的失败工作和过期租约，最终都进入各模块原有的领取路径。岗位发现的一轮扫描可以包含多个串行外部读取，所以每个页面清单或岗位读取返回后再读取当时的 `time.Now()`，用于记录真实进度时间并把 Worker 租约续到该进度之后；这不改变本轮的到期判断，也不建立精确定时器。v1 接受最多约一个扫描周期加本轮执行时间的调度延迟，不定义 `Clock`、`Retry`、通用 `Worker` 或通用调度器接口。测试直接以固定 `now` 调用单轮逻辑，并可让模块时钟前进以验证长页续租，不等待真实时间。

## 状态所有权

| 模块 | 唯一可写状态 | 可以读取 | 不负责 |
| --- | --- | --- | --- |
| `OnlineResumeVersions` 在线简历版本模块 | `online_resume_versions` | 求职者显式刷新时读取 BOSS 在线简历 | 自动刷新、搜索岗位、选择鉴定策略、修改历史版本 |
| `SearchService` 岗位发现模块 | `discovery_runs` | 当前已保存的在线简历版本、全局岗位处理结果 | 刷新在线简历、选择鉴定策略、直接更新 `platform_jobs`、等待鉴定或打招呼收口 |
| `JobPool` 全局岗位模块 | `platform_jobs` | 显式提交的在线简历版本和策略版本 | 浏览 BOSS、调用 Pi、运行 Worker |
| `AutomationSettings` 自动化设置模块 | `automation_settings` | 当前全局岗位数量只读视图 | 拥有岗位状态、运行 Worker、修改已经入队的动作 |
| `AdviceService` 岗位鉴定模块 | `assessment_policy_versions`、本实例受管 Pi 的临时标记文件 | `JobPool` 领取的鉴定输入、当前自动化设置 | 直接更新 `platform_jobs`、决定搜索和打招呼循环 |
| `PostService` 打招呼模块 | 不单独拥有业务表；只通过 `JobPool` 确认打招呼结果 | `JobPool` 领取的打招呼或对账输入、当前自动化设置 | 直接更新 `platform_jobs`、回复招聘者、发送简历 |
| Web | 无 | 岗位发现运行和全局岗位只读视图 | SQL、Worker 生命周期、直接设置任何状态 |

`PostService` 没有专属业务表，但仍是必要的深模块：它封装打招呼前复查、真实外部动作、成功证据识别、`possibly_contacted` 对账和浏览器错误分类。删除它会让这些有副作用的规则散落到 Worker、界面或 `JobPool` 中。

`AutomationSettings` 不是第四个执行模块或 Worker。它只集中校验并保存一行实例级设置；删除它会让 Web、`AdviceService` 和 `PostService` 分别理解时间窗、影响预览和开关语义。

`OnlineResumeVersions` 也不是执行模块或第二份求职资料。只有求职者在设置中显式点击刷新时，它才读取一次 BOSS 在线简历并保存新的不可变版本；发现和鉴定过程只读取已经保存的当前版本，不发起刷新。`SearchService` 为搜索运行记录实际采用的版本，`AdviceService` 在真正开始鉴定时记录实际采用的版本，二者不通过对方的运行记录取得输入。

## 最小业务接口

以下是调用者需要理解的完整业务接口。参数类型表示业务数据，不允许调用者提交目标状态字符串。

### `OnlineResumeVersions`

```text
RefreshFromBoss(ctx) -> ResumeRefreshResult
GetCurrent(ctx) -> OnlineResumeVersion?
```

`RefreshFromBoss` 只能响应求职者的显式操作，不能由岗位发现、鉴定 Worker、定时器或后台启动自动调用。读取失败不得创建版本，也不得改变最后一次可靠的当前版本；内容与当前版本相同就返回“内容未变化”，内容变化时才新增递增版本并在同一事务中设为当前。刷新期间如果存在使用 v1 的未结束发现运行，新产生的 v2 只成为全局当前版本，不得回写该运行；Web 必须提示“当前发现继续使用 v1，v2 将用于下一次发现和尚未开始的鉴定”。界面展示版本号、保存时间和刷新结果，不展示内部数据库 ID。

### `SearchService`

```text
Start(ctx) -> DiscoveryRunID
Continue(ctx, runID)
Pause(ctx, runID)
EndEarly(ctx, runID)
Run(ctx)                         // 只由后台进程启动
```

`Start` 在存在未结束运行时拒绝创建；它只通过 `OnlineResumeVersions.GetCurrent` 取得用户已保存的当前在线简历版本，并把版本 ID 记录到本次发现运行，不访问 BOSS 刷新简历，也不读取或记录岗位鉴定策略。版本一旦记录就不可修改；`Continue` 和 `Run` 始终读取该运行记录的同一版本，而不是重新读取全局当前版本，因此一轮发现的全部搜索范围只会使用一个版本。要用新刷新的版本重新初筛，求职者必须先让旧运行完成或执行 `EndEarly`，再执行 `Start` 创建新运行。尚无在线简历版本时拒绝开始并提示求职者先到设置中手动刷新。开始搜索不要求自动岗位鉴定或自动打招呼已经开启，也不要求已经配置固定招呼语。自动岗位鉴定关闭时，新发现且没有有效 AI 结论的岗位由 `JobPool.Observe` 保存为 `assessment_status=not_queued`，等待以后开启自动入队或由求职者手工批量选择。`Continue` 用于暂停或失败后的同一运行，并开启新的无人干预重试周期。`Pause` 和 `EndEarly` 使当前 Worker 的后续旧写入失效。`Run` 只推进搜索检查点，并把每个可靠岗位观察提交给 `JobPool.Observe`。

### `JobPool`

```text
Observe(ctx, runID, observation) -> JobView
Review(ctx, decisions[jobID, expectedJDHash, verdict, note])
QueueAssessments(ctx, jobIDs) -> BatchActionResult
QueueAuthorizedOutreach(ctx, jobIDs, authorization) -> BatchActionResult
RetryAssessmentFailures(ctx, jobIDs) -> BatchActionResult
RetryOutreachFailures(ctx, jobIDs) -> BatchActionResult

AdmitAssessments(ctx, limit) -> admittedCount
ClaimAssessments(ctx, worker, resumeVersionID, policyVersionID, evaluatorVersion, limit) -> AssessmentWork[]
FinishAssessments(ctx, outcomes)
AdmitOutreach(ctx, limit) -> admittedCount
ClaimOutreach(ctx, worker, limit) -> OutreachWork[]
FinishOutreach(ctx, outcomes)

GetActiveDiscovery(ctx) -> DiscoveryRunView?
ListJobs(ctx, filter, intendedAction) -> JobView[]
GetJob(ctx, jobID, intendedAction) -> JobView
GetJobDetail(ctx, jobID) -> JobDetailView
```

`Observe` 原子处理平台岗位去重、JD 更新、平台开放状态、因 JD 变化导致的鉴定失效、人工结论待复核以及撤回尚未领取的打招呼请求；它不读取当前策略，也不因发现运行采用的在线简历版本而选择鉴定依据。刷新简历、修改策略，或者从新运行再次观察到 JD 未变化的岗位，都不会让它改写历史岗位的鉴定或打招呼状态；重复观察只更新同一条平台岗位的当前可靠事实和最近发现时间。`Review` 携带求职者实际查看的 JD 哈希，在同一事务中拒绝已经变化的 JD，并原子处理人工结论和当前打招呼资格。`QueueAssessments` 只接受缺少当前有效 AI 结论、平台状态允许鉴定且尚未入队的选中岗位，并把它们改为 `pending`；已有成功 AI 结论的岗位不能通过本接口重新鉴定，排队时也不选择在线简历或策略。已经 `contacted` 的岗位不会再次进入鉴定或打招呼队列，已有 AI 和人工结论继续保留展示。

`QueueAuthorizedOutreach` 不是 Web 可以直接调用的命令，只接受 `AutomationSettings` 已经根据当前设置校验过的手工授权；它仍逐项重新校验岗位确实适合且可沟通，并把已授权的当前招呼语冻结到岗位。对求职者可见的手工首次打招呼命令是 `AutomationSettings.QueueRealOutreach`：它要求确认本批岗位数量、当前固定招呼语和打招呼时间窗，只处理本次选中的岗位，也不修改 `automation_settings` 中的自动打招呼开关。

`JobView` 根据 `intendedAction` 返回该岗位当前是否可以被选择，以及不可选择时的用户可见原因；“AI 鉴定”和“真实打招呼”分别计算，Web 不复制资格规则。界面进入某个批量操作后，只允许勾选当前可执行的岗位；已入队、正在处理、已打招呼、已关闭或不满足该操作条件的岗位禁用复选框并直接展示原因。

提交时 `JobPool` 仍逐项重新校验，防止页面展示后岗位状态已经变化。共享输入本身无效时整批拒绝，例如首次打招呼尚未配置固定招呼语；共享输入有效时返回 `BatchActionResult`，说明实际成功数量，以及因提交瞬间状态变化而跳过的岗位和原因。重复提交已入队岗位不会产生第二份工作。该返回结果只用于当前交互，不建立批次表或批次历史；岗位当前状态仍只写入 `platform_jobs`。

两个 `Admit` 方法只把当前符合条件但尚未入队的岗位加入对应队列；自动开关关闭时执行模块不调用它们，手工批量命令仍可直接入队。`AdmitOutreach` 使用岗位当前判断，不要求 AI 结论所用的简历和策略等于后来保存的当前版本；开启自动打招呼就是求职者允许这些仍然有效的既有结论继续产生新打招呼工作。`ClaimAssessments` 才显式接收当前已保存的在线简历版本 ID、启用策略版本 ID 和鉴定器版本，并在同一事务中把 `pending` 改为 `processing`、记录当前 JD 哈希、递增尝试编号并建立租约。到期可重试的 `failed` 鉴定也由该方法直接重新领取：实际输入未变化就延续失败次数，输入变化则开始新的失败周期。打招呼领取继续按自己的时间窗处理。两个 `Finish` 方法使用岗位 ID、尝试编号和结果证据拒绝迟到写入。两种人工重试命令只接受各自流程中明确的 `failed`；`RetryOutreachFailures` 不能把 `possibly_contacted` 直接改回待打招呼。查询返回只读视图，不返回可持久化实体或数据库句柄。

### `AdviceService`

```text
Run(ctx)                         // 只由后台进程启动
Confirm(ctx, confirmationBatch) -> ConfirmationReceipt // Pi 的唯一业务回调入口
GetPolicyOptimization(ctx) -> PolicyOptimizationView
GeneratePolicyDraft(ctx, generationJobIDs) -> PolicyDraft
ValidatePolicyDraft(ctx, draft) -> PolicyValidationReport
CreatePolicyVersion(ctx, rules, changeNote) -> PolicyVersionID
```

`Run` 在自动岗位鉴定开启时先通过 `JobPool.AdmitAssessments` 接收新工作；这一步只形成 `pending`。无论开关是否开启，它在每次准备领取工作时都只读取数据库中当前已保存的在线简历版本、用户设置中当前启用的策略版本和当前鉴定器版本，再显式传给 `JobPool.ClaimAssessments`，整个过程不得访问 BOSS 刷新简历。领取事务把岗位改为 `processing` 并记录实际采用的简历版本、策略和 JD；返回的 `AssessmentWork` 包含完整简历和完整策略，Agent 不只接收内部 ID，也不读取岗位发现运行。每次领取数量不得让当前 `processing` 岗位超过“同时鉴定岗位上限”；调低上限不取消已领取工作，只阻止新的领取。`Confirm` 校验回调协议后，把每项结果交给 `JobPool.FinishAssessments`，并在 `ConfirmationReceipt` 中逐项返回已接受、格式无效或尝试号过期，单项失败不遮蔽同批其他结果。策略或在线简历当前版本变化会被尚未开始的 `pending` 岗位采用，已经 `processing` 的岗位继续使用自己实际记录的版本；在新发现运行中再次出现但 JD 未变化的历史岗位也继续保留原结论和打招呼资格，不会仅因重新发现而进入 `pending`。

新实例初始化时，`AdviceService` 同时创建并启用默认第 1 版策略。默认版要求鉴定器只依据本次实际采用的在线简历和当前 JD：明确且重要的不匹配判为 `unsuitable`，明确匹配判为 `suitable`，信息不足或证据冲突判为 `needs_user_confirmation`。它与用户后来设置的策略版本使用同一张表、同一套版本规则，不存在只藏在代码里的“无策略兜底”。初始化完成后没有当前启用策略属于数据异常，不能开始新的鉴定执行，但不影响岗位发现或形成 `pending` 队列。

人工复核只在平台岗位上积累标注，不自动调用模型、重新鉴定岗位或修改策略。`GetPolicyOptimization` 返回当前已保存的在线简历版本、当前策略和仍基于当前 JD 的全部人工“适合”与“不适合”复核；是否存在 AI 结论、AI 使用哪个旧策略或是否与人一致，都不影响人工复核进入这个只读视图。Web 在当前页面会话中管理是否启用策略验收和生成样本选择：默认关闭验收并把全部有效岗位 ID 传给 `GeneratePolicyDraft`；开启时默认随机勾选约 50%，允许求职者在生成前调整，最终只把选中的岗位 ID 作为生成样本。

`GeneratePolicyDraft` 重新校验每个选中岗位仍有基于当前 JD 的人工结论，再使用调用开始时当前已保存的完整在线简历、当前策略和这些完整岗位样本发起一次模型调用，返回完整、可直接采用的 `PolicyDraft`。返回值说明生成时间、实际采用的在线简历与策略版本、生成岗位 ID 和样本数量，但只保留在当前界面内存中，不写入 SQLite。若未启用验收，Web 明确标记“本候选策略未经独立验收”；是否启用验收必须在生成前选择，候选稿已经使用全部样本生成后不能补开验收。

只有求职者在生成前开启验收时，Web 才允许调用 `ValidatePolicyDraft`，并在调用前说明会产生额外模型消费。该方法重新读取当时全部有效人工复核，并用候选稿记录的同一不可变在线简历版本分别运行生成时的当前策略和候选策略，返回全量结果、未参与生成样本结果，以及“通过、未通过、结果有取舍、证据不足”之一；若任一生成样本已因 JD 或人工结论变化而不再有效，则要求重新生成，不能悄悄改变生成集合与验收集合的包含关系。它不能调用 `JobPool.FinishAssessments`，也不能修改平台岗位、鉴定版本、队列、岗位当前判断或打招呼资格。通过要求两版比较时“人工不适合、AI 判适合”和“人工适合、AI 判不适合”都不增加，且至少一类减少；两类不变而人工确认减少时也通过，一项改善另一项退步时返回“结果有取舍”。

求职者可以在当前界面查看和修改候选稿；验收后再次编辑会让旧报告立即失效，可以重新验收或人工直接采用。确认采用时，界面把候选稿的当前完整内容交给 `CreatePolicyVersion`，由它新增并启用不可变策略版本；未验收、未通过、有取舍或证据不足都不阻止这项显式人工决定。求职者主动关闭、取消、离开当前页面或要求重新生成时，界面必须先显眼提示“候选稿、生成样本选择和验收报告都会永久消失，再次取得需要重新调用模型”；确认后直接丢弃页面内存，不调用后台删除接口，也不建立验收历史。下一次 `GeneratePolicyDraft` 始终重新读取届时的当前策略与有效人工复核。新策略只供以后真正开始的 AI 鉴定采用，不扫描或改写已有成功鉴定；`CreatePolicyVersion` 也继续用于求职者从头保存自己编辑的完整策略。

### `PostService`

```text
Run(ctx)                         // 只由后台进程启动
```

`Run` 在自动打招呼开启时通过 `JobPool.AdmitOutreach` 把全局符合条件的岗位加入真实打招呼队列；关闭只停止新增自动入队，已经入队的工作继续保留。每次入队冻结当前固定招呼语。`Run` 可以随时领取 `possibly_contacted` 对账工作；没有配置打招呼时间窗时可以随时领取真实打招呼，配置后只有当前位于任一时间窗内才领取。浏览器动作结束后只提交结果和证据给 `JobPool.FinishOutreach`。打招呼入队、批量重试和人工操作都经 `JobPool`，不为 `PostService` 增加重复入口。

### `AutomationSettings`

```text
Get(ctx) -> AutomationSettingsView
ConfigureAssessment(ctx, enabled, processingLimit)
PreviewOutreachChange(ctx, enabled) -> OutreachChangeImpact
ConfigureOutreach(ctx, enabled, greetingText, timeWindows)
QueueRealOutreach(ctx, jobIDs, confirmation) -> BatchActionResult
```

设置模块只暴露两组业务配置、一个真实打招呼影响预览和一个手工真实打招呼命令，不提供通用键值写入接口。它校验鉴定上限、固定招呼语和打招呼时间窗，并只更新 `automation_settings` 的单行记录；`AdviceService` 和 `PostService` 读取当前设置决定是否自动入队或能否领取真实打招呼。`PreviewOutreachChange` 统一计算当前可以进入真实队列的岗位数，Web 不得复制这套筛选规则。`QueueRealOutreach` 重新读取当前设置、验证页面确认仍与当前岗位数量、完整招呼语和时间窗一致，再调用 `JobPool.QueueAuthorizedOutreach` 交付可信授权；后者重新校验每个岗位并冻结招呼语。配置变化如何唤醒后台循环属于模块内部实现，不要求界面管理 Worker。

应用第一次创建 `automation_settings` 时使用安全默认值：自动岗位鉴定关闭、同时鉴定岗位上限为 5、自动打招呼关闭、固定招呼语尚未配置、打招呼时间不受限制。这些默认值只用于第一次初始化；之后每次启动都读取用户上次保存的同一行，不能重新覆盖设置。

## 模块运行配置

- 设置页提供独立的“刷新在线简历”操作，并显示当前在线简历版本号和保存时间。只有这项显式操作可以访问 BOSS 更新版本；发现、鉴定、界面重开和后台重启都不得自动刷新。刷新失败保留旧版本并展示错误，内容未变化时不增加版本号。如果未结束的发现运行使用 v1，而刷新得到 v2，界面同时显示“当前已保存 v2；本轮发现仍使用 v1”；不提供把本轮切到 v2 的操作。
- 开始岗位发现时，Web 展示当前下游自动化设置，但只作提示，不把开关状态当作发现前提。例如自动岗位鉴定关闭时明确提示“新发现岗位将只保存，暂不进行 AI 鉴定”；自动打招呼关闭或固定招呼语尚未配置时，也只说明合适岗位暂不会自动进入打招呼队列。岗位发现要求已有一个用户手动刷新的在线简历版本；当前鉴定策略即使异常缺失，也只阻止 `pending` 岗位真正开始鉴定，不阻止发现或排队。
- 自动岗位鉴定是“新工作入队开关”，不是模块电源开关。关闭时，新的无有效结论岗位保持未入队；已有 `pending`、`processing` 和尚在自动重试周期内的工作继续完成。手工批量鉴定不受该开关限制。
- 同时鉴定岗位上限统计 `assessment_status=processing` 的岗位数。v1 只有一个在途 Pi 请求，该请求最多领取当前空闲名额数量的岗位，因此调用批量大小自然受同一个上限约束。
- 自动打招呼也是“新工作入队开关”。关闭时不再自动加入新的岗位；已经 `pending` 的岗位以及手工批量加入的岗位继续等待打招呼。
- MVP 只执行真实打招呼，不提供模拟打招呼、执行模式选择或 `simulated` 岗位状态。无外部副作用的验证只存在于测试 Adapter 和显式本地 live integration test 中。
- 自动化设置保存一条当前固定招呼语；自动或手工把岗位加入首次打招呼队列时，都把当时的完整文本复制到岗位上。之后修改全局招呼语只影响新入队岗位，不改变已有 `pending`、`processing` 或已完成记录；尚未配置招呼语时不能加入真实打招呼队列。
- 手工批量操作明确显示为“加入真实打招呼队列”。确认页展示本批可入队岗位数量、将冻结的完整招呼语和当前打招呼时间限制；未设置时间窗时明确显示“全天可打招呼”。确认只授权本批岗位，不会顺便开启或修改自动打招呼。
- 打招呼时间窗按 `Asia/Shanghai` 解释；未设置任何时间窗表示全天允许开始真实打招呼。设置后允许多个每日重复且互不重叠的半开区间，例如 `[10:00, 12:00)`、`[14:00, 16:00)`；区间结束后不再领取新的打招呼工作，已经进入 `processing` 的动作继续收尾。手工和自动加入的真实打招呼遵守相同规则，`possibly_contacted` 对账全天允许。
- 模块运行配置保存在 SQLite 的单行 `automation_settings` 中，后台重启后直接恢复。它属于整个本地实例，不放进 `discovery_runs`，也不把整套设置复制到每个 `platform_jobs`；岗位真正开始鉴定时只记录本次实际采用的在线简历版本、JD 和策略版本，打招呼入队时只记录本轮冻结的招呼语。

## 两个岗位状态机的衔接

队列是 `platform_jobs` 上的持久化状态，不是把全部 JD 一次性推给外部程序：

```text
鉴定：not_queued → pending → processing → suitable / unsuitable / needs_user_confirmation
打招呼：not_queued → pending → processing → contacted / possibly_contacted / failed
```

开启自动岗位鉴定时，`AdviceService` 可以在一次短事务中把当前所有符合条件的 `not_queued` 改为 `pending`；此时不选择简历或策略，只形成可见、可恢复的等待队列。准备调用 Agent 时，它才读取当前已保存的在线简历版本和用户设置策略，最多领取“同时鉴定岗位上限”数量的 `pending` 为 `processing`，记录实际采用的版本，并在一个 Pi 请求中发送完整简历、完整策略和这些岗位的 JD；这里不访问 BOSS，也不会把全部等待岗位一次性推给 Pi。

同理，开启自动打招呼时显示的 N 是开启确认这一刻，当前满足打招呼资格且尚未入队的岗位数量。确认后，`PostService` 按时间窗逐个把 `pending` 领取为 `processing`。关闭自动打招呼只影响之后的新入队，不修改已经排队或执行中的真实打招呼授权。

鉴定状态 `suitable` 是鉴定状态机的成功终态之一，只构成打招呼入队条件，不直接等于 `outreach_status=pending`。只有 `PostService` 的自动入队规则正在开启，或者求职者执行手工批量加入，`JobPool` 才会把打招呼状态从 `not_queued` 改为 `pending`；因此鉴定和打招呼状态机保持独立，只通过打招呼资格规则关联。

关闭自动打招呼只阻止之后的合适岗位自动入队，不修改已有 `pending` 和 `processing`：待打招呼岗位继续按时间窗执行，执行中的岗位继续收尾。v1 不再增加单独的“暂停打招呼”概念。

只有取得 BOSS 证据后的 `contacted` 才表示“这个岗位已经真实打招呼过，以后不再重复打招呼”；从 `not_queued` 进入 `pending` 前，`JobPool` 必须重新检查岗位开放状态和当前有效的人机结论。

## 事务与外部动作

`JobPool` 的所有方法都必须支持三个执行模块并发调用，并只执行短 SQLite 事务。Pi 调用和浏览器操作绝不能包在数据库事务里，统一采用以下顺序：

```text
Claim：校验资格、递增尝试号、写 processing 和租约并提交
  ↓
Execute：在事务外调用 Pi 或浏览器
  ↓
Finish：用岗位 ID + 尝试号校验当前状态，原子写入结果或失败
```

如果进程在 `Execute` 中断，租约恢复规则负责产生 `failed` 或 `possibly_contacted`；如果旧执行在新尝试开始后才返回，`Finish` 必须拒绝它。`JobPool` 本身不调用 Pi 或浏览器，因此外部副作用和平台岗位状态机可以分别测试。

每个执行模块长期管理自己的 Worker，工作只作为参数传入 Worker：岗位失败、完成或等待重试时，Worker 释放的是当前工作占用，而不是自己的外部资源。岗位发现 Worker 长期持有自己的 BOSS 发现 Adapter 与 session；首次打招呼 Worker 长期持有同时满足打招呼与只读对账接口的 BOSS Adapter 与 session；岗位鉴定 Worker 长期持有自己的 Pi Adapter 与受管进程。v1 各有一个 Worker；未来增加 Worker 时，每个新增 Worker 都拥有独立 Adapter 实例及独立 session 或 Pi 进程，工作不与原 Worker 建立亲和关系，而是通过 SQLite 租约原子领取。

## 外部依赖 seam

外部接口由调用模块在自己的包内定义，参数和返回值只使用本项目业务类型，不暴露 Kimi WebBridge、Pi RPC、MCP 或其他外部 SDK 类型。每次调用只执行一次外部尝试；Adapter 不循环重试、不休眠，也不拥有业务状态、租约或重试次数。

### `OnlineResume`

由 `OnlineResumeVersions` 定义并调用：

```go
type OnlineResume interface {
    Read(context.Context) (ResumeContent, error)
}
```

`Read` 一次返回完整的求职条件、工作经历、项目经历、教育经历和技能；任一必需部分读取或校验失败时整次失败，不返回可保存的部分简历。生产 Adapter 使用 BOSS session，内存 Adapter 返回固定完整简历或指定错误；接口不暴露 session。

### `JobDiscovery`

由 `discovery.Service` 定义并调用：

```go
type JobDiscovery interface {
    ListPage(context.Context, SearchRange, int) (JobPage, error)
    ReadJob(context.Context, string) (JobObservation, error)
}
```

`ListPage` 的第三个参数是从 1 开始的页码，返回本页有序稳定岗位 ID 和显式 `HasMore`。`ReadJob` 每次只返回一个经过稳定 ID、完整 JD 和可靠平台状态校验的岗位观察。`discovery.Service` 拥有遍历顺序、持久化页内检查点、恢复核对、错误分类、重试与页码推进；生产 Adapter 只在发现 Worker 独占的 BOSS session 内暂存读取当前页岗位详情所需的技术参数，不能把端点、Cookie、会话材料或浏览器执行类型放入 interface 或 SQLite。

每个岗位先在短 SQLite 事务中校验当前 Worker 并经 `JobPool.Observe` 写入全局岗位池，事务提交后再单独推进页内序号；跨进程暂停或新尝试不能在校验与岗位写入之间插入旧写。恢复时重新 `ListPage` 并完整核对有序 ID 与 `HasMore`。清单变化时返回 `invalid_response` 并保留原检查点。只有当前页全部岗位完成后才能清除页内检查点，并且只有显式 `HasMore=false` 才能证明当前搜索范围耗尽。生产 Adapter 和受控 Adapter 都实现同一 `ListPage`/`ReadJob` seam；默认测试与 CI 只使用受控 Adapter，不访问 BOSS。

### `SendFirstContact` 与 `CheckContactStatus`

两者都由 `PostService` 定义并调用；同一个首次打招呼 Worker 的生产 Adapter 同时满足这两个小接口并共用该 Worker 独占的 BOSS session：

```go
type SendFirstContact interface {
    Send(context.Context, FirstContactRequest) (FirstContactResult, error)
}

type CheckContactStatus interface {
    Check(context.Context, PlatformJobRef) (ContactStatus, error)
}
```

`Check` 只读，只有取得可靠证据时才返回“已沟通”或“未打招呼”；读取失败返回错误，不把“未知”写成平台岗位状态。真实打招呼必须先调用 `Check` 复查；`Send` 的 `FirstContactResult` 无论是否同时返回错误，都必须把外部影响分为“已确认发送”“已确认没有产生发送”和“可能已经产生发送”三类，最后一类必须进入对账而不能自动重发。生产 Adapter 操作 BOSS；内存 Adapter 可以分别模拟三种外部影响和只读对账结果。

### `AssessmentSubmitter`

由 `AdviceService` 定义并调用：

```go
type AssessmentSubmitter interface {
    Submit(context.Context, AssessmentRequest) error
    Close(context.Context) error
}
```

`Submit` 只表示一次完整请求已经提交给 Pi，不同步返回 AI 鉴定结论；结论只能经 `AdviceService.Confirm` 回传。一次请求携带完整简历、完整策略和一组完整岗位输入，每项都带平台岗位本地主键与鉴定尝试号；不同请求使用隔离会话。`Close` 只能关闭该 Adapter 可验证归属的受管 Pi 进程。生产 Adapter 管理 Pi RPC 子进程，内存 Adapter 记录提交内容并显式触发确认回调。

### `PolicyAdvisor`

由 `AdviceService` 定义并调用：

```go
type PolicyAdvisor interface {
    Generate(context.Context, PolicyGenerationRequest) (PolicyDraft, error)
    Validate(context.Context, PolicyValidationRequest) (PolicyValidationResult, error)
}
```

`Generate` 和 `Validate` 各执行一次完整外部模型调用。生成请求包含当前已保存的完整在线简历、当前策略和求职者本次选定的有效人工复核样本；验收请求使用同一在线简历版本，并包含候选策略、生成时的当前策略、全部有效人工复核以及哪些样本参与过生成，返回全量与未参与生成样本的逐项实验结论。两种结果都只是当前页面会话中的策略证据，不携带岗位鉴定尝试号，不调用 `confirm_assessment_results`，也不能进入 `JobPool.FinishAssessments`。即使生产环境与岗位 AI 鉴定复用同一种 Pi 运行能力，这仍是独立于 `AssessmentSubmitter` 的外部接口；内存 Adapter 直接返回固定候选策略和验收结果，使策略优化可以在不改写 `platform_jobs` 的情况下测试。

### 测试 Adapter 与 live integration

默认 `go test ./...` 使用真实业务模块和真实内存 SQLite，只把 BOSS、Pi 等外部依赖替换为受控 Adapter；它验证业务资格、状态机、调用次数和外部影响分类，但不证明生产 Adapter 能访问当前真实外部系统。每个生产 Adapter 还必须有显式本地 live integration test，直接连接真实数据源并按自己的接口合同自动校验输入、输出、错误分类和 trace。接口稳定后再运行全部生产 Adapter 组成的完整真实链路测试。

live integration test 不属于默认测试或 CI；缺少 BOSS 登录、真实 Pi 或显式 live 配置时不得运行，也不能制造失败噪声。真实 `SendFirstContact` 测试每次最多自动选择一个当前合格岗位，保存目标、招呼语、外部证据和 trace，并验证后续检查不会再次发送。受控 Adapter、live 配置和测试结果都不形成 Web 功能、自动化设置或 `platform_jobs` 业务状态。

### 错误、日志与重试

外部 Adapter 的错误必须让所属执行模块能够区分临时失败、登录失效、验证码、平台限制、响应或协议无效；首次打招呼另外使用上述三态结果表达外部影响，不能用通用错误码推测是否已经发送。各模块可以保留更细的内部错误，但不定义跨模块公共错误包。

每次外部尝试都写入持久化结构化运行日志，至少包含 `trace_id`、执行流程、操作名、运行或平台岗位标识、尝试号、稳定错误分类和底层错误链；同一业务动作的相关日志使用同一 `trace_id`，也可通过实体标识与尝试号独立检索。技术错误文本、暂停原因和提前结束原因不写入业务表；业务表只保存恢复所需的状态、尝试次数、连续失败次数、`retry_at`、租约以及真实业务证据。

三个执行模块各自根据自己的错误分类和外部影响计算是否重试及下次 `retry_at`，不共享通用 `Retry` Interface 或 Backoff Adapter。`runSchedulingCycle` 周期查询当前可执行工作；普通工作与到期重试在领取后走相同 Worker 路径。`JobPool` 直接使用真实 SQLite 事务，测试使用内存 SQLite 和同一份 DDL，不为了 mock 数据库再暴露 Repository。
