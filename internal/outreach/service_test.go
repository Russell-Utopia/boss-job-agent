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

func TestServiceAutomaticallyAdmitsEligibleOutreachWhenEnabled(t *testing.T) {
	t.Parallel()

	service, pool, settings, adapter, _ := openOutreachServiceTest(t)
	job := eligibleOutreachTestJob(t, pool, "outreach-automatic")
	configureAutomaticOutreachTest(t, settings, "您好，想和您聊聊", nil)
	adapter.checkStatus = ContactStatus{Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	adapter.sendResult = FirstContactResult{Effect: OutreachEffectConfirmedSent, Evidence: json.RawMessage(`{"sent":true}`)}

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(5000)); err != nil {
		t.Fatalf("run automatic outreach cycle: %v", err)
	}
	if len(adapter.checks) != 1 || len(adapter.sends) != 1 {
		t.Fatalf("automatic adapter calls = checks %d, sends %d; want one check and one send", len(adapter.checks), len(adapter.sends))
	}
	if adapter.sends[0].GreetingText != "您好，想和您聊聊" {
		t.Errorf("automatic greeting = %q, want configured greeting", adapter.sends[0].GreetingText)
	}
	view := mustGetOutreachJob(t, pool, job.ID)
	if view.OutreachStatus != jobpool.OutreachStatusContacted {
		t.Errorf("automatic outreach status = %q, want contacted", view.OutreachStatus)
	}
}

func TestServiceContinuesExistingOutreachAfterAutomaticSettingIsDisabled(t *testing.T) {
	t.Parallel()

	service, pool, settings, adapter, _ := openOutreachServiceTest(t)
	job := eligibleOutreachTestJob(t, pool, "outreach-automatic-disabled")
	window := []automationsettings.OutreachTimeWindow{{Start: "09:00", End: "10:00"}}
	configureAutomaticOutreachTest(t, settings, "您好，想和您聊聊", window)
	// 08:00 Asia/Shanghai is outside the window: admission is allowed, but
	// starting the external action is not.
	if err := service.runSchedulingCycle(t.Context(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("run automatic admission cycle: %v", err)
	}
	if view := mustGetOutreachJob(t, pool, job.ID); view.OutreachStatus != jobpool.OutreachStatusPending {
		t.Fatalf("automatically admitted outreach status = %q, want pending", view.OutreachStatus)
	}

	if err := settings.ConfigureOutreach(t.Context(), false, "您好，想和您聊聊", window); err != nil {
		t.Fatalf("disable automatic outreach: %v", err)
	}
	adapter.checkStatus = ContactStatus{Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)}
	adapter.sendResult = FirstContactResult{Effect: OutreachEffectConfirmedSent, Evidence: json.RawMessage(`{"sent":true}`)}
	if err := service.runSchedulingCycle(t.Context(), time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("run existing queue cycle: %v", err)
	}
	if len(adapter.checks) != 1 || len(adapter.sends) != 1 {
		t.Fatalf("existing queue adapter calls = checks %d, sends %d; want one check and one send", len(adapter.checks), len(adapter.sends))
	}
	if adapter.sends[0].GreetingText != "您好，想和您聊聊" {
		t.Errorf("existing queue greeting = %q, want greeting frozen at admission", adapter.sends[0].GreetingText)
	}
	if view := mustGetOutreachJob(t, pool, job.ID); view.OutreachStatus != jobpool.OutreachStatusContacted {
		t.Errorf("existing queue outreach status = %q, want contacted", view.OutreachStatus)
	}
}

func TestAutomaticOutreachAndFrozenGreetingSurviveServiceRestart(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "outreach.db")
	jobID := persistAutomaticOutreachBeforeRestart(t, databasePath)
	runRestartedAutomaticOutreach(t, databasePath, jobID)
}

func persistAutomaticOutreachBeforeRestart(t *testing.T, databasePath string) int64 {
	t.Helper()

	db, err := storage.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("open initial sqlite: %v", err)
	}
	p := jobpool.New(db)
	settings := automationsettings.New(db, p)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure initial settings: %v", err)
	}
	job := eligibleOutreachTestJob(t, p, "outreach-restart")
	window := []automationsettings.OutreachTimeWindow{{Start: "09:00", End: "10:00"}}
	configureAutomaticOutreachTest(t, settings, "重启前的完整招呼语", window)
	initialLogs := runlog.Open(filepath.Join(t.TempDir(), "initial.jsonl"))
	initialAdapter := &controlledOutreachAdapter{}
	initialService := newService(p, settings, initialAdapter, initialLogs, func() time.Time {
		return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	})
	if err := initialService.runSchedulingCycle(t.Context(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("run initial automatic admission: %v", err)
	}
	if view := mustGetOutreachJob(t, p, job.ID); view.OutreachStatus != jobpool.OutreachStatusPending || view.OutreachGreetingText != "重启前的完整招呼语" {
		t.Fatalf("persisted automatic queue = %#v, want pending with frozen greeting", view)
	}
	if err := initialLogs.Close(); err != nil {
		t.Fatalf("close initial logs: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close initial sqlite: %v", err)
	}
	return job.ID
}

func runRestartedAutomaticOutreach(t *testing.T, databasePath string, jobID int64) {
	t.Helper()

	restartedDB, err := storage.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	t.Cleanup(func() { _ = restartedDB.Close() })
	restartedPool := jobpool.New(restartedDB)
	restartedSettings := automationsettings.New(restartedDB, restartedPool)
	restartedLogs := runlog.Open(filepath.Join(t.TempDir(), "restarted.jsonl"))
	t.Cleanup(func() { _ = restartedLogs.Close() })
	restartedAdapter := &controlledOutreachAdapter{
		checkStatus: ContactStatus{Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`)},
		sendResult:  FirstContactResult{Effect: OutreachEffectConfirmedSent, Evidence: json.RawMessage(`{"sent":true}`)},
	}
	restartedService := newService(restartedPool, restartedSettings, restartedAdapter, restartedLogs, func() time.Time {
		return time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	})
	if err := restartedService.runSchedulingCycle(t.Context(), time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("run restarted outreach cycle: %v", err)
	}
	if len(restartedAdapter.sends) != 1 || restartedAdapter.sends[0].GreetingText != "重启前的完整招呼语" {
		t.Fatalf("restarted send = %#v, want frozen greeting", restartedAdapter.sends)
	}
	if view := mustGetOutreachJob(t, restartedPool, jobID); view.OutreachStatus != jobpool.OutreachStatusContacted {
		t.Errorf("restarted outreach status = %q, want contacted", view.OutreachStatus)
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

func eligibleOutreachTestJob(t *testing.T, pool *jobpool.Pool, platformID string) jobpool.JobView {
	t.Helper()
	job, err := pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: platformID, CanonicalURL: "https://www.zhipin.com/job_detail/" + platformID + ".html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatalf("observe eligible outreach job: %v", err)
	}
	if err := pool.Review(t.Context(), []jobpool.ReviewDecision{{JobID: job.ID, ExpectedJDHash: job.JDHash, Verdict: jobpool.HumanVerdictSuitable}}); err != nil {
		t.Fatalf("review eligible outreach job: %v", err)
	}
	return job
}

func configureAutomaticOutreachTest(t *testing.T, settings *automationsettings.Settings, greeting string, windows []automationsettings.OutreachTimeWindow) {
	t.Helper()
	impact, err := settings.PreviewOutreachConfiguration(t.Context(), true, greeting, windows)
	if err != nil {
		t.Fatalf("preview automatic outreach: %v", err)
	}
	err = settings.ConfigureOutreachWithConfirmation(t.Context(), true, greeting, windows, automationsettings.OutreachSettingsConfirmation{
		EligibleJobCount: impact.EligibleJobCount,
		GreetingText:     impact.GreetingText,
		TimeDescription:  impact.TimeDescription,
		Confirmed:        true,
	})
	if err != nil {
		t.Fatalf("enable automatic outreach: %v", err)
	}
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
