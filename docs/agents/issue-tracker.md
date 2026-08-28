# Issue tracker: GitHub

本项目的规格和工程工单保存在私有 GitHub 仓库的 GitHub Issues 中，所有操作使用 `gh` CLI。

执行 Issue 操作前，从 `git remote -v` 确定目标仓库。如果尚未配置 GitHub remote，应停止操作并提示用户先创建或关联私有仓库，不能猜测仓库地址。

## Conventions

- 创建：`gh issue create --title "..." --body "..."`
- 阅读：`gh issue view <number> --comments`
- 列表：`gh issue list --state open --json number,title,body,labels,comments`
- 评论：`gh issue comment <number> --body "..."`
- 添加标签：`gh issue edit <number> --add-label "..."`
- 删除标签：`gh issue edit <number> --remove-label "..."`
- 关闭：`gh issue close <number> --comment "..."`

多行正文使用 heredoc，读取结果时同时取得正文、评论和标签。

## Pull requests as a triage surface

**PRs as a request surface: no.**

Pull Request 不作为外部需求入口；`triage` 默认只处理 Issues。

## Skill operations

当技能要求“发布到 issue tracker”时，创建 GitHub Issue。

当技能要求“读取相关 ticket”时，运行：

`gh issue view <number> --comments`

## Blocking relationships

优先使用 GitHub 原生 Issue dependencies 表达阻塞关系。若目标仓库暂不支持，则在子 Issue 顶部写入：

`Blocked by: #<number>, #<number>`

只有全部 blocker 关闭后，该 Issue 才可以开始实现。

## Wayfinder operations

- Map：一个带 `wayfinder:map` 标签的 GitHub Issue。
- Child ticket：优先使用 GitHub sub-issue；不可用时，在 Map 的任务列表中引用，并在子 Issue 顶部写 `Part of #<map-number>`。
- 类型标签：`wayfinder:research`、`wayfinder:prototype`、`wayfinder:grilling`、`wayfinder:task`。
- Claim：把 Issue 分配给当前用户。
- Resolve：先追加答案和上下文链接，再关闭 Issue。
