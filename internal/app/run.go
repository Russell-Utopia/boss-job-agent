package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

func Run(ctx context.Context, config Config) (runErr error) {
	config, err := config.withDefaults()
	if err != nil {
		return fmt.Errorf("resolve application config: %w", err)
	}
	runtime, err := assemble(ctx, config)
	if err != nil {
		return fmt.Errorf("assemble application: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, runtime.close())
	}()

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", config.Address)
	if err != nil {
		return fmt.Errorf("listen for Web requests: %w", err)
	}
	server := &http.Server{
		Handler:           runtime.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverError := make(chan error, 1)
	go func() {
		log.Printf("BOSS Job Agent 后台已启动：http://%s", listener.Addr())
		serverError <- server.Serve(listener)
	}()

	select {
	case err := <-serverError:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("run Web server: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("stop Web server: %w", err)
		}
		if err := <-serverError; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("finish Web server: %w", err)
		}
		return nil
	}
}
