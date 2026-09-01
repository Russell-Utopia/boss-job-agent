package outreach

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type controlledOutreachAdapter struct {
	checkStatus ContactStatus
	checkErr    error
	sendResult  FirstContactResult
	sendErr     error
	checks      []PlatformJobRef
	sends       []FirstContactRequest
}

func (a *controlledOutreachAdapter) Check(_ context.Context, ref PlatformJobRef) (ContactStatus, error) {
	a.checks = append(a.checks, ref)
	return a.checkStatus, a.checkErr
}

func (a *controlledOutreachAdapter) Send(_ context.Context, request FirstContactRequest) (FirstContactResult, error) {
	a.sends = append(a.sends, request)
	return a.sendResult, a.sendErr
}

func openOutreachServiceTest(t *testing.T) (*Service, *jobpool.Pool, *automationsettings.Settings, *controlledOutreachAdapter, *sql.DB) {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := jobpool.New(db)
	settings := automationsettings.New(db, p)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}
	logs := runlog.Open(filepath.Join(t.TempDir(), "outreach.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	adapter := &controlledOutreachAdapter{}
	now := func() time.Time { return time.UnixMilli(5000) }
	return newService(p, settings, adapter, logs, now), p, settings, adapter, db
}

func TestServiceSendsOnlyAfterFreshCheckAndPersistsConfirmedContact(t *testing.T) {
	t.Parallel()

	service, pool, _, adapter, _ := openOutreachServiceTest(t)
	job := queueOutreachTestJob(t, pool, "outreach-send", "您好，想和您聊聊")
	adapter.checkStatus = ContactStatus{Open: true, AlreadyContacted: false, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	adapter.sendResult = FirstContactResult{Effect: OutreachEffectConfirmedSent, Evidence: json.RawMessage(`{"sent":true}`)}

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(5000)); err != nil {
		t.Fatalf("run outreach cycle: %v", err)
	}
	if len(adapter.checks) != 1 || len(adapter.sends) != 1 {
		t.Fatalf("adapter calls = checks %d, sends %d; want one check and one send", len(adapter.checks), len(adapter.sends))
	}
	if adapter.sends[0].GreetingText != "您好，想和您聊聊" {
		t.Errorf("sent greeting = %q, want frozen greeting", adapter.sends[0].GreetingText)
	}
	view := mustGetOutreachJob(t, pool, job.ID)
	if view.OutreachStatus != jobpool.OutreachStatusContacted || view.ContactSource != jobpool.ContactSourceAgent {
		t.Errorf("contacted job = %#v, want agent-confirmed contact", view)
	}
}

func TestServiceRecordsExistingBossContactWithoutSending(t *testing.T) {
	t.Parallel()

	service, pool, _, adapter, _ := openOutreachServiceTest(t)
	job := queueOutreachTestJob(t, pool, "outreach-existing", "您好")
	adapter.checkStatus = ContactStatus{Open: true, AlreadyContacted: true, Evidence: json.RawMessage(`{"open":true,"contacted":true}`)}

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(5000)); err != nil {
		t.Fatalf("run outreach reconciliation cycle: %v", err)
	}
	if len(adapter.checks) != 1 || len(adapter.sends) != 0 {
		t.Fatalf("adapter calls = checks %d, sends %d; want check only", len(adapter.checks), len(adapter.sends))
	}
	view := mustGetOutreachJob(t, pool, job.ID)
	if view.OutreachStatus != jobpool.OutreachStatusContacted || view.ContactSource != jobpool.ContactSourceBossExisting {
		t.Errorf("existing contact job = %#v, want boss-existing contact", view)
	}
}

func TestServiceDoesNotReprocessAContactedJob(t *testing.T) {
	t.Parallel()

	service, pool, _, adapter, _ := openOutreachServiceTest(t)
	job := queueOutreachTestJob(t, pool, "outreach-dedup", "您好")
	work := mustClaimOutreach(t, pool, time.UnixMilli(5000))
	if _, err := pool.FinishOutreach(t.Context(), []jobpool.OutreachOutcome{{
		JobID: work[0].JobID, AttemptNo: work[0].AttemptNo, Status: jobpool.OutreachStatusContacted,
		ContactSource: jobpool.ContactSourceAgent, Evidence: json.RawMessage(`{"sent":true}`), CompletedAt: time.UnixMilli(5000),
	}}); err != nil {
		t.Fatalf("seed contacted outreach job: %v", err)
	}
	if view := mustGetOutreachJob(t, pool, job.ID); view.OutreachStatus != jobpool.OutreachStatusContacted {
		t.Fatalf("seeded contacted status = %q", view.OutreachStatus)
	}

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(6000)); err != nil {
		t.Fatalf("run contacted outreach cycle: %v", err)
	}
	if len(adapter.checks) != 0 || len(adapter.sends) != 0 {
		t.Fatalf("adapter calls for contacted job = checks %d, sends %d; want none", len(adapter.checks), len(adapter.sends))
	}
}

func TestServiceReconcilesPossiblyContactedBeforeAllowingAnyRetry(t *testing.T) {
	t.Parallel()

	service, pool, _, adapter, _ := openOutreachServiceTest(t)
	job := queueOutreachTestJob(t, pool, "outreach-uncertain", "您好")
	adapter.checkStatus = ContactStatus{Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	adapter.sendResult = FirstContactResult{Effect: OutreachEffectPossiblyEffective, Evidence: json.RawMessage(`{"uncertain":true}`)}
	adapter.sendErr = errors.New("connection closed after click")

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(5000)); err != nil {
		t.Fatalf("run uncertain outreach cycle: %v", err)
	}
	view := mustGetOutreachJob(t, pool, job.ID)
	if view.OutreachStatus != jobpool.OutreachStatusPossiblyContacted {
		t.Fatalf("uncertain job status = %q, want possibly_contacted", view.OutreachStatus)
	}

	adapter.sendErr = nil
	adapter.checkStatus = ContactStatus{Open: true, AlreadyContacted: false, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(6000)); err != nil {
		t.Fatalf("run reconciliation cycle: %v", err)
	}
	if len(adapter.sends) != 1 {
		t.Errorf("send calls after reconciliation = %d, want still one", len(adapter.sends))
	}
	view = mustGetOutreachJob(t, pool, job.ID)
	if view.OutreachStatus != jobpool.OutreachStatusFailed {
		t.Errorf("reconciled no-contact status = %q, want failed", view.OutreachStatus)
	}
}

func TestServiceDoesNotClaimNewContactOutsideConfiguredWindow(t *testing.T) {
	t.Parallel()

	service, pool, settings, adapter, _ := openOutreachServiceTest(t)
	job := queueOutreachTestJob(t, pool, "outreach-window", "您好")
	if err := settings.ConfigureOutreach(t.Context(), false, "您好", []automationsettings.OutreachTimeWindow{{Start: "09:00", End: "10:00"}}); err != nil {
		t.Fatalf("configure outreach window: %v", err)
	}
	adapter.checkStatus = ContactStatus{Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	adapter.sendResult = FirstContactResult{Effect: OutreachEffectConfirmedSent, Evidence: json.RawMessage(`{"sent":true}`)}

	// 13:00 Asia/Shanghai is outside the configured daily window.
	if err := service.runSchedulingCycle(t.Context(), time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("run outside-window cycle: %v", err)
	}
	if len(adapter.checks) != 0 || len(adapter.sends) != 0 {
		t.Fatalf("adapter calls outside window = checks %d, sends %d; want none", len(adapter.checks), len(adapter.sends))
	}
	if view := mustGetOutreachJob(t, pool, job.ID); view.OutreachStatus != jobpool.OutreachStatusPending {
		t.Errorf("outside-window job status = %q, want pending", view.OutreachStatus)
	}
}

func TestServiceRetriesOnlyConfirmedNoEffectTransientFailures(t *testing.T) {
	t.Parallel()

	service, pool, _, adapter, _ := openOutreachServiceTest(t)
	job := queueOutreachTestJob(t, pool, "outreach-transient", "您好")
	adapter.checkStatus = ContactStatus{Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	adapter.sendResult = FirstContactResult{Effect: OutreachEffectConfirmedNoEffect, Evidence: json.RawMessage(`{"sent":false}`)}
	adapter.sendErr = &ActionError{Category: ErrorTransient, Err: fmt.Errorf("temporary BOSS outage")}

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(5000)); err != nil {
		t.Fatalf("run transient failure cycle: %v", err)
	}
	view := mustGetOutreachJob(t, pool, job.ID)
	if view.OutreachStatus != jobpool.OutreachStatusFailed || view.OutreachAction.Allowed {
		t.Errorf("transient failure job = %#v, want failed and no direct queue action", view)
	}
	if view.OutreachEvidence == nil || !json.Valid(view.OutreachEvidence) {
		t.Errorf("transient failure evidence = %s, want JSON", view.OutreachEvidence)
	}
}

func TestServicePersistsConfirmedNoEffectAsFailureWithoutRetry(t *testing.T) {
	t.Parallel()

	service, pool, _, adapter, _ := openOutreachServiceTest(t)
	job := queueOutreachTestJob(t, pool, "outreach-no-effect", "您好")
	adapter.checkStatus = ContactStatus{Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	adapter.sendResult = FirstContactResult{
		Effect:   OutreachEffectConfirmedNoEffect,
		Evidence: json.RawMessage(`{"sent":false,"reason":"chat_unavailable"}`),
	}

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(5000)); err != nil {
		t.Fatalf("run confirmed no-effect cycle: %v", err)
	}
	view := mustGetOutreachJob(t, pool, job.ID)
	if view.OutreachStatus != jobpool.OutreachStatusFailed {
		t.Fatalf("confirmed no-effect job status = %q, want failed", view.OutreachStatus)
	}
	if view.OutreachAction.Allowed {
		t.Error("confirmed no-effect job has a direct outreach action, want explicit retry required")
	}
}

func queueOutreachTestJob(t *testing.T, pool *jobpool.Pool, platformID, greeting string) jobpool.JobView {
	t.Helper()
	job, err := pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: platformID, CanonicalURL: "https://www.zhipin.com/job_detail/" + platformID + ".html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatalf("observe outreach job: %v", err)
	}
	if err := pool.Review(t.Context(), []jobpool.ReviewDecision{{JobID: job.ID, ExpectedJDHash: job.JDHash, Verdict: jobpool.HumanVerdictSuitable}}); err != nil {
		t.Fatalf("review outreach job: %v", err)
	}
	result, err := pool.QueueAuthorizedOutreach(t.Context(), []int64{job.ID}, jobpool.OutreachAuthorization{GreetingText: greeting, TimeDescription: "全天可打招呼"})
	if err != nil || result.Succeeded != 1 {
		t.Fatalf("queue outreach job = %#v, err=%v", result, err)
	}
	return job
}

func mustClaimOutreach(t *testing.T, pool *jobpool.Pool, claimedAt time.Time) []jobpool.OutreachWork {
	t.Helper()
	work, err := pool.ClaimOutreach(t.Context(), jobpool.OutreachClaim{
		Worker: "outreach-test-worker", Limit: 1, ClaimedAt: claimedAt, LeaseUntil: claimedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("claim outreach work: %v", err)
	}
	if len(work) != 1 {
		t.Fatalf("claimed outreach work = %d, want 1", len(work))
	}
	return work
}

func mustGetOutreachJob(t *testing.T, pool *jobpool.Pool, jobID int64) jobpool.JobView {
	t.Helper()
	job, err := pool.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatalf("get outreach job: %v", err)
	}
	return job
}
