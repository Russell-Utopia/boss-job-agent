package app

import (
	"context"
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
		Now: func() time.Time {
			return time.UnixMilli(1000)
		},
	})
	if err != nil {
		t.Fatalf("run canceled application: %v", err)
	}
}
