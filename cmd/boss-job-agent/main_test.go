package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

func TestLogsFindCommandUsesOnlyRunlogAndReturnsExactEvents(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := runlog.Open(path)
	trace, err := logs.Start(t.Context(), runlog.Attempt{
		Flow:          runlog.FlowAssessment,
		Operation:     runlog.OperationSubmitAssessment,
		PlatformJobID: "boss-123",
		AttemptNo:     2,
	})
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	if err := logs.Finish(t.Context(), trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	if err := logs.Close(); err != nil {
		t.Fatalf("close runlog: %v", err)
	}

	var output bytes.Buffer
	if err := execute(t.Context(), []string{
		"logs", "find",
		"--log", path,
		"--trace-id", trace.ID(),
	}, &output); err != nil {
		t.Fatalf("execute logs find: %v", err)
	}
	var report runlog.Report
	if err := json.NewDecoder(&output).Decode(&report); err != nil {
		t.Fatalf("decode logs find output: %v", err)
	}
	if len(report.Events) != 2 {
		t.Fatalf("event count = %d, want start and finish", len(report.Events))
	}
}
