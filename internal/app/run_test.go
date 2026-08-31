package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRunStopsCleanlyWhenItsContextEnds(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err := Run(ctx, Config{
		Address:      "127.0.0.1:0",
		DatabasePath: ":memory:",
		LogPath:      filepath.Join(t.TempDir(), "boss-job-agent.jsonl"),
		Now: func() time.Time {
			return time.UnixMilli(1000)
		},
	})
	if err != nil {
		t.Fatalf("run canceled application: %v", err)
	}
}

func TestAssembleKeepsWebAvailableWhenDefaultRunlogPathCannotResolve(t *testing.T) {
	t.Setenv("HOME", "relative-home")

	runtime, err := assemble(t.Context(), Config{
		DatabasePath: ":memory:",
		Now:          time.Now,
	})
	if err != nil {
		t.Fatalf("assemble degraded application: %v", err)
	}
	t.Cleanup(func() { _ = runtime.close() })
	if health := runtime.logs.Health(); health.Healthy {
		t.Fatalf("runlog health = %#v, want degraded path resolution", health)
	}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/jobs", nil)
	response := httptest.NewRecorder()
	runtime.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("jobs status = %d, want Web available", response.Code)
	}
}
