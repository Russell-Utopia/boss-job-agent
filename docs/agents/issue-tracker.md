# 工单系统：GitHub

本仓库的工单与规格统一保存在 `Russell-Utopia/boss-job-agent` 的 GitHub Issues 中。所有操作使用 `gh` CLI。

## 操作约定

- **创建工单**：运行 `gh issue create --title "..." --body "..."`。多行正文使用 heredoc。
- **读取工单**：运行 `gh issue view <编号> --comments`，同时读取工单标签。
- **列出工单**：运行 `gh issue list`，根据任务添加合适的 `--label`、`--state` 和 JSON 字段。
- **评论工单**：运行 `gh issue comment <编号> --body "..."`。
- **添加或删除标签**：运行 `gh issue edit <编号> --add-label "..."` 或 `--remove-label "..."`。
- **关闭工单**：运行 `gh issue close <编号> --comment "..."`。

仓库身份从 `git remote -v` 推断；在本仓库工作目录中运行时，`gh` 会自动识别远端仓库。

## 是否将 Pull Request 作为分诊请求入口

**否。PR 不作为请求入口。**

GitHub 的 Issue 与 Pull Request 共用编号空间。如果 `#42` 的类型不明确，先运行 `gh pr view 42`，失败后再运行 `gh issue view 42`。

## 当技能要求“发布到工单系统”时

创建一个 GitHub Issue。

## 当技能要求“读取相关工单”时

运行 `gh issue view <编号> --comments`。

## Wayfinder 操作约定

Wayfinder 的地图是一个 GitHub Issue，子工单是该地图下的工作项。

- **地图**：使用一个带有 `wayfinder:map` 标签的 Issue。
- **子工单**：优先使用 GitHub 子工单关系；不可用时，在地图正文任务列表中引用子工单，并在子工单正文开头写明 `Part of #<地图编号>`。
- **子工单类型**：使用 `wayfinder:research`、`wayfinder:prototype`、`wayfinder:grilling` 或 `wayfinder:task` 标签。
- **阻塞关系**：优先使用 GitHub 原生 Issue 依赖关系；不可用时，在正文开头写明 `Blocked by: #<编号>`。
- **选择下一项工作**：从地图中按顺序查找没有开放阻塞项、也没有负责人认领的第一个开放子工单。
- **认领**：运行 `gh issue edit <编号> --add-assignee @me`；这是一次 Wayfinder 会话的首次写操作。
- **完成**：在子工单中评论结果、关闭子工单，并把上下文链接追加到地图的 Decisions-so-far 中。
