# BOSS Job Agent

BOSS Job Agent 是运行在求职者 Mac 上的个人求职助手。当前 T01 切片提供一个持有 SQLite 的常驻 Go 后台，以及岗位、岗位鉴定、首次沟通和在线简历四个 Web 入口。

## 本地启动

需要 Go 1.23 或更高版本。在仓库根目录运行：

```bash
make run
```

然后打开 <http://127.0.0.1:8080>。默认数据保存在 `./var/boss-job-agent.db`；也可以显式指定监听地址和数据库路径：

```bash
go run ./cmd/boss-job-agent -addr 127.0.0.1:8090 -db ./var/local.db
```

关闭浏览器页面只会关闭界面，不会停止后台进程。需要停止后台时，在启动它的终端按 `Ctrl+C`。

首次启动只初始化五张业务表、默认策略 v1 和一行安全默认设置。它不会访问 BOSS、启动 Pi 或执行任何真实沟通。

## 验证

```bash
make check
```

测试从 Web 使用的业务查询与命令启动完整应用，使用生产同一份 DDL 和真实内存 SQLite。
