package runlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindMatchesExactTraceAndCompositeKeysAndReportsIncompleteFiles(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, logFilename)
	traceID := createFindFixture(t, path)

	traceReport := findIncompleteReport(t, path, Query{TraceID: traceID}, 2)
	compositeReport := findIncompleteReport(t, path, Query{
		Flow:           FlowDiscovery,
		Operation:      OperationFetchPage,
		DiscoveryRunID: 42,
		AttemptNo:      7,
		SearchRole:     "Go工程师",
		SearchCity:     "福州",
		PageNo:         3,
	}, 2)
	if !reflect.DeepEqual(rawStrings(traceReport.Events), rawStrings(compositeReport.Events)) {
		t.Errorf("trace and composite matches differ\ntrace: %v\ncomposite: %v", traceReport.Events, compositeReport.Events)
	}
	assertMatchedTrace(t, traceReport.Events, traceID)
}

func createFindFixture(t *testing.T, path string) string {
	t.Helper()
	logs := Open(path)
	trace, err := logs.Start(t.Context(), Attempt{
		Flow:           FlowDiscovery,
		Operation:      OperationFetchPage,
		DiscoveryRunID: 42,
		AttemptNo:      7,
		SearchRole:     "Go工程师",
		SearchCity:     "福州",
		PageNo:         3,
	})
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	if err := logs.Finish(t.Context(), trace, AttemptResult{Outcome: OutcomeSucceeded}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	if err := logs.Close(); err != nil {
		t.Fatalf("close runlog: %v", err)
	}
	rotated := filepath.Join(filepath.Dir(path), "boss-job-agent-2026-08-31T01-02-03.004.jsonl")
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rotate test log: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatalf("write incomplete log: %v", err)
	}
	return trace.ID()
}

func findIncompleteReport(t *testing.T, path string, query Query, wantEvents int) Report {
	t.Helper()
	report, findErr := Find(t.Context(), path, query)
	var incomplete *IncompleteError
	if !errors.As(findErr, &incomplete) {
		t.Fatalf("find error = %v, want IncompleteError", findErr)
	}
	if !reflect.DeepEqual(incomplete.Files, []string{path}) {
		t.Errorf("incomplete files = %v, want [%s]", incomplete.Files, path)
	}
	if len(report.Events) != wantEvents {
		t.Fatalf("matches = %d, want %d", len(report.Events), wantEvents)
	}
	return report
}

func assertMatchedTrace(t *testing.T, events []json.RawMessage, traceID string) {
	t.Helper()
	for _, event := range events {
		var fields map[string]any
		if err := json.Unmarshal(event, &fields); err != nil {
			t.Fatalf("decode matched event: %v", err)
		}
		if got := fields["trace_id"]; got != traceID {
			t.Errorf("matched trace_id = %v, want %s", got, traceID)
		}
	}
}

func rawStrings(values []json.RawMessage) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = string(values[index])
	}
	return result
}
