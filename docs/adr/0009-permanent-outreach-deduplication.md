# 同一岗位真实沟通成功后不再重复打招呼

一旦某个 `platform_job_id` 被确认成功完成首次沟通，`platform_jobs.outreach_status` 永久记为 `contacted`，不按岗位发现运行或日期重置；以后再次发现该岗位只更新可展示的当前 JD 和最后发现时间，不重新进入发送队列。如果 SQLite 没有记录但 BOSS 显示此前已经沟通过，也以 BOSS 的外部状态为准停止发送并补记全局岗位状态。招聘方重新发布并产生新的 `platform_job_id` 时，即使公司、岗位名称和 JD 都与旧岗位相同或相似，也视为新的平台岗位；旧岗位的本地 `contacted` 不阻止新岗位鉴定或发送，但如果 BOSS 对新岗位本身显示已经沟通过，仍按该新岗位的外部证据停止发送。

`simulated` 只证明某一轮模拟已经完成，没有对 BOSS 或招聘者产生外部影响，因此不会阻止以后真实发送。岗位可以从 `simulated` 重新进入 `pending(real)`，真实入队时仍需重新检查岗位是否开放、是否适合以及是否已经在 BOSS 沟通过。已经排队的模拟轮次不会被直接改成真实模式，避免设置切换把无外部影响的动作静默升级为真实发送。

“已经真实沟通过的岗位不再重复打招呼”由 `JobPool` 的本地沟通资格检查和 `PostService` 的 BOSS 发送前复查共同保证，而不是交给 LLM 或单个 Worker 自行判断。平台岗位入队和发送 Worker 领取由 `JobPool` 重新检查本地全局状态，只有最近一次可靠平台岗位状态为“可沟通”才允许继续；真实发送前，`PostService` 还要读取 BOSS 当前状态，发现岗位已关闭或 `contact=true` 时停止动作，并把证据提交给 `JobPool` 更新平台岗位状态或补记已沟通事实。若检查在外部动作发生前失败，则保留最近一次可靠平台岗位状态，由 `JobPool` 把发送状态写为 `failed` 并保存错误；只有动作可能已经发生但结果无法确认时才写为 `possibly_contacted`，在对账前禁止重试。并发、恢复、重新鉴定或批量选择都不能绕过这两层检查。

正常发送取得 BOSS 成功证据后，`PostService` 在同一次处理里把证据提交给 `JobPool`，由后者自动把岗位从 `processing` 推进为 `contacted`，保存时间、来源和页面证据，不经过 `possibly_contacted`。只有点击或请求可能已经到达 BOSS、但程序在取得确认或写入本地结果前发生超时、断线或崩溃时，才进入 `possibly_contacted`。恢复后 `PostService` 优先自动对账并把证据提交给 `JobPool`：BOSS 证明已沟通则推进为 `contacted`，可靠证明未沟通则推进为 `failed`，仍无法取得证据则保持 `possibly_contacted`；任何情况下都不能直接从该状态重新发送。

每次发送尝试开始前递增生命周期内单调递增的 `outreach_attempt_no` 并把状态写为 `processing`。能够确认没有产生沟通的错误写为 `failed`，保存 `outreach_last_error` 并递增 `outreach_consecutive_failure_count`；临时错误只有在连续失败次数尚未达到程序配置上限时才设置有限退避后的 `outreach_retry_at`，调度器到时先恢复为 `pending`，但不把连续失败次数归零。v1 默认一个无人干预周期总共最多尝试三次（包含首次）；确认成功、模拟完成或求职者处理后显式重新开始时才归零。登录失效、验证码和平台限制等不可自动恢复错误从第一次失败起就不安排自动重试。`possibly_contacted` 在对账确认没有沟通并转为 `failed` 前不计为明确失败。详细逐次错误只写普通运行日志，不建立历史业务表；鉴定与发送分别保存失败次数和重试时间，不能共用字段。
