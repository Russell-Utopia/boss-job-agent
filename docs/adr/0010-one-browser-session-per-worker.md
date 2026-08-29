# 同一 BOSS 账号下每个 Worker 独占浏览器 Session

岗位发现 Worker 和每个首次沟通 Worker 分别长期持有一个浏览器 Adapter 实例，该实例独占一个 session，并可在同一个 BOSS 账号下并行执行；工作作为参数交给 Worker，岗位失败、完成或等待重试不会释放或转移该 session。v1 使用一个发现 session 和一个发送 session，后续增加 Worker 时同时增加对应 Adapter 实例和独立 session，不建立岗位与原 Worker 的亲和关系。Kimi WebBridge 的 session 是共享 Chrome 登录态的独立标签组，未来执行器也可以提供真正隔离 Cookie 的浏览器上下文；接口不暴露 session，账号级登录、验证、平台限制、失败恢复，以及避免对已沟通岗位重复打招呼，仍由 Go 程序统一协调，但不再要求所有浏览器动作全局串行。
