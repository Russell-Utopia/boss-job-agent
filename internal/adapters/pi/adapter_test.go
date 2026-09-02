package pi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestAdapterSubmitsOneIsolatedRPCRequestAndForwardsTheConfirmationTool(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	promptPath := filepath.Join(directory, "prompt.jsonl")
	executable := writeFakePi(t, directory, `
set -eu
IFS= read -r prompt
printf '%s\n' "$prompt" > "$1"
printf '%s\n' '{"id":"assessment-submit","type":"response","command":"prompt","success":true}'
curl -fsS \
  -H "Authorization: Bearer $BOSS_JOB_AGENT_CONFIRM_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"results":[{"jobId":41,"attemptNo":3,"status":"suitable","reason":"Go 经历明确匹配","evidence":{"matches":["Go 服务"]}}]}' \
  "$BOSS_JOB_AGENT_CONFIRM_URL" >/dev/null
printf '%s\n' '{"type":"agent_end"}'
`)
	confirmed := make(chan assessment.ConfirmationBatch, 1)
	adapter := NewWithConfig(Config{
		Executable: executable,
		Arguments:  []string{promptPath},
		RuntimeDir: filepath.Join(directory, "runtime"),
	}, func(_ context.Context, batch assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
		confirmed <- batch
		return assessment.ConfirmationReceipt{Results: []assessment.ConfirmationItemReceipt{{
			JobID: 41, AttemptNo: 3, Status: assessment.ConfirmationAccepted,
		}}}, nil
	})
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	request := assessment.AssessmentRequest{
		TraceID: "0123456789abcdef0123456789abcdef", ResumeVersion: 2,
		Resume: onlineresume.ResumeContent{
			JobIntentions:      []onlineresume.JobIntention{{Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职"}},
			WorkExperiences:    []string{"Go 后端工程师"},
			ProjectExperiences: []string{"招聘助手"},
			Educations:         []string{"计算机本科"},
			Skills:             []string{"Go"},
		},
		Policy: assessment.Policy{Version: 4, Name: "策略 v4", Rules: []string{"只依据完整输入"}},
		Jobs: []assessment.AssessmentJobInput{{
			JobID: 41, AttemptNo: 3, PlatformJobID: "boss-job-41",
			CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-41.html",
			JobTitle:     "Go 平台工程师", CompanyName: "示例科技", City: "福州", Salary: "25-35K",
			FullJD: "负责 Go 平台服务\n熟悉 Go 与 SQLite", JDHash: "jd-hash-41",
		}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := adapter.Submit(ctx, request); err != nil {
		t.Fatalf("submit assessment through fake Pi RPC: %v", err)
	}

	batch := <-confirmed
	assertForwardedConfirmationBatch(t, batch, request.TraceID)
	promptBytes, err := os.ReadFile(promptPath) //nolint:gosec // The path is created inside this test's private temporary directory.
	if err != nil {
		t.Fatalf("read submitted Pi prompt: %v", err)
	}
	var command struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(promptBytes, &command); err != nil {
		t.Fatalf("decode submitted Pi command: %v", err)
	}
	for _, want := range []string{
		`"resumeVersion":2`, `"workExperiences":["Go 后端工程师"]`,
		`"version":4`, `"jobId":41`, `"attemptNo":3`,
		`"fullJD":"负责 Go 平台服务\n熟悉 Go 与 SQLite"`, `confirm_assessment_results`,
	} {
		if !strings.Contains(command.Message, want) {
			t.Errorf("Pi prompt does not contain %q: %s", want, command.Message)
		}
	}
}

func assertForwardedConfirmationBatch(t *testing.T, batch assessment.ConfirmationBatch, traceID string) {
	t.Helper()
	if batch.TraceID != traceID {
		t.Fatalf("forwarded trace ID = %q, want %q", batch.TraceID, traceID)
	}
	if len(batch.Results) != 1 || batch.Results[0].JobID != 41 || batch.Results[0].AttemptNo != 3 ||
		batch.Results[0].Status != jobpool.AssessmentStatusSuitable {
		t.Fatalf("forwarded confirmation = %#v", batch)
	}
}

func TestAdapterRejectsOrdinaryModelTextWithoutAConfirmationToolCall(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	executable := writeFakePi(t, directory, `
set -eu
IFS= read -r prompt
printf '%s\n' '{"id":"assessment-submit","type":"response","command":"prompt","success":true}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"suitable"}]}}'
printf '%s\n' '{"type":"agent_end"}'
`)
	adapter := NewWithConfig(Config{Executable: executable, RuntimeDir: filepath.Join(directory, "runtime")}, func(
		context.Context,
		assessment.ConfirmationBatch,
	) (assessment.ConfirmationReceipt, error) {
		t.Fatal("ordinary model text reached the confirmation handler")
		return assessment.ConfirmationReceipt{}, nil
	})
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	err := adapter.Submit(t.Context(), assessment.AssessmentRequest{
		TraceID: "0123456789abcdef0123456789abcdef",
		Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "without calling confirm_assessment_results") {
		t.Fatalf("ordinary model text result = %v, want missing confirmation error", err)
	}
	assertSubmissionErrorCategory(t, err, assessment.SubmissionErrorInvalidProtocol)
}

func TestAdapterRejectsTrailingJSONWithoutCallingTheConfirmer(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	executable := writeFakePi(t, directory, `
set -eu
IFS= read -r prompt
printf '%s\n' '{"id":"assessment-submit","type":"response","command":"prompt","success":true}'
status=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $BOSS_JOB_AGENT_CONFIRM_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary '{"results":[]} {"results":[]}' \
  "$BOSS_JOB_AGENT_CONFIRM_URL")
test "$status" = 400
printf '%s\n' '{"type":"agent_end"}'
`)
	adapter := NewWithConfig(Config{Executable: executable, RuntimeDir: filepath.Join(directory, "runtime")}, func(
		context.Context,
		assessment.ConfirmationBatch,
	) (assessment.ConfirmationReceipt, error) {
		t.Fatal("malformed callback body reached the confirmation handler")
		return assessment.ConfirmationReceipt{}, nil
	})
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	err := adapter.Submit(t.Context(), assessment.AssessmentRequest{
		TraceID: "0123456789abcdef0123456789abcdef",
		Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "without calling confirm_assessment_results") {
		t.Fatalf("trailing JSON result = %v, want missing confirmation error", err)
	}
	assertSubmissionErrorCategory(t, err, assessment.SubmissionErrorInvalidProtocol)
}

func TestAdapterClassifiesProcessStartFailureAsTransient(t *testing.T) {
	t.Parallel()

	adapter := NewWithConfig(
		Config{Executable: filepath.Join(t.TempDir(), "missing-pi"), RuntimeDir: filepath.Join(t.TempDir(), "runtime")},
		func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
			return assessment.ConfirmationReceipt{}, nil
		},
	)
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	err := adapter.Submit(t.Context(), assessment.AssessmentRequest{
		TraceID: "0123456789abcdef0123456789abcdef",
		Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
	})
	assertSubmissionErrorCategory(t, err, assessment.SubmissionErrorTransient)
}

func TestAdapterTreatsContextInterruptionAsTransientAndRemovesItsProcessMarker(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	runtimeDirectory := filepath.Join(directory, "runtime")
	executable := writeFakePi(t, directory, `
set -eu
IFS= read -r prompt
while IFS= read -r ignored; do :; done
`)
	adapter := NewWithConfig(Config{
		Executable: executable,
		RuntimeDir: runtimeDirectory,
	}, func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
		return assessment.ConfirmationReceipt{}, nil
	})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := adapter.Submit(ctx, assessment.AssessmentRequest{
		TraceID: "0123456789abcdef0123456789abcdef",
		Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
	})
	assertSubmissionErrorCategory(t, err, assessment.SubmissionErrorTransient)
	assertNoProcessMarkers(t, runtimeDirectory)
}

func TestAdapterCloseLetsAnOwnedPiExitGracefullyBeforeItEscalates(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	runtimeDirectory := filepath.Join(directory, "runtime")
	gracefulPath := filepath.Join(directory, "graceful")
	executable := writeFakePi(t, directory, fmt.Sprintf(`
set -eu
IFS= read -r prompt
while IFS= read -r ignored; do :; done
printf 'graceful' > %q
`, gracefulPath))
	adapter := NewWithConfig(Config{
		Executable: executable,
		RuntimeDir: runtimeDirectory,
	}, func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
		return assessment.ConfirmationReceipt{}, nil
	})

	result := make(chan error, 1)
	go func() {
		result <- adapter.Submit(t.Context(), assessment.AssessmentRequest{
			TraceID: "0123456789abcdef0123456789abcdef",
			Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
		})
	}()
	waitForProcessMarker(t, runtimeDirectory)
	if err := adapter.Close(t.Context()); err != nil {
		t.Fatalf("close owned Pi: %v", err)
	}
	if err := <-result; err == nil {
		t.Fatal("submit after graceful Pi shutdown succeeded, want interruption failure")
	}
	if _, err := os.Stat(gracefulPath); err != nil {
		t.Fatalf("graceful shutdown marker: %v", err)
	}
	assertNoProcessMarkers(t, runtimeDirectory)
}

func TestAdapterCloseEscalatesOnlyAfterGracefulShutdownFails(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	runtimeDirectory := filepath.Join(directory, "runtime")
	executable := writeFakePi(t, directory, `
set -eu
trap '' INT
IFS= read -r prompt
while :; do :; done
`)
	adapter := NewWithConfig(Config{
		Executable: executable,
		RuntimeDir: runtimeDirectory,
	}, func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
		return assessment.ConfirmationReceipt{}, nil
	})

	result := make(chan error, 1)
	go func() {
		result <- adapter.Submit(t.Context(), assessment.AssessmentRequest{
			TraceID: "0123456789abcdef0123456789abcdef",
			Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
		})
	}()
	waitForProcessMarker(t, runtimeDirectory)
	if err := adapter.Close(t.Context()); err != nil {
		t.Fatalf("close Pi after escalation: %v", err)
	}
	if err := <-result; err == nil {
		t.Fatal("submit after forced Pi shutdown succeeded, want interruption failure")
	}
	assertNoProcessMarkers(t, runtimeDirectory)
}

func TestAdapterRecoveryPreservesAProcessWhoseIdentityCannotBeVerified(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	runtimeDirectory := filepath.Join(directory, "runtime")
	executable := writeFakePi(t, directory, `
set -eu
IFS= read -r prompt
while IFS= read -r ignored; do :; done
`)
	owner := NewWithConfig(Config{
		Executable: executable,
		RuntimeDir: runtimeDirectory,
	}, func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
		return assessment.ConfirmationReceipt{}, nil
	})
	result := make(chan error, 1)
	go func() {
		result <- owner.Submit(t.Context(), assessment.AssessmentRequest{
			TraceID: "0123456789abcdef0123456789abcdef",
			Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
		})
	}()
	markerPath := waitForProcessMarker(t, runtimeDirectory)
	marker := readOwnedProcessMarker(t, markerPath)

	restarted := NewWithConfig(Config{
		RuntimeDir: runtimeDirectory,
		Inspector: fixedProcessInspector{identity: ProcessIdentity{
			StartTime:  marker.ProcessStartTime,
			Executable: marker.Executable + ".different",
		}},
	}, func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
		return assessment.ConfirmationReceipt{}, nil
	})
	if err := restarted.Recover(t.Context()); err == nil {
		t.Fatal("recovery with mismatched executable succeeded, want ownership error")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("mismatched marker was removed: %v", err)
	}

	if err := owner.Close(t.Context()); err != nil {
		t.Fatalf("clean up owned Pi after failed recovery: %v", err)
	}
	if err := <-result; err == nil {
		t.Fatal("owner submission succeeded after close, want interruption failure")
	}
}

func readOwnedProcessMarker(t *testing.T, path string) processMarker {
	t.Helper()
	markerBytes, err := os.ReadFile(path) //nolint:gosec // The marker is created inside this test's private temporary directory.
	if err != nil {
		t.Fatalf("read owned Pi marker: %v", err)
	}
	var marker processMarker
	if err := json.Unmarshal(markerBytes, &marker); err != nil {
		t.Fatalf("decode owned Pi marker: %v", err)
	}
	if marker.ApplicationInstance == "" || marker.Worker == "" || marker.PID <= 0 ||
		marker.ProcessStartTime == "" || marker.Executable == "" || marker.RunToken == "" {
		t.Fatalf("owned Pi marker is incomplete: %#v", marker)
	}
	return marker
}

func TestAdapterRecoveryStopsTheMatchingOwnedProcessAfterRestart(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	runtimeDirectory := filepath.Join(directory, "runtime")
	executable := writeFakePi(t, directory, `
set -eu
trap '' INT
IFS= read -r prompt || true
while :; do :; done
`)
	owner := NewWithConfig(Config{
		Executable: executable,
		RuntimeDir: runtimeDirectory,
	}, func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error) {
		return assessment.ConfirmationReceipt{}, nil
	})
	result := make(chan error, 1)
	go func() {
		result <- owner.Submit(t.Context(), assessment.AssessmentRequest{
			TraceID: "0123456789abcdef0123456789abcdef",
			Jobs:    []assessment.AssessmentJobInput{{JobID: 1, AttemptNo: 1}},
		})
	}()
	markerPath := waitForProcessMarker(t, runtimeDirectory)

	restarted := NewWithConfig(Config{RuntimeDir: runtimeDirectory}, func(
		context.Context,
		assessment.ConfirmationBatch,
	) (assessment.ConfirmationReceipt, error) {
		return assessment.ConfirmationReceipt{}, nil
	})
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("recover matching owned Pi: %v", err)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("matching marker after recovery = %v, want removed", err)
	}
	if err := <-result; err == nil {
		t.Fatal("owner submission succeeded after restart recovery, want interruption failure")
	}
}

type fixedProcessInspector struct {
	identity ProcessIdentity
	err      error
}

func (i fixedProcessInspector) Inspect(int) (ProcessIdentity, error) {
	return i.identity, i.err
}

func waitForProcessMarker(t *testing.T, directory string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(directory)
		if err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), processMarkerPrefix) && strings.HasSuffix(entry.Name(), processMarkerSuffix) {
					return filepath.Join(directory, entry.Name())
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a Pi process marker in %s", directory)
	return ""
}

func assertNoProcessMarkers(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read Pi runtime directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), processMarkerPrefix) && strings.HasSuffix(entry.Name(), processMarkerSuffix) {
			t.Errorf("Pi process marker %q remains after process exit", entry.Name())
		}
	}
}

func assertSubmissionErrorCategory(
	t *testing.T,
	err error,
	want assessment.SubmissionErrorCategory,
) {
	t.Helper()
	var submissionError *assessment.SubmissionError
	if !errors.As(err, &submissionError) {
		t.Fatalf("submission error = %v, want categorized error %q", err, want)
	}
	if submissionError.Category != want {
		t.Errorf("submission error category = %q, want %q", submissionError.Category, want)
	}
}

type confirmationIntegrationFixture struct {
	service *assessment.Service
	pool    *jobpool.Pool
	jobs    []jobpool.JobView
}

func seedConfirmationIntegrationInputs(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	resumeResult, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (1, '{"jobIntentions":[],"workExperiences":[],"projectExperiences":[],"educations":[],"skills":[]}', 'resume-1', 1, 1000)
	`)
	if err != nil {
		t.Fatalf("seed resume version: %v", err)
	}
	resumeID, err := resumeResult.LastInsertId()
	if err != nil {
		t.Fatalf("read resume version ID: %v", err)
	}
	policyResult, err := db.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (1, '{"rules":["只依据输入"]}', 1, 'test', 1000)
	`)
	if err != nil {
		t.Fatalf("seed policy version: %v", err)
	}
	policyID, err := policyResult.LastInsertId()
	if err != nil {
		t.Fatalf("read policy version ID: %v", err)
	}
	return resumeID, policyID
}

func newConfirmationIntegrationFixture(t *testing.T) confirmationIntegrationFixture {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(filepath.Join(t.TempDir(), "pi-confirmation.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	pool := jobpool.New(db)
	jobs := make([]jobpool.JobView, 3)
	for index := range jobs {
		jobs[index], err = pool.Observe(t.Context(), 1, jobpool.Observation{
			PlatformJobID: fmt.Sprintf("pi-confirmation-job-%d", index+1),
			CanonicalURL: fmt.Sprintf(
				"https://www.zhipin.com/job_detail/pi-confirmation-job-%d.html",
				index+1,
			),
			JobTitle: "Go 工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
			FullJD:         "负责 Go 服务\n熟悉 Go",
			PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(int64(1_000 + index)),
		})
		if err != nil {
			t.Fatalf("observe job %d: %v", index+1, err)
		}
	}
	resumeID, policyID := seedConfirmationIntegrationInputs(t, db)
	queued, err := pool.QueueAssessments(t.Context(), []int64{jobs[0].ID, jobs[1].ID, jobs[2].ID})
	if err != nil || queued.Succeeded != 3 {
		t.Fatalf("queue assessments: result=%#v err=%v", queued, err)
	}
	work, err := pool.ClaimAssessments(t.Context(), jobpool.AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 3, ClaimedAt: time.UnixMilli(2_000), LeaseUntil: time.UnixMilli(12_000),
	})
	if err != nil || len(work) != 3 {
		t.Fatalf("claim assessments: work=%#v err=%v", work, err)
	}
	return confirmationIntegrationFixture{
		service: assessment.New(db, nil, pool, nil, nil, nil, logs, func() time.Time { return time.UnixMilli(3_000) }),
		pool:    pool, jobs: jobs,
	}
}

func TestControlledAdapterIsolatesMalformedAndOutOfRequestItems(t *testing.T) {
	t.Parallel()

	fixture := newConfirmationIntegrationFixture(t)
	directory := t.TempDir()
	executable := writeFakePi(t, directory, fmt.Sprintf(`
set -eu
IFS= read -r prompt
printf '%%s\n' '{"id":"assessment-submit","type":"response","command":"prompt","success":true}'
curl -fsS \
  -H "Authorization: Bearer $BOSS_JOB_AGENT_CONFIRM_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"results":[{"jobId":%d,"attemptNo":1,"status":"suitable","reason":"Go 经历匹配","evidence":{"matches":["Go"]}},{"jobId":%d,"attemptNo":1,"status":42,"reason":"类型错误","evidence":{}},{"jobId":%d,"attemptNo":1,"status":"suitable","reason":"伪报其他请求","evidence":{"matches":["Go"]}}]}' \
  "$BOSS_JOB_AGENT_CONFIRM_URL" >/dev/null
printf '%%s\n' '{"type":"agent_end"}'
`, fixture.jobs[0].ID, fixture.jobs[1].ID, fixture.jobs[2].ID))
	adapter := NewWithConfig(Config{Executable: executable, RuntimeDir: filepath.Join(directory, "runtime")}, fixture.service.Confirm)
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	if err := adapter.Submit(t.Context(), assessment.AssessmentRequest{
		TraceID: "0123456789abcdef0123456789abcdef",
		Jobs: []assessment.AssessmentJobInput{
			{JobID: fixture.jobs[0].ID, AttemptNo: 1},
			{JobID: fixture.jobs[1].ID, AttemptNo: 1},
		},
	}); err != nil {
		t.Fatalf("submit mixed confirmation batch: %v", err)
	}
	accepted, err := fixture.pool.GetJob(t.Context(), fixture.jobs[0].ID)
	if err != nil {
		t.Fatalf("get accepted assessment: %v", err)
	}
	if accepted.AssessmentStatus != jobpool.AssessmentStatusSuitable {
		t.Fatalf("valid item status = %q, want suitable", accepted.AssessmentStatus)
	}
	invalid, err := fixture.pool.GetJob(t.Context(), fixture.jobs[1].ID)
	if err != nil {
		t.Fatalf("get invalid assessment: %v", err)
	}
	if invalid.AssessmentStatus != jobpool.AssessmentStatusProcessing {
		t.Fatalf("malformed item status = %q, want unchanged processing", invalid.AssessmentStatus)
	}
	outOfRequest, err := fixture.pool.GetJob(t.Context(), fixture.jobs[2].ID)
	if err != nil {
		t.Fatalf("get out-of-request assessment: %v", err)
	}
	if outOfRequest.AssessmentStatus != jobpool.AssessmentStatusProcessing {
		t.Fatalf("out-of-request item status = %q, want unchanged processing", outOfRequest.AssessmentStatus)
	}
}

func TestConfirmationExtensionDefersItemValidationToTheGoService(t *testing.T) {
	t.Parallel()

	if !strings.Contains(string(confirmationExtension), "Type.Array(Type.Unknown()") {
		t.Fatal("confirmation extension validates item fields before Service.Confirm")
	}
	if strings.Contains(string(confirmationExtension), "maxItems") {
		t.Fatal("confirmation extension caps the result batch instead of following the configured processing limit")
	}
}

func writeFakePi(t *testing.T, directory, body string) string {
	t.Helper()
	path := filepath.Join(directory, "fake-pi")
	content := "#!/bin/sh\n" + strings.TrimSpace(body) + "\n"
	// The private test script must be executable by exec.CommandContext.
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil { //nolint:gosec // Executability is required for this controlled test fixture.
		t.Fatalf("write fake Pi: %v", err)
	}
	return path
}
