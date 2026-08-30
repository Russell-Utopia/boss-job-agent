package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/application"
	"github.com/Russell-Utopia/boss-job-agent/internal/webui"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() (runErr error) {
	address := flag.String("addr", "127.0.0.1:8080", "Web 监听地址")
	databasePath := flag.String("db", "./var/boss-job-agent.db", "SQLite 文件路径")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := application.Open(ctx, application.Config{DatabasePath: *databasePath})
	if err != nil {
		return fmt.Errorf("启动应用: %w", err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("关闭应用: %w", err))
		}
	}()

	server := &http.Server{
		Addr:              *address,
		Handler:           webui.New(app),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverError := make(chan error, 1)
	go func() {
		log.Printf("BOSS Job Agent 后台已启动：http://%s", *address)
		serverError <- server.ListenAndServe()
	}()

	select {
	case err := <-serverError:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("运行 Web 服务: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("停止 Web 服务: %w", err)
		}
		if err := <-serverError; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("停止 Web 服务: %w", err)
		}
		return nil
	}
}
