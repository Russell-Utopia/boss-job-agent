# 由 JobPool 模块统一推进平台岗位状态

`platform_jobs` 的所有写入统一经过进程内 `JobPool` 模块；岗位发现、岗位鉴定、打招呼和 Web 都只能提交“观察到岗位”“记录人工结论”“完成鉴定”“完成打招呼”等业务意图，不能直接设置状态或逐字段更新。这样 JD 变化使旧 AI 结论失效并撤回待打招呼请求、人工结论变化影响打招呼资格、动作前发现岗位关闭等跨流程规则，可以在一个 SQLite 事务中校验并落库。策略优化与验收只读取有效人工复核并返回页面会话证据，不能通过 `JobPool` 写回实验结果。

`JobPool` 不是第四个 Worker、进程、队列或数据表，只是全局岗位状态机及持久化实现所在的深模块。`online_resume_versions` 只由在线简历版本模块响应求职者手动刷新写入，`discovery_runs` 由岗位发现模块写入，`assessment_policy_versions` 由岗位鉴定模块写入；只读查询可以共享，但不得暴露通用 `UpdateJob`、Repository 或状态 setter 绕过 `JobPool` 的业务接口。
