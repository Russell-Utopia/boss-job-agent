# BOSS Job Agent

BOSS Job Agent 是运行在求职者 Mac 上的个人求职助手。当前 T01 切片提供一个持有 SQLite 的常驻 Go 后台，以及岗位、岗位鉴定、首次沟通和在线简历四个 Web 入口。

## 本地启动

需要仓库固定的 Go 1.27.0 工具链。在仓库根目录运行：

```bash
make run
```

然后打开 <http://127.0.0.1:8080>。Mac 默认数据库保存在 `~/Library/Application Support/boss-job-agent/data/boss-job-agent.db`，升级前备份保存在同级 `backups/`；开发和测试也可以显式指定监听地址和数据库路径：

```bash
go run ./cmd/boss-job-agent -addr 127.0.0.1:8090 -db ./var/local.db
```

关闭浏览器页面只会关闭界面，不会停止后台进程。需要停止后台时，在启动它的终端按 `Ctrl+C`。

首次启动只初始化五张业务表、默认策略 v1 和一行安全默认设置。它不会访问 BOSS、启动 Pi 或执行任何真实沟通。

## 刷新在线简历

“在线简历”页的“刷新在线简历”是唯一会读取 BOSS 简历的入口。点击后，应用通过本机 `http://127.0.0.1:10086` 的 Kimi WebBridge 复用已经登录 BOSS 的 Chrome 会话，只读取求职期望、工作经历、项目经历、教育经历和技能；不会读取或保存电话、微信、邮箱，也不会由启动、岗位发现或岗位鉴定自动触发。

生产 Adapter 的真实连接验收不属于默认测试或 CI。确认 Chrome 已登录 BOSS、Kimi WebBridge daemon 与扩展均正常后，显式运行：

```bash
make live-online-resume
```

该命令用临时内存 SQLite 验证在线简历读取、版本保存和结构化 trace 合同，并输出五部分的条目数量；它不写入应用 SQLite，也不输出简历正文。若真实读取失败，测试还会先校验错误是否带有稳定分类。

## 岗位发现 live 验收

岗位发现的默认测试使用受控 Adapter，不连接 BOSS。确认 Chrome 已登录、Kimi WebBridge 正常，并准备好一个与在线简历一致的单一搜索范围后，可显式验证一页生产读取合同：

```bash
BOSS_JOB_DISCOVERY_ROLE='Go 后端工程师' \
BOSS_JOB_DISCOVERY_CITY='福州' \
BOSS_JOB_DISCOVERY_SALARY='20-30K' \
BOSS_JOB_DISCOVERY_EMPLOYMENT_TYPE='全职' \
make live-job-discovery
```

该命令只读取第 1 页，验证稳定平台岗位 ID、完整 JD、可靠平台状态和 `hasMore`，不写入应用 SQLite，也不会开始鉴定或打招呼。

## 架构资料

- [Go 工程架构](./docs/architecture.md)：目标目录、依赖方向、应用装配、状态所有权与迁移切片。
- [v1 后台模块与接口](./docs/application-modules.md)：业务模块 seam 与调用语义。
- [MVP Web 交互规格](./docs/web-mvp.md)：页面流程、状态展示与用户操作。

## 验证

```bash
make check
```

测试从 Web 使用的业务查询与命令启动完整应用，使用生产同一份 DDL 和真实内存 SQLite。
