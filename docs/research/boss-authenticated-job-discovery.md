# BOSS 已登录岗位发现与合规外部数据源研究

研究日期：2026-08-31

仓库基线：`issue-36` 的 `a4ae3fca83bf3f1be825e3b72ebc0b3c66cc943b`

对应工单：[R01：调研 BOSS 真实多页岗位读取与 code=37 风控边界](https://github.com/Russell-Utopia/boss-job-agent/issues/36)

## 结论

1. **当前没有足够的一手证据把 BOSS 网页内部请求称为“官方公开 API”。** 本次核验的 BOSS 官方职位页提供面向人的职位搜索入口，页脚列出的企业服务也只有职位搜索、客户端、投资者关系等普通产品入口；没有发现开发者门户、职位列表 API 文档、应用注册或访问凭证申请入口。这只是对已核验公开入口的结论，不是对 BOSS 全部商业合作能力的否定。[BOSS 官方职位页](https://www.zhipin.com/zhaopin/index.html)（访问于 2026-08-31）；[BOSS 联系方式](https://www.zhipin.com/aboutContact)（访问于 2026-08-31）
2. **外部系统的首选路径只能是 BOSS 明确公开或通过合作合同授权的数据接口。** BOSS 官方页面提供的 2019 版用户协议明确要求通过 BOSS 软件使用服务，并说明未经许可不得通过第三方工具获取包括浏览职位在内的服务。因此，登录用户在浏览器里能够看到数据，不等于第三方系统获得了自动采集或转授权权利；该历史版本只能说明本次核验到的公开条款，正式接入仍须确认当前有效协议和书面授权。[BOSS 用户协议第四条第 8 项](https://www.zhipin.com/web/common/protocol/protocol-2019-09-30.html)（页面标注 2019 版，访问于 2026-08-31）
3. **当前仓库实现是一条“已登录浏览器页面上下文中的私有网页接口”实验路径，不是可交付给外部调用方的开放 API。** 它把 `SearchRange + pageNo` 交给 `JobDiscovery.FetchPage`，经本机 Kimi WebBridge 在已登录 Chrome 标签页中执行页面脚本，再把规范化 JSON 字符串交回 Go 校验和解析。[`JobDiscovery` 输入与输出合同](../../internal/discovery/service.go#L20-L88)；[生产 Adapter 的导航、求值和解码](../../internal/adapters/boss/job_discovery.go#L15-L101)（均访问于 2026-08-31）
4. **`code=37` 的触发条件仍是未知，但失败位置现已可观察。** Issue #36 最初只记录了一个相关性：第 1 页曾返回 15 条且有下一页，连续读取岗位详情时随后出现 `code=37` 和安全检查页面。固定 15 秒探针仍返回 `platform_limited`，但当时尚未保留失败位置；加入非敏感诊断字段后的下一次授权探针则明确观察到第 10 个对外请求、即第 7 个岗位详情返回 `code=37`。这能定位首次失败，仍不能单独证明请求频率、调用形态、会话状态、私有接口方式或某个岗位响应是原因。[Issue #36 的 Problem 与 Questions](https://github.com/Russell-Utopia/boss-job-agent/issues/36)（访问于 2026-08-31）
5. **生产 seam 仍应由 `discovery` 模块拥有，但当前“一次返回整页”的 `FetchPage` 粒度过粗。** 稳定恢复需要把它深化为“打开一页并取得稳定 ID 清单”与“在该页逐岗读取详情”两个能力；`discovery.Service` 持久化本页 ID 清单和已完成 ID，Adapter 只持有本次页面的临时技术状态。这样首个限制仍会立即停止，但恢复时不再重放已经成功的详情。页面内部端点、参数、Cookie 或 CDP 类型仍不能泄漏到 interface。[架构设计原则](../architecture.md#L5-L11)（访问于 2026-08-31）
6. **一次固定 15 秒间隔的授权只读探针仍在第 1 页触发 `platform_limited`。** 探针在每个岗位详情之前固定等待 15 秒，约 112.5 秒后失败，没有提交第 1 页，也没有进入第 2、3 页。这能否定“把 750 ms 改成 15 秒即可解决”的简单假设，但仍不能证明具体触发条件，也不能确认本次上游原始码就是 `37`。
7. **加入诊断后的对照探针确认固定 15 秒仍不能通过第 1 页。** 默认 750 ms 探针在第 10 个总请求、即第 7 个详情返回 `code=37`；之后再次明确授权的固定 15 秒探针则在第 9 个总请求、即第 6 个详情返回 `code=37`，耗时约 96.61 秒。延长间隔没有消除限制，失败序号也不固定；两个不同时间和 session 的观测不能证明更长间隔导致更早失败。
8. **两份离线原型已把恢复规则和 DOM 身份规则跑通，但真实稳定性仍未验收。** 逐岗状态机证明旧模型会重放已成功详情，岗位级 checkpoint 只从失败岗位继续；可见 DOM fixture 先后抓到“清洗时丢失 JD 换行”“`textContent` 混入隐藏节点”和“私用区薪资非空但不可读”三类 parser 缺陷。运行后的修复使七组场景全部符合预期，包括隐藏节点排除、只有职责标题的完整可见 JD，以及私用区薪资被清空并标记为不可用而不丢弃岗位。fixture 不连接 BOSS，不能替代获准的 live 对照。
9. **第一次获准的原生 DOM live probe 没有进入详情循环，但发现了可修复的 DOM 就绪问题。** `navigate` 返回后立即查询时得到 `missing_job_cards`；probe 按规则停止，未点击任何岗位。随后在同一页面只读检查发现 URL 已跳转到 `/web/geek/jobs`，17 个 `.job-card-box` 已完成 hydration，当前列表字段为 `.job-salary/.boss-name/.company-location`，详情身份链接为 `.more-job-btn`，右侧具有可见 JD 和未禁用的“立即沟通”按钮。脚本随后加入稳定卡片清单等待并兼容这些真实结构。
10. **第二次获准的修复版 DOM probe 进入了详情循环，但整批仍失败，不能称为已返回 3 个岗位。** 按 `detail_ordinal`，前 3 个岗位在页面脚本内通过卡片/最终详情身份、开放状态和可见 JD 校验；第 4 个岗位的可见 JD 没有满足当时 parser 强制要求的独立“任职要求”分界，脚本在首错停止。停止前未出现终止性的登录失效、验证、平台限制或详情 ID 不一致分类，但 Go 没有收到 `visiblePageProbeResult`，也没有提交或 checkpoint；本轮没有翻页或耗尽证据。运行后本地候选修复改用 `innerText`，并把 `FullJD` 作为事实、把职责/要求拆分作为可选结构分类；该候选随后在第三次受控 probe 中完成 8 个详情，边界见第 12 条和下文。
11. **单次最多 8 岗位的 DOM probe 只是第一阶段，不是“稳定生产读取”证明。** 它可以验证当前页面的 hydration、selector、稳定 ID、开放状态、可见 JD 和首错停止，但脚本会截断超过 8 个的列表，研究结果明确声明耗尽证据不可用，而且 Adapter 尚未接入生产装配。只有固定搜索输入、逐岗可靠结果和持久化 checkpoint、恢复不重放、完整第 1 页、同输入连续第 2/3 页、明确耗尽，以及登录/验证/平台限制首错停止全部通过，才能声称生产读取稳定。[DOM probe 的 8 岗位上限和截断结果](../../internal/adapters/boss/visible_page_probe.js#L140-L192)；[未装配且不能证明耗尽](../../internal/adapters/boss/visible_page_probe.go#L27-L38)；[Issue #36 Acceptance criteria](https://github.com/Russell-Utopia/boss-job-agent/issues/36)（均访问于 2026-08-31）
12. **第三次获准的 `FullJD-first` DOM probe 完成 8 个详情，但仍不是生产成功。** 本次约 11.5 秒返回 `jobs=8 / scanned_cards=15 / truncated=true / exhaustion_evidence=unavailable`，至少一个 JD 被如实标记为 `responsibilities_only`，没有以登录、验证或平台限制终止；这证明当前候选在该次运行中能连续走完前 8 个卡片/详情。随后只读检查发现前 8 项薪资在 `innerText`、`textContent` 与 accessibility tree 中均只有 Unicode 私用区字形，且没有 `aria-label`、`title` 或 `data-*` 正常文本可作为替代证据。**第三次运行时** probe 只校验薪资非空，因此机械 PASS 不等于薪资可读。经领域决策，薪资现为可选判断内容：运行后的本地候选会清空不可读薪资并标记 `unavailable`，保留其它可靠字段完整的岗位；这项候选尚未 live 复验，且固定搜索输入、完整页、多页、checkpoint 与耗尽证据仍未完成。本文不推断该字体呈现的设计目的，也不提供字体逆向或解码方案。

本研究不会给出网页内部接口的端点、参数、请求头、签名、会话材料或重放方法；不会提出隐藏自动化、指纹伪装、验证码处理或平台限制规避方案；也没有访问 BOSS 安全策略文档。所有真实读取均限于下文逐次记录的当次明确授权范围；首个异常即停止，没有自动重试、换身份、换网络或继续翻页。

作为一般合规背景，《网络数据安全管理条例》第十八条要求使用自动化工具访问、收集网络数据时评估对网络服务的影响，不得非法侵入网络或干扰网络服务正常运行。本文只把它作为访问边界的一手依据，不构成法律意见。[国家行政法规库《网络数据安全管理条例》](https://xzfg.moj.gov.cn/front/law/detail?LawID=1734)（访问于 2026-08-31）

## 研究边界与证据等级

| 等级 | 本文如何使用 |
| --- | --- |
| 已证实 | 当前仓库代码、GitHub Issue/commit、BOSS 官方公开页面与协议、Chrome/CDP 和 Fetch 标准明确写出的事实。 |
| 已观察但未证明因果 | Issue #36 记载的一次 BOSS live 读取现象；可以说明“发生过什么”，不能说明“为什么发生”。 |
| 待授权验证 | 真实 BOSS 多页稳定读取、DOM 候选的业务字段可用性、下一页耗尽信号和平台限制恢复行为。 |
| 明确排除 | 绕过登录、验证码、签名、访问控制、安全检查或反自动化措施；复制或重放登录材料；对外暴露网页内部接口细节。 |

## 两条时间线不能混为一谈

### 2026-08-25：代理逐步操作

这是仓库建立前的代理驱动浏览器工作流：代理在用户已登录的浏览器中逐步读取页面、判断下一步并记录进度。当前 Git 仓库没有 2026-08-25 的可执行代码或原始 runlog；最早提交是 2026-08-26 00:05 的产品方案，而且该方案明确说它不定义浏览器控制方式，也把“不绕过验证码、安全验证或平台限制”列为非目标。因此这条时间线只能作为产品形成背景，不能作为程序 Adapter 已完成真实多页验收的证据。[初始提交 `5f513fd`](https://github.com/Russell-Utopia/boss-job-agent/commit/5f513fd39b8de74bd94e390c062ce303a14c60cb)；[初始产品方案的范围与安全边界](https://github.com/Russell-Utopia/boss-job-agent/blob/5f513fd39b8de74bd94e390c062ce303a14c60cb/PRODUCT_PLAN.md#L11-L54)（均访问于 2026-08-31）

### 2026-08-31：Issue #4 程序 Adapter

Issue #4 建立的是可离线验收的程序合同：`discovery.Service` 逐页调用 `JobDiscovery`，整页可靠后才交给 `jobpool.Pool`，只有显式 `HasMore=false` 才完成。[Issue #4 验收标准与交付评论](https://github.com/Russell-Utopia/boss-job-agent/issues/4)；[`discovery.Service` 的逐页推进](../../internal/discovery/service.go#L268-L323)（均访问于 2026-08-31）

该切片同时加入了生产 BOSS Adapter 和显式 `live` 测试入口，但 Issue #4 的交付评论及 Issue #36 都明确说明：生产 Adapter 没有成功完成真实第 2、3 页，默认离线测试不能替代真实多页证据。[Issue #4 交付边界](https://github.com/Russell-Utopia/boss-job-agent/issues/4#issuecomment-5473554501)；[最多三页的显式 live 测试](../../internal/adapters/boss/job_discovery_live_test.go#L13-L55)（均访问于 2026-08-31）

所以两条时间线的证据含义不同：

| 时间 | 执行者与控制方式 | 可证明内容 | 不能证明内容 |
| --- | --- | --- | --- |
| 2026-08-25 | 代理按页面状态逐步操作，人工工作流语义 | 产品交互与停止边界的历史来源 | 当前 Go Adapter、同一输入连续三页、明确耗尽 |
| 2026-08-31 | Go `JobDiscovery` 经 WebBridge 驱动一个发现专属浏览器 session | 输入/输出合同、离线解析、错误分类、显式 live 测试形状 | 真实第 2、3 页稳定性、`code=37` 原因、长期接口支持性 |

## 三类访问形态

| 形态 | 判定标准 | 本次核验结果 |
| --- | --- | --- |
| 公开/授权 API | BOSS 官方提供开发者文档、认证方式、字段/分页合同、限额与应用或合作方授权 | 已核验公开入口中未发现；应通过 BOSS 官方联系方式继续确认商业合作能力，不能用网页内部请求替代。 |
| 浏览器人工可见页面 | 求职者通过 BOSS 官方 Web/客户端看见并手工操作的职位搜索、列表和详情 | 官方页面明确存在职位搜索入口；“人工可见”只描述产品事实，不自动授予第三方采集权。[BOSS 官方职位页](https://www.zhipin.com/zhaopin/index.html)（访问于 2026-08-31） |
| 内部未公开接口 | BOSS 自身网页为实现页面功能而调用、但没有对外开发者合同的接口 | 当前程序 Adapter 使用这一形态；它是实现细节，不是公开 API，也不应向外部调用方暴露。 |

## 当前链路的数据流

### 1. `discovery` 模块输入

输入是：

```text
SearchRange {
  role,
  city,
  salary,
  employmentType
}
+ pageNo（正整数）
```

输出必须是：

```text
DiscoveryPage {
  observations: [
    {
      platformJobId,
      canonicalUrl,
      jobTitle,
      companyName,
      city,
      salary,
      responsibilities,
      requirements,
      platformStatus
    }
  ],
  hasMore: boolean
}
```

这组类型由 `discovery` 模块拥有，调用方不需要知道 WebBridge、CDP 或网页传输形状。[`SearchRange`、`JobObservation`、`DiscoveryPage`](../../internal/discovery/service.go#L30-L88)（访问于 2026-08-31）

### 2. Go 到 Kimi WebBridge

生产 Adapter 检查搜索输入，找到或打开 BOSS 搜索标签页，然后把一段异步页面求值任务交给本机 WebBridge。WebBridge 命令的仓库侧合同只有 `action + args + session`，返回 `ok/data/error`；Go 对响应大小、HTTP 状态和 JSON envelope 做校验。[`webBridge` 命令与响应合同](../../internal/adapters/boss/webbridge.go#L52-L96)；[WebBridge 响应校验](../../internal/adapters/boss/webbridge.go#L99-L170)（均访问于 2026-08-31）

发现 Worker 与打招呼 Worker 分别拥有自己的 Adapter/browser session，但 session 不暴露给业务 interface；仓库 ADR 明确把登录、验证和平台限制留给程序统一协调。[ADR-0010](../adr/0010-one-browser-session-per-worker.md#L1-L3)（访问于 2026-08-31）

### 3. WebBridge/CDP 到页面上下文

仓库发出的动作名是 `evaluate`，期望一个字符串结果。Chrome DevTools Protocol 的 `Runtime.evaluate` 定义为在目标执行上下文的全局对象上执行表达式；未指定上下文时使用被检查页面的上下文，并可等待 Promise、按值返回结果。Chrome 的扩展调试接口也说明，附着到标签页后可以向该 target 发送包括 Runtime、DOM 和 Network 在内的 CDP 命令。[CDP `Runtime.evaluate`](https://chromedevtools.github.io/devtools-protocol/tot/Runtime/#method-evaluate)；[Chrome `debugger` 扩展接口](https://developer.chrome.com/docs/extensions/reference/api/debugger)（均访问于 2026-08-31）

这解释了当前链路为何能够在已登录页面中发起同站读取：请求由页面执行上下文交给浏览器处理，而不是由 Go 导出、保存或重放登录材料。Fetch 标准规定 request 有 `omit | same-origin | include` 三种 credentials mode，并由该模式控制请求凭证流动；这只是浏览器机制说明，不构成 BOSS 对第三方自动化的授权。[WHATWG Fetch Standard](https://fetch.spec.whatwg.org/#concept-request-credentials-mode)（访问于 2026-08-31）

### 4. 页面结果回到 Go

页面任务返回一个只含规范化字段的 JSON 字符串。Go 要求：

- 顶层必须同时存在岗位数组和显式 `hasMore`；
- 列表稳定 ID 与详情证据中的稳定 ID 一致；
- 平台状态证据可靠；
- JD 能拆分成非空职责与要求；
- 任一岗位不可靠时整页失败。

这些检查集中在生产 Adapter 与 `discovery.ValidatePage`，调用者不会接触原始网页响应。[Adapter 的规范化结果与可靠性检查](../../internal/adapters/boss/job_discovery.go#L104-L190)；[`ValidatePage` 整页合同](../../internal/discovery/service.go#L376-L410)（均访问于 2026-08-31）

## 当前私有接口 + 逐岗详情模式：知道什么，不知道什么

### 已证实

- 当前实现先取得一页列表，再对列表中的每个岗位读取详情，以补全稳定 ID、开放状态与完整 JD；Issue #36 把它称为当前“列表接口 + 每岗详情接口”路径。[Issue #36](https://github.com/Russell-Utopia/boss-job-agent/issues/36)（访问于 2026-08-31）
- 当前一次 `FetchPage` 固定先读取城市元数据、筛选条件和岗位列表，再为列表中的每个岗位读取一次详情；当前页大小上限为 15，所以一次业务调用会放大为最多 18 次页面上下文请求。这是代码可确定的请求放大，不是 `code=37` 的已证实原因。[生产 Adapter 源码](../../internal/adapters/boss/job_discovery.go)（访问于 2026-08-31）
- 当前固定等待只发生在同页第二个及以后的详情读取之前，值为 750 ms；第一页结束到下一页开始之间没有额外等待。这个数值没有官方合同依据，不能解释为“安全频率”。[生产 Adapter 源码](../../internal/adapters/boss/job_discovery.go)（访问于 2026-08-31）
- 任一详情读取失败都会终止页面脚本，整页不会交给 `jobpool.Pool`；Adapter 没有逐岗详情 checkpoint，所以从同一页重试时会重新读取该页先前已经成功的详情。请求放大和重复读取是确定的工程缺陷。[整页成功后才提交与推进](../../internal/discovery/service.go#L268-L345)（访问于 2026-08-31）
- 研究基线脚本把多种上游返回统一折叠成 `platform_limited`，没有把原始上游 code、失败阶段或详情序号带回 Go；页面 URL 出现 `_security_check=1` 时也没有独立分类分支。因此，该次 live 探针留下的 runlog 无法回答“第几个详情、哪个阶段、哪个上游 code 首先失败”。后续诊断补强见下文。[生产 Adapter 源码](../../internal/adapters/boss/job_discovery.go)（访问于 2026-08-31）
- 离线 fixture 可以证明 Go 的动作顺序是查找标签页、导航、求值，并能把返回 JSON 解析成 `DiscoveryPage`；fixture 还证明不可靠岗位会使整页失败。[生产 Adapter 离线测试](../../internal/adapters/boss/job_discovery_test.go#L15-L67)；[整页失败测试](../../internal/adapters/boss/job_discovery_test.go#L79-L160)（均访问于 2026-08-31）
- Issue #36 记载第 1 页曾有 15 条岗位且 `hasMore=true`，并记载随后发生 `code=37` 与安全检查页面；这只是一段观察记录。[Issue #36](https://github.com/Russell-Utopia/boss-job-agent/issues/36)（访问于 2026-08-31）

### 未证实

- `code=37` 与频率、逐岗详情数量、请求间隔、某个岗位、会话时长或网页私有接口之间的因果关系；
- 当前私有接口是否有稳定 schema、分页保证、限额说明、变更通知或第三方使用许可；
- `hasMore=true` 后真实第 2、3 页能否在同一输入下连续可靠返回；
- 页面遇到平台限制后，何时以及以什么授权方式可以恢复；
- 当前代码中的固定等待时间是否符合平台规则。固定延时不是授权，也不是可靠限速合同。

因此，**请求放大和研究基线中的诊断证据丢失是已确定缺陷，`code=37` 的根因仍未知**。研究结论应写成 `unknown trigger / platform_limited`，而不是“调慢即可解决”或“某个参数导致”。

在任何下一次获准的诊断中，Adapter 应保留以下**非敏感**字段：`request_ordinal`、`upstream_code`、`stage`（如 page/list/detail）和从 1 开始的 `detail_ordinal`。这些字段只定位首个失败，不保存原始响应。Cookie、request headers、`securityId`、个人简历正文和任何可重放会话材料都禁止进入返回值或日志。

## 固定 15 秒详情间隔的 live 结果

2026-08-31，求职者明确同意尝试“每个详情停顿 15 到 20 秒”。为了只改变一个变量，实际探针采用**固定 15 秒**，没有使用随机区间；研究构造器在第一个及之后每个岗位详情之前都等待 15 秒，生产默认构造器仍保持原来的 750 ms 行为。

固定输入为：

```text
role=Golang
city=厦门
salary=18-20K
employmentType=全职
```

第一次启动在查找隔离 session 的 BOSS 标签页时由本机 WebBridge 返回 HTTP 502，尚未进入 BOSS 页面读取，因此不计为平台实验结果。按 WebBridge 正常流程在同一隔离 session 打开官方 BOSS 搜索页后，只重跑一次：

```text
详情间隔：固定 15 秒，首个详情前也等待
第 1 页结果：platform_limited
失败时间：约 112.5 秒
第 1 页提交：否
第 2、3 页：未读取
```

该次探针使用的 Adapter 在页面上下文中把多个上游返回和页面限制信号统一折叠成 `BOSS_PLATFORM_LIMITED`，所以历史结果不能回答原始上游 code、失败阶段和 `detail_ordinal`。它只证明：在这次浏览器 session 和固定输入下，15 秒间隔仍不足以让第 1 页成为可靠 `DiscoveryPage`。这不证明频率与限制无关，也不证明某个请求参数、岗位或会话状态是原因。

探针在首个 `platform_limited` 后立即停止，没有尝试 20 秒、随机延时、换 session、刷新、自动重试或继续下一页；随后关闭了本次隔离 session。输出未包含 Cookie、请求头、`securityId`、简历正文或原始响应。

## 后续诊断补强

固定 15 秒探针结束后，Adapter 增加了首个失败请求的非敏感定位证据，但没有再次连接 BOSS：

- `request_ordinal` 在每次 `FetchPage` 开始时归零；城市元数据、筛选条件和岗位列表依次是第 1、2、3 个请求，第 N 个岗位详情是第 `3 + N` 个请求；
- `stage` 区分页面预检、城市元数据、筛选条件、岗位列表、岗位详情及其本地校验；
- `detail_ordinal` 表示当前是列表中的第几个岗位详情，非详情阶段为 0；
- `upstream_code` 只接受短数字码；不保存 URL、查询参数、请求头、响应正文或会话材料；
- `discovery.FetchError` 将证据带回 Service，失败的 `external_attempt_finished` runlog 以结构化字段保存；整页失败和 checkpoint 不推进的行为不变。

例如第 4 个岗位详情返回上游码 `37` 时，finished runlog 将记录 `request_ordinal=7`、`stage=job_detail`、`detail_ordinal=4`、`upstream_code=37`。离线测试已覆盖页面错误解析、Service 到 runlog 的映射和 JSONL 字段持久化。

随后一次明确授权的只读 live 探针保持生产默认 750 ms 详情间隔和相同搜索输入，在约 6.88 秒后得到：

```text
request_ordinal=10
stage=job_detail
detail_ordinal=7
upstream_code=37
```

因此，这次失败发生在第 1 页的第 7 个岗位详情请求。探针在首个限制后停止，没有重试或进入下一页，并关闭了隔离浏览器会话。该结果证明诊断链路可用，但不是 `code=37` 根因的因果实验。

用户随后再次明确授权固定 15 秒对照。探针在首个及之后每个详情前均等待 15 秒，不使用随机延时，得到：

```text
request_ordinal=9
stage=job_detail
detail_ordinal=6
upstream_code=37
```

该探针约 96.61 秒后在第 1 页停止，没有重试或进入下一页，并关闭了隔离浏览器会话。与默认 750 ms 的结果并列后，只能得出“固定 15 秒不足以解除限制、失败位置并非稳定的第 7 个详情”；不能据此归因于时间间隔或第 6 个岗位本身。随机延时没有执行。

## 根因假设与可证伪预测

以下假设按当前证据排序；它们不是结论。下一次获准的 live 对照只改变“详情读取路径”一个变量，不继续搜索延时间隔：

| 排名 | 假设 | 当前支持 | 可证伪预测 |
| --- | --- | --- | --- |
| 1 | 账号、session 或网络出口存在较长时间窗的累计平台状态 | 750 ms 与 15 s 在不同位置都失败；延时没有恢复 | 关闭标签页或新开隔离 session 后仍可能在相近或更早位置失败；较长时间后状态可能变化 |
| 2 | 私有详情接口的累计调用形态比列表/页面读取更敏感 | 两次可定位失败都发生在 `job_detail`，不是前三个列表准备请求 | 同一输入下，原生页面逐岗点击若能明显超过 6–7 个，而私有详情路径仍失败，则该假设增强 |
| 3 | 当前“一次 `FetchPage` 连读全部详情”本身造成不必要的请求放大 | 每页最多 18 个请求；失败恢复会重放已成功详情；首次 DOM probe 在任何详情点击前停止，证明 DOM 路径可以独立诊断 | 加入岗位级 checkpoint 后，即使同一失败仍发生，恢复后的重复详情数应从 N 降为最多 1 |
| 4 | 某个岗位的参数或内容触发失败 | 两次失败序号不同，当前不支持固定序号 | 记录脱敏岗位 fingerprint；若不同运行总在同一 fingerprint 失败，该假设增强，否则减弱 |
| 5 | 页面后台流量或同账号其他活动贡献了平台可见请求数 | 脚本的 `request_ordinal` 只统计自身 `fetch` | 若浏览器网络审计中的同域总请求显著多于脚本序号，则需要把背景流量纳入解释 |

无论哪一个假设成立，平台限制、登录失效、验证码或安全检查都仍是立即停止状态；实验不通过更换身份、网络、指纹、会话材料或随机节奏来规避。

## 三种候选路径

| 候选 | 输入 -> 输出 | 优点 | 主要缺口 | 结论 |
| --- | --- | --- | --- | --- |
| 页面上下文私有接口 | `SearchRange + pageNo` -> 私有 JSON -> `DiscoveryPage` | 当前最容易取得结构化分页、稳定 ID 和详情字段 | 未发现公开文档或第三方许可；逐岗详情放大调用量；已观察到平台限制；schema/限额不可依赖 | **不作为长期生产路径**。只有 BOSS 书面授权并给出正式合同后，才把授权合同实现为 Adapter。 |
| 原生 DOM/CDP | 已登录搜索/详情页面 -> DOM/可见文本 -> `DiscoveryPage` | 只解释浏览器实际呈现给求职者的内容；不依赖网页内部 JSON schema | DOM、虚拟列表和文案会变；稳定 ID、完整 JD、开放状态、显式耗尽不一定都可见；第三方自动化仍需许可 | **可做离线 parser 原型**。真实运行仍要求求职者当次授权和 BOSS 许可。 |
| 混合可见页面 | 搜索页 DOM 取得卡片与分页 -> 逐个打开可见详情页读 DOM -> 合并 `DiscoveryPage` | 保留页面级证据，避免调用未公开数据接口；列表与详情身份可相互核对 | 浏览器操作多、慢；仍可能触发平台限制；耗尽和虚拟列表证据复杂；仍不是官方 API | **取得许可但没有官方数据 API 时的首选浏览器候选**；首个限制即停止，不能用节流技巧规避。 |

CDP 的 DOM domain 可读取 root DOM、查询节点与读取节点属性；DOMSnapshot 可取得包含 DOM/layout 的快照，因此原生 DOM 路径在技术上可做。但 tip-of-tree CDP 本身明确不保证向后兼容，生产实现应固定并验证实际 Chrome 版本，而不是依赖最新协议偶然可用。[CDP DOM](https://chromedevtools.github.io/devtools-protocol/tot/DOM/)；[CDP DOMSnapshot](https://chromedevtools.github.io/devtools-protocol/tot/DOMSnapshot/)；[CDP 版本说明](https://chromedevtools.github.io/devtools-protocol/)（均访问于 2026-08-31）

## 推荐的生产 seam 与安全架构

### 把整页方法深化为可恢复的页读取器

```go
type JobDiscovery interface {
    OpenPage(context.Context, SearchRange, int) (DiscoveryPageReader, error)
}

type DiscoveryPageReader interface {
    StableJobIDs() []string
    HasMore() bool
    ReadJob(context.Context, string) (JobObservation, error)
    Close() error
}
```

`OpenPage` 在一次获准动作中取得有序稳定 ID 清单和显式 `HasMore`，返回的 reader 只保存当前页的临时浏览器/授权接口状态。`discovery.Service` 把 `page_no + ordered_stable_ids + has_more + completed_stable_ids` 保存为技术恢复 checkpoint，然后按顺序调用 `ReadJob`：每个可靠岗位先交给 `JobPool.Observe`，再推进已完成 ID。若两步之间崩溃，最多重读当前一个岗位；绝不先推进 checkpoint 再漏写岗位。全部 ID 完成后才递增页码或在 `HasMore=false` 时完成运行。

恢复时重新 `OpenPage` 并核对有序稳定 ID 清单；清单发生变化时返回 `invalid_response` 等待人工处理，不能把新清单与旧 checkpoint 静默拼接。页面端点、参数、headers、cookies、session、`securityId` 或 CDP 类型都不进入 interface 或持久化结构。

这是对现有深 seam 的粒度调整，而不是增加通用 Repository 或让 Adapter 持有业务状态。改动会涉及 forward-only SQLite migration、`discovery.Service` 恢复入口、生产与受控 Adapter 以及 ADR-0011/0016，已超出单个研究票中的“小 replacement”；本研究只给出已验证合同，不在 Issue #36 直接实施整套迁移。

推荐 Adapter 优先级：

1. `AuthorizedJobDiscovery`：实现 BOSS 公开或合同授权的数据源；认证、限额、游标、字段与变更通知完全按官方合同。
2. `VisiblePageJobDiscovery`：只有在 BOSS 明确许可浏览器自动读取时使用；搜索与详情均只从可见页面/DOM 取证。
3. 现有私有接口 Adapter：迁移期间只用于问题复现，不继续扩展为长期生产依赖，也不对外暴露；迁移完成后删除。

### 速率、缓存与去重

- **速率**：只接受官方/合同给出的 quota、backoff 与并发上限；没有合同时不猜“安全 QPS”。Adapter 每次只做一次尝试，Worker 决定是否重试。现有 ADR-0011 允许生产 Worker 对 `transient` 做最多三次的有界自动尝试，但 `authentication_expired`、`verification_required`、`platform_limited` 不自动重试；为了保存首个失败证据，Issue #36 的研究 live 反馈环比生产规则更严格，任何失败都停止且不自动重试。[ADR-0011](../adr/0011-require-explicit-search-exhaustion.md#L9)（访问于 2026-08-31）
- **恢复 checkpoint**：只保存页码、有序稳定岗位 ID、显式 `hasMore` 与已完成稳定 ID，不保存原始卡片、JD、页面 HTML、内部请求参数或会话材料。它只用于避免重读详情，不是“某次运行拥有这些岗位”的业务关系。
- **缓存**：若授权合同允许，只缓存规范化岗位观察；key 至少包含 `source + normalized search range + page/cursor + stable_job_id + adapter version`。缓存不能把旧结果当作新的开放状态证据，也不能把缓存命中当成新一次 live 验收。
- **去重**：只按数据源给出的稳定平台岗位 ID 去重；没有稳定 ID 的岗位整页失败，不用岗位名、公司名或 JD 文本猜 ID。仓库已有全局稳定 ID 去重测试。[稳定 ID 去重测试](../../internal/discovery/service_test.go#L83-L149)（访问于 2026-08-31）
- **最小数据**：保存岗位判断真正需要的规范化字段，不保存招聘者联系方式、页面 HTML、原始响应、登录材料或可重放请求。

### 审计

每次 `OpenPage` 和 `ReadJob` 分别写一对 started/finished 事件，至少记录：

- `trace_id`、`discovery_run_id`、`attempt_no`；
- `adapter_kind`、`adapter_version`、授权合同/配置版本的非敏感标识；
- 搜索输入的规范化值或不可逆 fingerprint、`page_no`；
- 页读取记录 `jobs_count`、`new_stable_ids`、`total_stable_ids`、显式 `has_more`；详情读取记录目标稳定 ID 的不可逆 fingerprint 与本页序号；
- 成功或稳定错误分类、是否来自缓存；失败时另记非敏感 `upstream_code + stage + detail_ordinal`，使第一个失败点可复核；
- 不记录原始请求/响应、登录材料、个人简历正文或内部接口细节。

### 失败分类与停止边界

沿用当前稳定分类：`transient`、`authentication_expired`、`verification_required`、`platform_limited`、`invalid_response`、`invalid_protocol`。[`FetchErrorCategory`](../../internal/discovery/service.go#L67-L89)（访问于 2026-08-31）

| 分类 | 当前页结果 | 自动动作 | 人工恢复 |
| --- | --- | --- | --- |
| `authentication_expired` | 保留已可靠写入的岗位和岗位级 checkpoint；页码不推进 | 立即停止该 BOSS Adapter | 求职者在官方页面重新登录，确认成功后显式继续原运行 |
| `verification_required` | 保留已可靠写入的岗位和岗位级 checkpoint；页码不推进 | 立即停止；不轮询、不代答 | 求职者在官方页面自行完成；显式继续前重新检查页面状态 |
| `platform_limited` | 保留已可靠写入的岗位和岗位级 checkpoint；页码不推进 | 首个限制立即停止；不自动退避重试 | 展示限制证据；只有平台允许、用户确认后才从失败岗位继续 |
| `invalid_response` | 当前岗位不写入；页码不推进 | 停止并报告哪个业务字段缺证据 | 修复 parser/合同并离线回归；不得以缺字段结果推进 |
| `invalid_protocol` | 当前岗位不写入；页码不推进 | 停止 Adapter | 修复 WebBridge/CDP/授权接口合同后离线回归 |
| `transient` | 当前岗位不写入；页码不推进 | 正式生产按 ADR-0011/授权合同有界重试；Issue #36 研究探针不自动重试 | 用户可从当前岗位显式继续 |

任何失败都不能覆盖最近一次可靠平台岗位状态。岗位级 checkpoint 可以在单个可靠岗位落库后推进，但页码只有在本页清单全部完成后才能推进；只有授权数据源的明确 end-of-list 或浏览器页面可复核的等价证据才能标记耗尽。

## 不连接 BOSS 的最小原型与验收

### 原型 A：逐岗恢复状态机（已完成）

[逐岗恢复状态机](../../throwaway-prototypes/boss-native-dom-read-recovery.html)是单文件纯 reducer 原型，覆盖旧整页重试、逐岗 checkpoint 恢复、DOM 卡片消失与显式耗尽四条路径。旧模型在第 6 个详情失败后重试会重放前 5 个详情；新模型保留前 5 个已完成 ID，从第 6 个继续。DOM 卡片消失时进入阻塞，不跳过；完整页面且 `hasMore=false` 时才完成。

### 原型 B：可见页面 DOM parser（已完成）

[可见页面 DOM reader](../../throwaway-prototypes/boss-visible-page-dom-reader.html)保存七组脱敏、本地构造的搜索卡片和详情 DOM。它不再复制 parser，而是直接加载研究 probe 嵌入的同一份 [`visible_page_probe.js`](../../internal/adapters/boss/visible_page_probe.js)。首次运行发现通用 whitespace 规范化会丢失 JD 换行；第二次 live 之后又证明 `textContent` 会把隐藏样式/干扰节点混入 JD；第三次 live 后加入私用区薪资不可读的场景。当前脚本只取浏览器实际渲染的 `innerText`，并把真实页面类名、`.more-job-btn` 详情身份、“立即沟通”开放证据、隐藏节点、职责单段 JD 和不可读薪资纳入 fixture。应用内浏览器离线重跑得到：

```text
目标 job-a + 详情 job-a + 招聘中 + 完整 JD -> PASS
目标 job-b + 残留详情 job-a                   -> BLOCK detail_identity_mismatch
目标 job-c + 缺招聘状态/可靠 JD              -> BLOCK missing_open_status
真实结构 job-d + 匹配详情 + 立即沟通 + JD      -> PASS
隐藏节点 job-e + 可见结构化 JD                -> PASS，隐藏文本未进入 fullJD
职责单段 job-f + 匹配详情 + 立即沟通           -> PASS，保留完整可见 fullJD
私用区薪资 job-g                             -> PASS，salary="" / salaryEvidence=unavailable
fixture_result                               -> pass (7/7)
```

离线 fixture 证明，目标详情链接中的稳定 ID、明确开放证据和完整可见 JD 三项缺一不可；详情区域只有内容不能证明它属于当前岗位。薪资若存在必须可读而不只是非空；不可可靠读取时只能以空值和显式 `unavailable` 证据进入后续处理，不能保存字形编码。职责/要求标题是派生结构，不应反过来决定原始可见 JD 是否存在。该 fixture 不连接 BOSS，也没有验证翻页/耗尽。

### 原型 C：显式 live 的原生 DOM probe（已执行三次受控诊断）

仓库新增未接入应用装配的研究 Adapter [`visible_page_probe.go`](../../internal/adapters/boss/visible_page_probe.go)及独立 live test。它具有以下硬边界：

- 目标必须是 `https://www.zhipin.com/web/geek/job` 的用户可见页面 URL；
- 固定最多读取 8 个当前 DOM 岗位卡片，严格串行点击；
- 只读取 `.job-card-box`、详情身份、招聘状态与可见 JD，不包含 `fetch()`、`/wapi/`、`securityId`、credentials 或其他私有请求材料；
- 登录失效、`_security_check=1`、验证码、平台限制、详情错配或缺身份/开放状态/可见 JD 时第一次即停止；不可读薪资被清空并标记为不可用，不单独终止岗位读取；
- 明确返回 `exhaustionEvidence=unavailable`，不把“当前没看到更多”冒充 `hasMore=false`；
- 默认测试跳过，只有 `live` build tag、`BOSS_VISIBLE_PAGE_PROBE_LIVE=1` 和显式 `BOSS_VISIBLE_PAGE_URL` 同时存在才会连接浏览器。

每次获得新的明确授权后，唯一执行入口是：

```bash
BOSS_VISIBLE_PAGE_URL='<获准的用户可见搜索页 URL>' make live-visible-job-discovery
```

单元测试还扫描最终交给 WebBridge 的脚本，拒绝任何私有请求材料；带 `live` tag 但没有开关时验证为 `SKIP`。

2026-08-31 获得一次明确授权后，使用官方职位页 URL 执行该命令。页面从 `/web/geek/job` 跳转到 `/web/geek/jobs`；`navigate` 返回后脚本立即查询列表，得到：

```text
BOSS_VISIBLE_PAGE_UNRELIABLE:missing_job_cards
stage=job_list_dom
detail_ordinal=0
```

probe 在第一个异常后终止，没有点击岗位或进入详情循环。随后只读 snapshot 已显示登录有效、17 个可见岗位卡片和右侧详情；结构检查进一步证明 `.job-card-box` 仍存在，因此失败不是 selector 删除，而是 Vue hydration 尚未完成。真实结构还显示：

```text
列表稳定 ID       a.job-name[href*="/job_detail/"]
薪资              .job-salary
公司              .boss-name
地点              .company-location
详情稳定 ID       .more-job-btn[href*="/job_detail/"]
开放证据          未禁用的 .op-btn-chat，文本包含“立即沟通”
JD                .job-detail-body .desc
当前卡片/详情 ID  相同
```

脚本随后只在本地修改为：等待稳定卡片 ID 清单连续两次一致；支持上述列表字段；严格用 `.more-job-btn` 核对详情 ID；把未禁用的“立即沟通”作为可沟通证据。更新后的真实结构 fixture 当时为 4/4，隔离浏览器 session 已关闭。

随后取得新的当次授权，运行修复版最多 8 岗位 probe。按页面脚本的 `detail_ordinal`，前 3 个岗位的卡片身份、最终详情身份、开放状态和 JD 读取均完成；第 4 个岗位停止于：

```text
BOSS_VISIBLE_PAGE_UNRELIABLE:ambiguous_jd_boundary
stage=job_detail
detail_ordinal=4
```

该错误发生前没有终止性的登录失效、验证、平台限制或详情 ID 不一致分类。由于 `run()` 只在整个循环完成后返回 JSON，首错使 Go 整次失败：前三个岗位没有作为结果返回、没有写入、没有 checkpoint，也没有进入第 2 页或取得耗尽证据。它只能证明 DOM 路径在该次运行中进入并推进了前三个详情，不能证明比私有接口更稳定，也不能与此前第 6/7 个详情的 `code=37` 直接比较。

失败后对当前第 4 个详情做同节点只读检查，`textContent` 含隐藏样式/干扰节点，而 `innerText` 只包含浏览器实际呈现的一行 JD；该行有“岗位职责”标题，没有独立可识别的“任职要求”标题。不能据此声称岗位没有要求信息，因为相关内容可能混在非结构化正文中。运行后候选修复做了两项本地变更：

- 只使用 `innerText` 读取可见字段，隐藏节点不再污染 JD；
- 研究结果以非空 `FullJD` 为事实，派生 `explicit_split / responsibilities_only / requirements_only / unstructured`，不复制或发明缺失段落。

`visiblePageProbeResult` 因而不再冒充生产 `discovery.JobObservation`；生产合同仍要求职责与要求均非空，是否采用 `FullJD-first` 需要独立实现票作领域决策。第三次 live 后，同源 fixture 已加入私用区薪资场景并升级为 7/7；Go 单元测试覆盖四种 JD 结构，以及不可读薪资清空后岗位继续输出、伪称可读的私用区文本仍被 Go 防线拒绝。

第三次取得新的当次授权后，执行 `FullJD-first` 版本的最多 8 岗位 probe。live test 约 11.5 秒完成并打印：

```text
jobs=8
scanned_cards=15
truncated=true
exhaustion_evidence=unavailable
JD structure samples include responsibilities_only
```

该次没有以登录失效、验证或平台限制分类终止；8 个结果的卡片/详情稳定 ID、开放状态和非空可见 `FullJD` 均通过研究 probe 当前合同。这说明修复后的流程在该次运行中连续完成了前 8 个详情，也证明 `responsibilities_only` 可以保留完整可见正文而不虚构要求段落。它仍只覆盖 15 个稳定卡片中的前 8 个，`truncated=true` 且没有耗尽证据，也没有把逐岗结果持久化或推进 checkpoint。[显式 live 门禁与脱敏输出形状](../../internal/adapters/boss/visible_page_probe_live_test.go#L14-L49)；[8 岗位截断与结果字段](../../internal/adapters/boss/visible_page_probe.js#L133-L186)（均访问于 2026-08-31）

随后对薪资节点作同页只读核对，前 8 项均得到相同性质的不可读文本：

```text
innerText / textContent / accessibility tree -> Unicode 私用区字形，例如“-K”
aria-label / title / data-*               -> 没有可用的正常文本
computed fontFamily                       -> kanzhun-mix, kanzhun-Regular
```

这些事实不说明字体设计目的，也不支持任何逆向或解码方案；它们只说明该次 DOM 证据不能可靠恢复求职者可读的薪资字符串。**第三次运行时**页面脚本和 Go decoder 都只检查 `salary` 去空白后非空，所以私用区字形也会通过；因此该次 `PASS` 是**流程连续性 PASS**，不是**业务字段可靠性 PASS**。

第三次运行后的本地候选已按 ADR-0026 把薪资改为可选：

- JavaScript 在 `readCard` 中检测空文本和 Unicode 私用区；可靠文本输出 `salaryEvidence=readable`，否则清空薪资并输出 `salaryEvidence=unavailable`；
- hydration 等待只用 `cardIdentity` 建立稳定 ID 签名，不再用薪资等业务字段决定页面是否“尚未就绪”；
- Go decoder 校验薪资文本和证据组合：`readable` 必须有非空且不含私用区的文本，`unavailable` 必须是空值，防止页面侧异常结果穿透；
- `discovery` 与 `jobpool` 允许空薪资；`jd_hash` 仍包含规范化的可选薪资，所以后来取得可靠薪资会形成判断内容变化；
- HTML fixture 为 7/7，Go 单元测试覆盖岗位保留、数据库读回空值、薪资出现导致哈希变化和伪称可读结果拒绝。

这些都是第三次运行后的本地证据，**尚未 live 复验**；它们能防止不可读字形被保存为薪资，但不能把私用区字形变成可用薪资。[ADR-0026](../adr/0026-allow-unavailable-job-salary.md)；[页面侧薪资证据](../../internal/adapters/boss/visible_page_probe.js)；[Go 侧证据校验](../../internal/adapters/boss/visible_page_probe.go)；[岗位池可选薪资测试](../../internal/jobpool/pool_test.go)；[HTML 7/7 fixture](../../throwaway-prototypes/boss-visible-page-dom-reader.html)（均访问于 2026-08-31）

### 离线验收清单

- [ ] 同一 `SearchRange` fingerprint 连续调用第 1、2、3 页；fixture 中第 3 页给出明确耗尽。
- [ ] 每页记录岗位数、新增稳定 ID、累计稳定 ID 与 `hasMore`；重复 ID 不新增全局岗位。
- [x] 详情身份不一致、缺可靠状态或缺可见完整 JD 时不输出岗位；职责/要求无法明确拆分时保留 `FullJD` 并标记结构，不伪造缺段。
- [x] 平台限制后恢复只从当前岗位继续的纯状态机规则；旧整页模型的重复详情数可见。
- [x] 原生 DOM probe 与 HTML fixture 共用同一份脚本；默认离线且显式 live 门禁有效。
- [x] 一次真实页面就绪诊断确认 17 个卡片和字段 selector；第二次受控 probe 在页面脚本内推进前三个详情并于第 4 个结构边界错误首错停止；第三次 `FullJD-first` probe 连续返回前 8 个详情，`scanned_cards=15 / truncated=true / exhaustion_evidence=unavailable`。
- [x] JavaScript 将私用区薪资清空并标记 `unavailable`，Go 校验证据组合；hydration 身份等待和业务字段读取已拆开，HTML fixture 为 7/7，Go 回归测试通过。
- [ ] 上述可选薪资修复尚未 live 复验；原生 DOM 路径仍没有可读且可复核的薪资证据，但这不再单独阻止可靠岗位保存。
- [ ] 第 2 页注入六类错误，证明三类人工错误不重试，且所有错误都保留原页 checkpoint。
- [ ] 只有显式 `hasMore=false`/官方 end cursor 才完成；“空 DOM”“连续无新增”“滚动次数”单独出现都不能完成。
- [ ] 缓存 key 包含 source、完整搜索输入、page/cursor 与 adapter version；缓存结果不会刷新 `observed_at`。
- [ ] 日志扫描证明没有登录材料、原始响应、完整页面 HTML、个人简历正文或内部接口细节。
- [ ] 默认 `go test ./...` 与 CI 完全离线；live 测试继续由独立 build tag 和显式开关保护。仓库当前 live 测试已经使用独立 build tag 和环境开关。[live 测试门禁](../../internal/adapters/boss/job_discovery_live_test.go#L1-L29)（访问于 2026-08-31）

当前已完成“逐岗恢复语义”“严格 DOM 身份”“FullJD-first 研究归一化”和“私用区薪资可选且不保存乱码”的离线规则，第三次 probe 也完成了 8 个详情的流程连续性验证；但可选薪资候选尚未 live 复验，且固定搜索输入、完整第 1 页、逐岗持久化、多页同输入、稳定 ID 去重、明确耗尽和日志脱敏仍未完成。8 岗位 live PASS 与 7/7 离线 fixture 都不等于生产 Adapter 已稳定。

## 将来取得明确 live 授权后的最小反馈环

### 前置授权

同时满足：

1. 求职者对本次只读探针给出当次明确授权；
2. BOSS 对所选访问方式有公开许可、书面合作许可或正式接口合同；
3. 只读范围、搜索输入、最多页数、最大岗位数、允许时间窗和停止条件写入 runlog；
4. 默认采用授权 API；没有授权 API 时只尝试获准的可见页面路径，不回退到网页私有接口。

### 一次最小运行

1. **预检**：runlog 可写；Chrome 已由求职者正常登录；页面无登录、验证或限制提示；不读取或输出登录材料。
2. **固定输入**：冻结一个 `SearchRange`，记录不可逆 fingerprint；整个反馈环禁止变更输入。
3. **只读第 1 页**：用原生页面 `OpenPage` 取得有序稳定 ID 和明确耗尽证据，再严格串行 `ReadJob`；每成功一个岗位即落库并推进岗位级 checkpoint，只输出 `jobs/new_ids/total_ids/has_more` 与脱敏样本。
4. **首个限制即停止**：出现登录失效、验证码、安全检查、平台限制、未知响应或缺字段，立即把本轮记为 `blocked`；不自动刷新、退避、换 session、换路径或重试。
5. **人工检查第 1 页证据**：确认输入一致、稳定 ID、详情身份、平台状态与可见 `FullJD` 均可靠；结构分类不能伪造缺失内容。第 1 页不可靠时不进入第 2 页。
6. **继续到第 2、3 页**：只有授权范围允许且前页可靠时顺序继续；每页使用相同 fingerprint，累加不同稳定 ID。任何页失败则整轮多页验收失败。
7. **结束**：只有明确 `hasMore=false`/正式 end cursor 才记耗尽；三页后仍有更多只说明“本次授权窗口已读到第 3 页”，不冒充耗尽。

人工恢复只做两件事：在 BOSS 官方页面完成其要求的登录/验证，以及在产品中显式继续原 checkpoint。不得自动判断平台限制已经解除，不得通过改变浏览器身份、网络身份、调用材料或访问路径尝试恢复。

## “稳定生产读取”的明确验收标准

“稳定”不是“某次返回了若干岗位”，也不是“单个页面连续点击没有触发平台限制”。候选 Adapter 必须在**同一版本、同一获准访问方式、固定搜索输入**下同时通过下列门槛；任一项缺失都只能记为研究阶段或局部可行，不能用于通过 #35 的生产 Adapter 真实多页验收。

岗位级 checkpoint 是本文推荐的目标合同，**不是当前生产能力**。现行实现和 ADR 只持久化在线简历版本、当前搜索范围与下一页，整页失败后会重读原页；SQLite 也没有有序页内 ID 清单或已完成详情序号。采用该合同前必须创建独立实现工单，更新/取代相关 ADR，增加 forward-only migration，并用真实 SQLite 文件验证崩溃恢复，不能把离线 reducer 直接标成生产验收通过。[ADR-0011 当前整页恢复规则](../adr/0011-require-explicit-search-exhaustion.md#L5-L9)；[ADR-0016 当前运行所有权](../adr/0016-use-global-job-pool-and-resumable-discovery-runs.md#L3-L7)；[`discovery_runs` 当前页级字段](../../internal/sqlite/migrations/00001_initial.sql#L121-L152)（均访问于 2026-08-31）

| 门槛 | 通过条件 | 必须保留的脱敏证据 |
| --- | --- | --- |
| 固定搜索输入 | 一次验收冻结 `resume_version_id`、完整 `SearchRange`（role、city、salary、employment type）及 Adapter 版本；第 1 页到耗尽均使用同一不可逆 fingerprint，不能在页间改条件后仍算同一次运行。 | `search_fingerprint`、`resume_version_id`、`adapter_version`、`page_no`；仓库的输入字段合同见 [`SearchRange`](../../internal/discovery/service.go#L35-L40)，领域规则要求整轮固定一个在线简历版本。[CONTEXT.md 的搜索范围与运行定义](../../CONTEXT.md#L111-L120)；当前 runlog 只记录 role、city、page，尚不足以独立证明四字段跨页一致。[`runlog.Attempt`](../../internal/runlog/attempt.go#L60-L71)（均访问于 2026-08-31） |
| 业务字段可靠 | 每个生产必填字段都必须有求职者可读、可复核且能稳定归一化的来源证据；“非空”本身不够。薪资是可选判断内容：只得到私用区字形且没有可靠替代文本时必须清空并明确记为不可用，不能把视觉猜测、字体推断或虚构值写入生产数据；这不单独使其它字段可靠的岗位失败。 | 按字段记录 `present/reliable` 结论和失败字段名，不记录整页 HTML 或字体文件；当前 `JobObservation` 与 `jobpool` 已允许空薪资，研究 probe 在 JavaScript 中输出 `readable/unavailable`，Go 再校验文本与证据组合，但该候选尚未 live 复验。[ADR-0026](../adr/0026-allow-unavailable-job-salary.md)；[当前完整页校验](../../internal/discovery/service.go)；[probe 双层防线](../../internal/adapters/boss/visible_page_probe.go)（均访问于 2026-08-31） |
| 逐岗位可恢复 | 每个可靠岗位必须先形成可持久化的规范化结果，并把该页有序稳定 ID 与已完成稳定 ID 写入持久化 checkpoint；只存在页面脚本内存或整批返回前的临时数组不算通过。写岗位成功后才能推进对应 checkpoint，不能先推进后漏写。 | 页码、有序稳定 ID、已完成稳定 ID、当前 `detail_ordinal`，以及每个已写岗位的稳定 ID fingerprint；不得记录 Cookie、请求头、页面 HTML 或内部请求材料。推荐顺序和恢复 seam 见本文 [`OpenPage`/`ReadJob` 设计](#把整页方法深化为可恢复的页读取器)。 |
| 失败不重放已完成岗位 | 在第 N 个详情注入或实际遇到失败后，恢复同一运行必须从第 N 个岗位继续；第 1 到 N-1 个已可靠完成的详情不得再次读取。若岗位写入与 checkpoint 之间崩溃，最多允许重读当前一个岗位，并依靠稳定 ID 幂等去重。 | 失败前后的 `attempt_no`、目标稳定 ID fingerprint、`detail_ordinal` 和详情调用序列；离线状态机已证明旧整页模型会重放，而逐岗 checkpoint 可保留已完成进度。[逐岗恢复原型](../../throwaway-prototypes/boss-native-dom-read-recovery.html#L154-L215)（访问于 2026-08-31） |
| 完整第 1 页 | 先冻结第 1 页有序稳定 ID 清单，再逐一取得可靠身份、开放状态和完整可见 JD；只有 `completed_stable_ids` 与该清单完全一致、每个结果已持久化后，才允许提交第 1 页并推进页码。读取前 8 个或若干样本不算完整页。 | `page_no=1`、`jobs_count`、有序 ID 数、已完成数、新增/累计稳定 ID 数和提交结果；当前生产 Service 也是整页可靠后才观察并推进页码。[当前逐页流程](../../internal/discovery/service.go#L278-L293)（访问于 2026-08-31） |
| 连续第 2、3 页 | 第 1 页完整且明确有下一页后，必须在同一运行、同一搜索 fingerprint 下顺序完成第 2 页和第 3 页；逐页记录岗位数、新增稳定 ID、累计不同稳定 ID 和 `has_more`。为验证三页能力而选择的搜索必须实际存在至少三页；提前耗尽的另一用例只能证明结束路径。 | 三页连续的 `page_no/jobs_count/new_stable_ids/total_stable_ids/has_more`，以及跨页稳定 ID 去重结果；仓库显式 live 测试定义了同一输入最多三页的证据形状。[三页 live 测试](../../internal/adapters/boss/job_discovery_live_test.go#L17-L54)（访问于 2026-08-31） |
| 明确耗尽 | 三页能力通过后仍须继续到授权数据源明确返回 `hasMore=false`、正式 end cursor，或浏览器页面中可复核且与之等价的正式结束证据。空 DOM、连续无新增、达到 8 个上限、读取三页或超时都不能冒充耗尽。 | 最后一页/游标、正式结束字段及对应运行 fingerprint；当前 DOM probe固定返回 `exhaustionEvidence=unavailable`，所以无论单次读取多少岗位都不能单独通过此项。[DOM probe 解码合同](../../internal/adapters/boss/visible_page_probe.go#L171-L195)；[ADR-0011 的显式耗尽规则](../adr/0011-require-explicit-search-exhaustion.md#L3-L7)（均访问于 2026-08-31） |
| 首错停止 | 任一页首次出现 `authentication_expired`、`verification_required`、`platform_limited`、`code=37` 或 `_security_check=1` 时立即停止，不刷新、自动重试、换 session、换网络或换读取路径；已可靠写入的岗位与 checkpoint 保留，页码不推进，等待人工恢复。 | 稳定错误分类、`stage`、`detail_ordinal`、允许时的非敏感 `upstream_code` 及停止后的 checkpoint；不得保留可重放会话材料。Issue #36 将这些状态定义为停止边界；生产 ADR 对三类人工错误也从首次失败起不安排自动重试。[Issue #36 Safety boundary](https://github.com/Russell-Utopia/boss-job-agent/issues/36)；[ADR-0011](../adr/0011-require-explicit-search-exhaustion.md#L9)（均访问于 2026-08-31） |

验收应分三阶段记录，不能把前一阶段结果提升为后一阶段结论：

1. **阶段 A：最多 8 岗位 DOM probe。** 验证页面 hydration、selector、卡片/详情身份、开放状态、可见 `FullJD`、可选薪资证据、错误分类和首错停止。第三次 probe 已连续返回 8 个详情，但同时发现薪资仅为不可可靠归一化的私用区字形；运行后已改为清空并标记 `unavailable`，该候选仍未 live 复验。所以 8 个全部返回只证明当时前 8 个候选的流程连续性，不证明完整页、持久化、失败恢复、跨页或耗尽。
2. **阶段 B：持久恢复离线门。** 实现岗位级持久化/checkpoint 后，用真实 SQLite 文件注入岗位写入前后崩溃、首错和进程重启，证明已完成岗位不重放、最多重读当前一个岗位、页内清单变化会阻塞。纯浏览器内存或纯 reducer 不足以通过此门。
3. **阶段 C：获准的真实生产验收。** 在获得相应许可后，用通过阶段 B 的候选 Adapter 完整通过上表的字段可靠、完整第 1 页、同输入连续第 2/3 页、明确耗尽和首错停止，并保存脱敏 runlog。只有阶段 C 可以支持“稳定生产读取”或 #35 对应验收。

## 对 Issue #36 的验收影响

本研究可以完成：

- 当前链路、候选路径、推荐 seam、错误分类与停止边界；
- 逐岗恢复状态机和严格 DOM 身份/JD parser 的可运行离线 PoC；
- 首个失败请求的非敏感诊断字段与 runlog 持久化；
- 明确 `code=37` 触发条件仍未知；
- 明确当前私有接口不能当作官方开放 API 或长期生产合同。

本轮研究仍未完成以下验收；继续测试还需要新的明确 live 授权以及 BOSS 对相应访问方式的许可：

- 原生 DOM 路径取得可靠、可归一化的薪资等全部生产必填字段；
- 将岗位级 checkpoint 落入 ADR、SQLite migration 和 Service，并通过真实数据库崩溃恢复测试；
- 当前私有接口与原生页面路径达到可比较详情数后的真实受控对比；
- 真实第 1、2、3 页连续成功；
- `code=37` 的单变量触发实验；
- 验证后的平台限制恢复时间或请求速率。

所以 Issue #36 当前仍不能声称“稳定拉取已完成”：真实 DOM selector、`FullJD-first` 和连续 8 个详情已有 live 证据，运行后也已离线拒绝私用区薪资，但这道保护尚未 live 复验，DOM 路径仍没有可用的规范化薪资；同时只读取了 15 个卡片中的前 8 个，没有逐岗持久化、完整第 1 页、真实第 2/3 页或明确耗尽。#35 中“岗位发现生产 Adapter 真实多页验收”不能据此通过。[Issue #36 的 Acceptance criteria 与 Safety boundary](https://github.com/Russell-Utopia/boss-job-agent/issues/36)（访问于 2026-08-31）

## 来源清单

访问日期均为 2026-08-31。

### BOSS 一手公开来源

- [BOSS 官方首页](https://www.zhipin.com/)
- [BOSS 官方职位搜索页](https://www.zhipin.com/zhaopin/index.html)
- [BOSS 联系方式](https://www.zhipin.com/aboutContact)
- [BOSS 用户协议](https://www.zhipin.com/web/common/protocol/protocol-2019-09-30.html)（官方页面标注 2019 版；当前有效条款和授权须另行确认）

### 法规一手来源

- [国家行政法规库《网络数据安全管理条例》](https://xzfg.moj.gov.cn/front/law/detail?LawID=1734)

### Chrome 与 Web 标准一手来源

- [Chrome DevTools Protocol](https://chromedevtools.github.io/devtools-protocol/)
- [CDP Runtime domain](https://chromedevtools.github.io/devtools-protocol/tot/Runtime/)
- [CDP DOM domain](https://chromedevtools.github.io/devtools-protocol/tot/DOM/)
- [CDP DOMSnapshot domain](https://chromedevtools.github.io/devtools-protocol/tot/DOMSnapshot/)
- [Chrome debugger extension API](https://developer.chrome.com/docs/extensions/reference/api/debugger)
- [WHATWG Fetch Standard](https://fetch.spec.whatwg.org/)

### 仓库与工单一手来源

- [Issue #36](https://github.com/Russell-Utopia/boss-job-agent/issues/36)
- [Issue #4](https://github.com/Russell-Utopia/boss-job-agent/issues/4)
- [2026-08-26 初始产品方案提交](https://github.com/Russell-Utopia/boss-job-agent/commit/5f513fd39b8de74bd94e390c062ce303a14c60cb)
- [2026-08-31 生产 Adapter 源码](../../internal/adapters/boss/job_discovery.go)（研究基线为 `a4ae3fc`）
