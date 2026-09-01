package runlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestOpenRepairsMissingPrivateDirectoryAndFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() {
		if err := logs.Close(); err != nil {
			t.Errorf("close runlog: %v", err)
		}
	})

	health := logs.Health()
	if !health.Healthy {
		t.Fatalf("runlog health = %#v, want healthy", health)
	}
	assertPrivateMode(t, filepath.Dir(path), 0o700)
	assertPrivateMode(t, path, 0o600)
}

func TestAttemptFailurePersistsStableFieldsAndCompleteErrorTree(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() { closeRunlog(t, logs) })
	trace := writeFailedAttempt(t, logs)
	assertTraceID(t, trace.ID())

	records := readJSONL(t, path)
	if len(records) != 3 {
		t.Fatalf("record count = %d, want startup + start + finish", len(records))
	}
	assertFailureRecord(t, records[2], trace.ID())
}

func TestLinkedAttemptKeepsTheOriginalBusinessTraceID(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() { closeRunlog(t, logs) })
	submit, err := logs.Start(t.Context(), Attempt{
		Flow: FlowAssessment, Operation: OperationSubmitAssessment,
		PlatformJobID: "boss-job-7", AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("start assessment submission: %v", err)
	}
	confirmation, err := logs.StartLinked(t.Context(), submit.ID(), Attempt{
		Flow: FlowAssessment, Operation: OperationConfirmAssessmentResults,
		PlatformJobID: "boss-job-7", AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("start linked confirmation: %v", err)
	}
	if confirmation.ID() != submit.ID() {
		t.Fatalf("confirmation trace ID = %q, want %q", confirmation.ID(), submit.ID())
	}
}

func TestTechnicalErrorPersistsDiscoveryIdentityAndErrorTree(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() { closeRunlog(t, logs) })
	err := logs.RecordTechnicalError(t.Context(), TechnicalError{
		Flow:           FlowDiscovery,
		Stage:          "worker_lease_expired",
		DiscoveryRunID: 42,
		AttemptNo:      3,
		Err:            fmt.Errorf("recover discovery: %w", errors.New("lease expired")),
	})
	if err != nil {
		t.Fatalf("record technical error: %v", err)
	}

	records := readJSONL(t, path)
	if len(records) != 2 {
		t.Fatalf("record count = %d, want startup + technical error", len(records))
	}
	record := records[1]
	assertJSONField(t, record, "event", "technical_error")
	traceID, ok := record["trace_id"].(string)
	if !ok {
		t.Fatalf("trace_id = %#v, want string", record["trace_id"])
	}
	assertTraceID(t, traceID)
	assertJSONField(t, record, "flow", "discovery")
	assertJSONField(t, record, "stage", "worker_lease_expired")
	assertJSONField(t, record, "discovery_run_id", float64(42))
	assertJSONField(t, record, "attempt_no", float64(3))
	if _, ok := record["error_chain"].([]any); !ok {
		t.Fatalf("error_chain = %#v, want array", record["error_chain"])
	}
}

func writeFailedAttempt(t *testing.T, logs *Log) Trace {
	t.Helper()
	trace, err := logs.Start(t.Context(), Attempt{
		Flow:             FlowDiscovery,
		Operation:        OperationReadJob,
		DiscoveryRunID:   42,
		AttemptNo:        7,
		PageNo:           3,
		JobOrdinal:       4,
		JobIDFingerprint: "9542806604c794eebc1517859836f31a3cf607ba0363d16be187120bb497c5fb",
	})
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	joined := errors.Join(
		errors.New(`{"Authorization":"Bearer secret-json"}`),
		fmt.Errorf("read response: %w", errors.New("Cookie: a=1; token=secret-after-semicolon")),
		errors.New("Prompt: first line\nsecond-line-model-secret"),
	)
	if err := logs.Finish(context.Background(), trace, AttemptResult{
		Outcome:       OutcomeFailed,
		ErrorCategory: ErrorCategoryTransient,
		ExternalFailure: &ExternalFailureEvidence{
			RequestOrdinal: 7,
			Stage:          "job_detail",
			DetailOrdinal:  4,
			UpstreamCode:   "37",
		},
		Err: fmt.Errorf("fetch page: %w", joined),
	}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	return trace
}

func assertTraceID(t *testing.T, traceID string) {
	t.Helper()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(traceID) || traceID == strings.Repeat("0", 32) {
		t.Fatalf("trace ID = %q, want non-zero 32 lowercase hex characters", traceID)
	}
}

func assertFailureRecord(t *testing.T, finish map[string]any, traceID string) {
	t.Helper()
	assertJSONField(t, finish, "schema_version", float64(1))
	assertJSONField(t, finish, "event", "external_attempt_finished")
	assertJSONField(t, finish, "outcome", "failed")
	assertJSONField(t, finish, "trace_id", traceID)
	assertJSONField(t, finish, "flow", "discovery")
	assertJSONField(t, finish, "operation", "read_job")
	assertJSONField(t, finish, "discovery_run_id", float64(42))
	assertJSONField(t, finish, "attempt_no", float64(7))
	assertJSONField(t, finish, "page_no", float64(3))
	assertJSONField(t, finish, "job_ordinal", float64(4))
	assertJSONField(t, finish, "job_id_fingerprint", "9542806604c794eebc1517859836f31a3cf607ba0363d16be187120bb497c5fb")
	assertJSONField(t, finish, "error_category", "transient")
	assertJSONField(t, finish, "request_ordinal", float64(7))
	assertJSONField(t, finish, "stage", "job_detail")
	assertJSONField(t, finish, "detail_ordinal", float64(4))
	assertJSONField(t, finish, "upstream_code", "37")
	assertCompleteRedactedErrorChain(t, finish["error_chain"])
}

func assertCompleteRedactedErrorChain(t *testing.T, value any) {
	t.Helper()
	chain, ok := value.([]any)
	if !ok {
		t.Fatalf("error_chain type = %T, want array", value)
	}
	wantPaths := []string{"0", "0.0", "0.0.0", "0.0.1", "0.0.1.0", "0.0.2"}
	if len(chain) != len(wantPaths) {
		t.Fatalf("error_chain length = %d, want %d: %#v", len(chain), len(wantPaths), chain)
	}
	for index, wantPath := range wantPaths {
		node := chain[index].(map[string]any)
		assertJSONField(t, node, "path", wantPath)
		message := node["message"].(string)
		if strings.Contains(message, "secret-json") || strings.Contains(message, "secret-after-semicolon") || strings.Contains(message, "second-line-model-secret") {
			t.Errorf("error node %s leaked a secret: %q", wantPath, message)
		}
	}
}

func TestCloseFailsWhenTerminalRecordsCannotBePersisted(t *testing.T) {
	t.Parallel()

	logs := Open(filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
	trace, err := logs.Start(t.Context(), testDiscoveryAttempt(1))
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	logs.mu.Lock()
	logs.handler = slog.NewJSONHandler(errorWriter{err: errors.New("disk remains unavailable")}, nil)
	logs.stderr = io.Discard
	logs.mu.Unlock()
	if err := logs.Finish(t.Context(), trace, AttemptResult{
		Outcome:       OutcomeFailed,
		ErrorCategory: ErrorCategoryTransient,
		Err:           errors.New("external call failed"),
	}); err == nil {
		t.Fatal("finish unexpectedly succeeded")
	}
	logs.mu.Lock()
	logs.resolvePath = func() (string, error) { return "", errors.New("path remains unavailable") }
	logs.mu.Unlock()

	if err := logs.Close(); !errors.Is(err, ErrPendingTerminalRecords) {
		t.Fatalf("close error = %v, want ErrPendingTerminalRecords", err)
	}
}

func TestBatchTraceWritesSelfContainedTerminalRecordForEachPlatformJob(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() { closeRunlog(t, logs) })
	attempts := []Attempt{
		{Flow: FlowAssessment, Operation: OperationSubmitAssessment, PlatformJobID: "boss-1", AttemptNo: 2},
		{Flow: FlowAssessment, Operation: OperationSubmitAssessment, PlatformJobID: "boss-2", AttemptNo: 4},
	}
	trace, err := logs.StartBatch(t.Context(), attempts)
	if err != nil {
		t.Fatalf("start batch: %v", err)
	}
	for _, attempt := range attempts {
		if err := logs.FinishItem(t.Context(), trace, attempt, AttemptResult{Outcome: OutcomeSucceeded}); err != nil {
			t.Fatalf("finish batch item %s: %v", attempt.PlatformJobID, err)
		}
	}

	records := readJSONL(t, path)
	if len(records) != 5 {
		t.Fatalf("record count = %d, want startup + two starts + two finishes", len(records))
	}
	for index, record := range records[1:] {
		assertJSONField(t, record, "trace_id", trace.ID())
		assertJSONField(t, record, "batch_size", float64(2))
		assertJSONField(t, record, "batch_item_index", float64(index%2))
		attempt := attempts[index%2]
		assertJSONField(t, record, "platform_job_id", attempt.PlatformJobID)
		assertJSONField(t, record, "attempt_no", float64(attempt.AttemptNo))
	}
}

func TestWriteFailureSynchronouslyDegradesAndClosesNewAttemptGate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() { _ = logs.Close() })
	trace, err := logs.Start(t.Context(), testDiscoveryAttempt(1))
	if err != nil {
		t.Fatalf("start first attempt: %v", err)
	}

	diskError := errors.New("simulated disk full")
	var warning bytes.Buffer
	logs.mu.Lock()
	logs.handler = slog.NewJSONHandler(errorWriter{err: diskError}, nil)
	logs.stderr = &warning
	logs.mu.Unlock()

	err = logs.Finish(t.Context(), trace, AttemptResult{
		Outcome:       OutcomeFailed,
		ErrorCategory: ErrorCategoryTransient,
		Err:           errors.New("network unavailable"),
	})
	if !errors.Is(err, diskError) {
		t.Fatalf("finish error = %v, want disk error", err)
	}
	if health := logs.Health(); health.Healthy || health.Code != "log_unavailable" {
		t.Fatalf("health after failed write = %#v, want unavailable", health)
	}
	if health := logs.Health(); health.PendingTerminalRecords != 1 {
		t.Fatalf("pending terminal records = %d, want 1", health.PendingTerminalRecords)
	}
	if !strings.Contains(warning.String(), "simulated disk full") {
		t.Errorf("stderr warning = %q, want minimal disk error", warning.String())
	}

	_, err = logs.Start(t.Context(), testDiscoveryAttempt(2))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second attempt error = %v, want ErrUnavailable", err)
	}
}

func TestRecheckRequiresConfirmationBeforeQuarantiningConflictingLogPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("create conflicting log directory: %v", err)
	}
	logs := Open(path)
	t.Cleanup(func() { _ = logs.Close() })

	assertConfirmationRequired(t, logs.Health())
	assertConfirmationRequired(t, logs.Recheck(t.Context(), RepairDecision{}))
	assertPathKind(t, path, true)

	assertHealthy(t, logs.Recheck(t.Context(), RepairDecision{ConfirmQuarantine: true}))
	assertPathKind(t, path, false)
	assertOneQuarantinedPath(t, path)
}

func assertConfirmationRequired(t *testing.T, health Health) {
	t.Helper()
	if health.Healthy || !health.ConfirmationRequired {
		t.Fatalf("health = %#v, want confirmation required", health)
	}
}

func assertHealthy(t *testing.T, health Health) {
	t.Helper()
	if !health.Healthy {
		t.Fatalf("health = %#v, want healthy", health)
	}
}

func assertPathKind(t *testing.T, path string, wantDirectory bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat repaired path: %v", err)
	}
	if info.IsDir() != wantDirectory {
		t.Fatalf("path mode = %v, want directory=%t", info.Mode(), wantDirectory)
	}
}

func assertOneQuarantinedPath(t *testing.T, path string) {
	t.Helper()
	quarantined, err := filepath.Glob(path + ".quarantine-*")
	if err != nil {
		t.Fatalf("find quarantined conflict: %v", err)
	}
	if len(quarantined) != 1 {
		t.Fatalf("quarantined paths = %v, want one", quarantined)
	}
}

func closeRunlog(t *testing.T, logs *Log) {
	t.Helper()
	if err := logs.Close(); err != nil {
		t.Errorf("close runlog: %v", err)
	}
}

func TestPeriodicRecheckAutomaticallyRecoversSafeWriteFailure(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() { _ = logs.Close() })
	trace, err := logs.Start(t.Context(), testDiscoveryAttempt(1))
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	logs.mu.Lock()
	logs.handler = slog.NewJSONHandler(errorWriter{err: errors.New("transient disk failure")}, nil)
	logs.stderr = io.Discard
	logs.mu.Unlock()
	if err := logs.Finish(t.Context(), trace, AttemptResult{
		Outcome:       OutcomeFailed,
		ErrorCategory: ErrorCategoryTransient,
		Err:           errors.New("network unavailable"),
	}); err == nil {
		t.Fatal("finish unexpectedly persisted through failing writer")
	}

	recheckContext, stopRechecking := context.WithCancel(t.Context())
	recheckDone := make(chan struct{})
	go func() {
		defer close(recheckDone)
		logs.RunRechecks(recheckContext, 5*time.Millisecond)
	}()
	waitContext, stopWaiting := context.WithTimeout(t.Context(), time.Second)
	defer stopWaiting()
	waitForHealthy(t, waitContext, logs)
	stopRechecking()
	<-recheckDone
	if health := logs.Health(); health.PendingTerminalRecords != 0 {
		t.Fatalf("pending terminal records after recovery = %d, want 0", health.PendingTerminalRecords)
	}
	records := readJSONL(t, path)
	if len(records) < 3 {
		t.Fatalf("record count after replay = %d, want at least startup, terminal, recovered", len(records))
	}
	assertJSONField(t, records[len(records)-2], "event", "external_attempt_finished")
	assertJSONField(t, records[len(records)-1], "event", "runlog_recovered")

	if _, err := logs.Start(t.Context(), testDiscoveryAttempt(2)); err != nil {
		t.Fatalf("start after automatic recovery: %v", err)
	}
}

func waitForHealthy(t *testing.T, ctx context.Context, logs *Log) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if logs.Health().Healthy {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for automatic recovery: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func testDiscoveryAttempt(attemptNo int64) Attempt {
	return Attempt{
		Flow:           FlowDiscovery,
		Operation:      OperationListPage,
		DiscoveryRunID: 42,
		AttemptNo:      attemptNo,
		PageNo:         1,
	}
}

func TestStartRejectsIncompleteExternalAttemptBeforeWriting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := Open(path)
	t.Cleanup(func() { _ = logs.Close() })
	_, err := logs.Start(t.Context(), Attempt{
		Flow:           FlowDiscovery,
		Operation:      OperationListPage,
		DiscoveryRunID: 42,
		AttemptNo:      1,
	})
	if err == nil {
		t.Fatal("incomplete list_page attempt was accepted")
	}
	if records := readJSONL(t, path); len(records) != 1 {
		t.Fatalf("record count = %d, want only startup record", len(records))
	}
}

func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open JSONL root: %v", err)
	}
	defer func() { _ = root.Close() }()
	content, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatalf("read JSONL: %v", err)
	}
	var records []map[string]any
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode JSONL line %d: %v", lineNumber+1, err)
		}
		records = append(records, record)
	}
	return records
}

func assertJSONField(t *testing.T, object map[string]any, field string, want any) {
	t.Helper()
	if got := object[field]; got != want {
		t.Errorf("%s = %#v, want %#v", field, got, want)
	}
}

func assertPrivateMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode %s = %04o, want %04o", path, got, want)
	}
}
