# 同一 BOSS 账号下每个 Worker 独占浏览器 Session

岗位发现 Worker 和每个首次沟通 Worker 分别独占一个浏览器 session，并可在同一个 BOSS 账号下并行执行；v1 使用一个发现 session 和一个发送 session，后续增加发送 Worker 时同时增加对应 session。Kimi WebBridge 的 session 是共享 Chrome 登录态的独立标签组，未来执行器也可以提供真正隔离 Cookie 的浏览器上下文；账号级登录、验证、平台限制、失败恢复，以及避免对已沟通岗位重复打招呼，仍由 Go 程序统一协调，但不再要求所有浏览器动作全局串行。
