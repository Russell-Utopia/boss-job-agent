package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Russell-Utopia/boss-job-agent/internal/app"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	config, err := app.DefaultConfig()
	if err != nil {
		return fmt.Errorf("解析默认 SQLite 路径: %w", err)
	}
	flag.StringVar(&config.Address, "addr", config.Address, "Web 监听地址")
	flag.StringVar(&config.DatabasePath, "db", config.DatabasePath, "SQLite 文件路径")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, config); err != nil {
		return fmt.Errorf("启动应用: %w", err)
	}
	return nil
}
