package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Russell-Utopia/boss-job-agent/internal/app"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return execute(ctx, os.Args[1:], os.Stdout)
}

func execute(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		return executeServe(ctx, args[1:])
	case "logs":
		if len(args) < 2 || args[1] != "find" {
			return fmt.Errorf("用法：boss-job-agent logs find [精确查询参数]")
		}
		return executeLogsFind(ctx, args[2:], output)
	default:
		return fmt.Errorf("未知命令 %q；可用命令：serve、logs find", args[0])
	}
}

func executeServe(ctx context.Context, args []string) error {
	config, err := app.DefaultConfig()
	if err != nil {
		return fmt.Errorf("解析默认本地路径: %w", err)
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&config.Address, "addr", config.Address, "Web 监听地址")
	flags.StringVar(&config.DatabasePath, "db", config.DatabasePath, "SQLite 文件路径")
	flags.StringVar(&config.LogPath, "log", config.LogPath, "JSONL 运行日志路径")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("解析 serve 参数: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve 不接受位置参数")
	}
	if err := app.Run(ctx, config); err != nil {
		return fmt.Errorf("启动应用: %w", err)
	}
	return nil
}

func executeLogsFind(ctx context.Context, args []string, output io.Writer) error {
	path, err := runlog.DefaultPath()
	if err != nil {
		return fmt.Errorf("解析默认运行日志路径: %w", err)
	}
	var query runlog.Query
	var flow string
	var operation string
	flags := flag.NewFlagSet("logs find", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&path, "log", path, "JSONL 运行日志路径")
	flags.StringVar(&query.TraceID, "trace-id", "", "精确 trace ID")
	flags.StringVar(&flow, "flow", "", "online_resume、discovery、assessment 或 outreach")
	flags.StringVar(&operation, "operation", "", "精确外部操作名")
	flags.Int64Var(&query.DiscoveryRunID, "discovery-run-id", 0, "岗位发现运行 ID")
	flags.StringVar(&query.PlatformJobID, "platform-job-id", "", "BOSS 平台岗位 ID")
	flags.Int64Var(&query.AttemptNo, "attempt-no", 0, "生命周期尝试号")
	flags.StringVar(&query.SearchRole, "search-role", "", "精确搜索岗位")
	flags.StringVar(&query.SearchCity, "search-city", "", "精确搜索城市")
	flags.IntVar(&query.PageNo, "page-no", 0, "搜索页码")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("解析 logs find 参数: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("logs find 不接受位置参数")
	}
	query.Flow = runlog.Flow(flow)
	query.Operation = runlog.Operation(operation)
	report, findErr := runlog.Find(ctx, path, query)
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("输出日志查询结果: %w", err)
	}
	return findErr
}
