# Domain Docs

本仓库采用 single-context 领域文档布局。

## Before exploring

开始产品设计、代码设计、实现或审查前：

1. 完整阅读仓库根目录的 `CONTEXT.md`。
2. 阅读 `docs/adr/` 中与当前工作相关且仍然有效的 ADR。
3. 涉及持久化时阅读 `docs/sqlite-schema.sql`。
4. 涉及后台模块或接口时阅读 `docs/application-modules.md`。
5. 涉及 MVP Web 时阅读 `docs/web-mvp.md`。

文件不存在时静默继续，不要为了满足目录结构提前创建空文件。

## Vocabulary

Issue 标题、规格、接口、测试名称和实现说明必须采用 `CONTEXT.md` 已确定的领域术语，不得重新引入其 `_Avoid_` 中列出的旧名称或同义词。

如果需要的概念尚未出现在词汇表中，应先判断它是否是真实领域缺口，再通过 `/domain-modeling` 或 `/grill-with-docs` 收敛。

## ADR conflicts

如果新设计与现有有效 ADR 冲突，必须明确指出冲突并重新讨论，不能静默覆盖。

已标记为 superseded 的 ADR 只用于理解历史，不得作为当前实现依据。

## Authority

- `CONTEXT.md`：领域词汇。
- `docs/adr/`：重要决策及其原因。
- `docs/sqlite-schema.sql`：持久化模型。
- `docs/application-modules.md`：模块边界和业务接口。
- `docs/web-mvp.md`：MVP Web 信息架构、交互和验收要求。
