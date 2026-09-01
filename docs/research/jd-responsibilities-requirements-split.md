# 为何最初把 JD 拆成「职责/要求」?该前提是否成立(事实与依赖清单)

研究日期:2026-09-02

仓库基线:`research/issue-42-jd-split`,分叉自 `issue-1` 的 `6f6e6cd`(“测试:补齐 MVP 离线恢复与安全边界验收(issue-16)”)

对应工单:[#42 探究:为何最初把 JD 拆成「职责/要求」?该前提是否成立](https://github.com/Russell-Utopia/boss-job-agent/issues/42)(Wayfinder map [#41](https://github.com/Russell-Utopia/boss-job-agent/issues/41))

范围声明:这是 AFK 事实采集,只列证据与依赖,不做去留决策(决策见 #43)。所有结论基于本仓库的一手源码、ADR 与 git 历史,已给出 file:line 与 commit/ADR/issue 引用。

---

## 一、拆分在何处被强制(确认)

沿发现→落库→鉴定链路,「职责(Responsibilities)+要求(Requirements)两段均非空」被反复强制:

1. **发现适配器** `internal/adapters/boss/job_discovery.go`
   - `requirementsHeading` 正则(`job_discovery.go:178-180`)匹配「任职要求/岗位要求/…」等标题,作为职责与要求的分界。
   - `splitReliableJD`(`job_discovery.go:258-270`):以该标题切分完整 JD;若**找不到标题、标题在开头、或任一段为空**则整条观察失败(`"complete JD has no reliable responsibilities and requirements boundary"` / `"complete JD responsibilities or requirements are empty"`)。
   - 注意:BOSS 详情脚本本身返回的是**单一** `fullJD` 字段(`job_discovery.go:171` `rawDiscoveryJob.FullJD`;抓取脚本 `job_discovery.go:581` `fullJD: normalized(info.postDescription || info.jobDescription)`)。拆分完全发生在 Go 侧,属适配器的**加工**,不是平台原生结构。

2. **领域校验** `internal/discovery/model.go`
   - `JobObservation` 带 `Responsibilities`/`Requirements` 两独立字段(`model.go:43-44`)。
   - `validateObservation`(`model.go:143`)把两字段都列入必填,任一为空即 `"stable ID, basic information, and complete JD are required"`。

3. **岗位池落库** `internal/jobpool/pool.go`
   - `judgmentContent` 结构体把两段作为独立 JSON 键 `responsibilities`/`requirements`(`pool.go:527-534`),该 JSON 就是 `jd_json`,其 SHA-256 即 `jd_hash`(`pool.go:548-552`,`encodeJudgmentContent`)。
   - `validateObservationContent`(`pool.go:555`)在 `pool.go:566` `hasEmptyText(content.Responsibilities, content.Requirements)` 强制两段非空,否则 `"complete JD is required"`。

4. **鉴定输入** `internal/assessment/worker.go`
   - `AssessmentJobInput` 把两段作为独立 JSON 字段 `responsibilities`/`requirements` 送 Pi(`worker.go:69-70`,组装于 `worker.go:275-283` `assessmentInputs`)。

5. **(旁路)可见页诊断探针** `internal/adapters/boss/visible_page_probe.go`
   - 另有一套 `visibleResponsibilitiesHeading`/`visibleRequirementsHeading`(`visible_page_probe.go:207-210`),把可见 JD 分类为 `explicit_split`/`responsibilities_only`/`requirements_only`(`visible_page_probe.go:45-46, 300-317`)。这是 issue-36 引入的**降级/诊断探针**,不参与真实鉴定落库,但说明「同一 JD 未必两段齐全」在本项目里是已知事实。

---

## 二、拆分的初始动机与所服务的具体需求(答问 1)

**结论:拆分不是任何 ADR 或 issue 验收显式要求的产品需求,而是 issue-4 编码时为「判断详情页可靠/完整」而选用的实现级启发式,顺带把结果建成两段模型。**

- **引入时间与提交**:两段字段随 issue-4 的第一条离线路径进入代码。
  - `6a27211`「feat:完成单范围岗位发现与全局岗位列表(issue-4)」(2026-08-31)首次在 `discovery/model.go`、`jobpool/pool.go`、`job_discovery.go` 引入 `Responsibilities`/`Requirements`(`git log -S "Responsibilities"`)。
  - `dce5f5a`「fix:收紧岗位发现确认与可靠观察(issue-4)」(2026-08-31)首次引入 `splitReliableJD` 与 `requirementsHeading`(`git log -S "splitReliableJD"`)。该提交 diff 的注释明说:出现在搜索响应中 + 详情响应成功匹配,是「该适配器判断岗位开放的可靠证据」——即拆分是**可靠性/完整性判据**的一部分,不是为了给下游提供结构化语义。

- **originating issue 只要求「完整 JD」,未要求「两段」**:issue-4 验收标准原文为「任一岗位缺少稳定 ID、**完整 JD** 或可靠平台状态时整页失败」(`gh issue view 4`)。文案是「完整 JD」而非「职责与要求」。把「完整 JD」实现为「必须能切出两段非空」是编码代理自选的**代理指标(proxy)**:一条能被切成非空「职责+要求」的文本,被当作「这是一份完整、规整的 JD」而非截断/占位页。

- **ADR 层面无一条把「预拆分」立为决策**。相关 ADR 均只谈「完整 JD」这一整体输入:
  - ADR-0012(`docs/adr/0012-configurable-traceable-job-assessment.md`):`jd_hash` 表示「规范化后的岗位判断内容」;通篇讲「当前 JD」为唯一事实源,未要求预拆。第 9 行仅在描述性话术里出现「针对旧职责和要求的判断」,是对内容的泛指,非结构约束。
  - ADR-0014(`0014-push-assessments-to-pi-with-single-confirmation-tool.md`)第 3 行:程序「主动推送完整在线简历、**完整 JD**、完整策略…」——契约措辞是**完整 JD**,不是两段。
  - ADR-0022(`0022-separate-policy-validation-from-job-assessment.md`):只谈策略验收与真实鉴定隔离,与 JD 结构无关。
  - ADR-0003(`0003-no-secondary-job-matching.md`,已被 0012 取代):明确「不再根据 JD、技术栈…二次筛选」,反而反对基于 JD 结构做前置硬筛。

- **该判据一直是脆弱的、需持续维护**:`requirementsHeading` 的可接受标题清单从 `dce5f5a` 的 4 种(任职要求/岗位要求/职位要求/任职资格)扩展到现在的约 14 种;**当前 `issue-1` 工作树里还有一处未提交改动继续加入「工作要求/招聘要求/岗位需求/职位需求/能力要求…」等变体**(`git diff internal/adapters/boss/job_discovery.go`)。标题清单反复扩张,直接说明「JD 必然含可识别的『要求』标题」这一前提在真实 BOSS 数据上并不稳固。

**动机小结**:拆分服务的具体需求是「用一个可编程判据确认详情页是完整规整的 JD(而非登录页/截断页/占位)」,并顺手把 JD 建成职责/要求两段以便展示与传输。它**不是**由「LLM 鉴定需要预拆分」或任何产品/ADR 决策驱动。

---

## 三、今天哪些环节真正依赖「两段独立」vs 只需「整段 JD」(答问 2)

| 环节 | file:line | 依赖「两段独立」? | 说明 |
|---|---|---|---|
| 发现可靠性门禁 | `job_discovery.go:258-270` | **依赖(作为判据)** | 逻辑上依赖「能切出非空两段」这一**判据**,但本质诉求是「JD 完整」;判据本身脆弱(见上,标题清单持续扩张)。 |
| 领域必填校验 | `discovery/model.go:143` | 依赖(继承自上游) | 只是把两字段列必填;若上游改为单字段,此处等价改为「fullJD 非空」。 |
| 落库 + 去重/JDHash | `pool.go:527-552` | **不依赖** | `jd_hash` = 规范化 `judgmentContent` JSON 的 SHA-256。去重只需内容**确定性规范化**;两键还是一键不影响去重语义,仅是 JSON schema 差异。ADR-0012 对 `jd_hash` 的定义是「规范化后的岗位判断内容」,与是否分段无关。 |
| 落库非空校验 | `pool.go:566` | 依赖(继承) | 同上,可退化为「fullJD 非空」。 |
| 送 Pi 的鉴定输入 | `worker.go:69-70, 275-283` | **不依赖(见第四节)** | 两段仅作为相邻 JSON 键透传给模型;prompt 未对两键做任何分别处理。 |
| Pi 确认工具契约 | `internal/adapters/pi/confirm_assessment_results.ts` | **不依赖** | 回调 schema 只有 `results: Array(Unknown)`,完全不提 responsibilities/requirements。 |
| Web 展示 | `internal/webui/templates/page.html:295-296` | **仅展示用途** | 分两个 `<article>`:「完整工作职责」「完整任职要求」。这是唯一在用户面前区分两段的地方,且纯属排版;单块 JD 也可展示。 |
| 策略评测/优化 | ADR-0022;`internal/assessment`(policy 路径) | **不依赖** | 策略验收只比对人工复核样本,不消费 JD 分段结构。 |

**小结**:真正把「两段独立」用出差异的只有 **Web 展示**(排版,可替代)。发现门禁把「两段非空」当作「JD 完整」的**代理判据**——是逻辑依赖但可被「fullJD 非空 + 其他完整性判据」替代。去重/JDHash、Pi 输入与确认契约、策略评测都**只需要整段 JD 文本**。

---

## 四、「LLM 鉴定需要预拆分 JD」这一前提是否成立(答问 3)

**结论:不成立。把整段 JD 交给 Pi 足以完成鉴定;当前分段对模型判断没有任何被代码利用的语义作用。**

一手证据(`internal/adapters/pi/adapter.go`):

- 送模型的 prompt 由 `assessmentPrompt`(`adapter.go:340-350`)生成,做法是 `json.Marshal(request)` 后整体附在提示语后。提示语原文为:「…只依据下面请求中的**完整在线简历、完整岗位鉴定策略和完整岗位输入**逐项判断…请求 JSON:\n」(`adapter.go:345-348`)。
- 即模型收到的是含 `responsibilities` 与 `requirements` 两个**相邻 JSON 键**的对象;prompt **没有任何**针对这两键的分别指示(不要求「先看职责再看要求」「按要求逐条打分」等)。两键对模型而言等价于一段被人为切成两截的文本。
- 确认回调 `confirm_assessment_results.ts` 的参数 schema 只有 `results: Type.Array(Type.Unknown())`,完全不引用分段。模型产出 `status/reason/evidence` 三态结论,与 JD 是一段还是两段无关。
- ADR-0014 的接口契约本身用的措辞就是「推送…**完整 JD**…」(`0014...md:3`);把两键合并为单一 `fullJD` 字段,与该 ADR 契约完全相容(接口不变:程序推送、单一确认回调、平台岗位为结果所有者)。

因此:若把 `AssessmentJobInput` 的两字段换成单一 `fullJD`,模型得到的是同样的文本信息量(甚至更接近原始 JD,不会因分界正则误切而丢字/错分),鉴定能力不减。「预拆分是鉴定所必需」的前提在代码里找不到支撑。

**唯一需要一并考虑的连带项(不作决策,仅提示 #43)**:去掉拆分意味着发现门禁失去「两段非空」这个现成的「JD 完整」代理判据,需要另立完整性判据(如 `fullJD` 非空 + 长度/占位符检测);`jd_json`/`jd_hash` 的 JSON schema 会变(现存数据的哈希与新计算不一致,涉及迁移);Web 展示需从两块改为单块。这些都是可控的工程连带项,不影响「整段 JD 对 LLM 足够」这一结论。

---

## 附:关键引用索引

- 拆分逻辑:`internal/adapters/boss/job_discovery.go:171, 178-180, 258-270, 581`
- 领域校验:`internal/discovery/model.go:43-44, 143`
- 落库/哈希/非空:`internal/jobpool/pool.go:527-534, 548-552, 566`
- 送 Pi 输入:`internal/assessment/worker.go:69-70, 275-283`
- Pi prompt(整段 JSON):`internal/adapters/pi/adapter.go:340-350`
- Pi 确认契约:`internal/adapters/pi/confirm_assessment_results.ts`
- Web 展示:`internal/webui/templates/page.html:295-296`
- 诊断探针:`internal/adapters/boss/visible_page_probe.go:45-46, 207-210, 300-317`
- 起源提交:`6a27211`、`dce5f5a`(均 issue-4);字段扩散:`184fe30`(issue-5)、`2d081a1`(issue-14)、`3cafb66`(issue-7)、`1428dcb`(issue-10)
- ADR:`docs/adr/0012-…md`、`0014-…md:3`、`0022-…md`、`0003-…md`
- originating issue 验收(「完整 JD」而非「两段」):`gh issue view 4`
