package automationsettings

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func openTestSettings(t *testing.T) (*Settings, *sql.DB) {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, jobpool.New(db)), db
}

func TestSafeAutomationSettingsAreReadyOnFirstUse(t *testing.T) {
	t.Parallel()

	settings, _ := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}

	view, err := settings.Get(t.Context())
	if err != nil {
		t.Fatalf("get safe defaults: %v", err)
	}
	assertSafeDefaults(t, view)
}

func TestSafeDefaultInitializationPreservesSavedAutomationSettings(t *testing.T) {
	t.Parallel()

	settings, db := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		UPDATE automation_settings
		SET automatic_assessment_enabled = 1,
			assessment_processing_limit = 12,
			automatic_outreach_enabled = 1,
			outreach_greeting_text = '您好，想和您聊聊这个岗位',
			outreach_time_windows_json = '[{"start":"10:00","end":"12:00"}]',
			updated_at = 2000
		WHERE id = 1
	`); err != nil {
		t.Fatalf("save automation settings fixture: %v", err)
	}

	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("ensure safe defaults after save: %v", err)
	}
	view, err := settings.Get(t.Context())
	if err != nil {
		t.Fatalf("get saved settings: %v", err)
	}
	if !view.AutomaticAssessmentEnabled || view.AssessmentProcessingLimit != 12 {
		t.Errorf("saved assessment settings = %#v, want enabled with limit 12", view)
	}
	if !view.AutomaticOutreachEnabled || view.OutreachGreeting == nil {
		t.Errorf("saved outreach settings = %#v, want enabled with greeting", view)
	}
	if view.OutreachTimeDescription != "10:00-12:00（Asia/Shanghai）" {
		t.Errorf("outreach time description = %q, want configured periods", view.OutreachTimeDescription)
	}
}

func TestConfigureAssessmentPersistsTheSwitchAndAnyPositiveProcessingLimit(t *testing.T) {
	t.Parallel()

	settings, _ := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}

	if err := settings.ConfigureAssessment(t.Context(), true, 37); err != nil {
		t.Fatalf("configure assessment: %v", err)
	}
	view, err := settings.Get(t.Context())
	if err != nil {
		t.Fatalf("get configured assessment settings: %v", err)
	}
	if !view.AutomaticAssessmentEnabled || view.AssessmentProcessingLimit != 37 {
		t.Errorf("configured assessment settings = %#v, want enabled with limit 37", view)
	}

	if err := settings.ConfigureAssessment(t.Context(), false, 1); err != nil {
		t.Fatalf("lower assessment processing limit: %v", err)
	}
	view, err = settings.Get(t.Context())
	if err != nil {
		t.Fatalf("get lowered assessment settings: %v", err)
	}
	if view.AutomaticAssessmentEnabled || view.AssessmentProcessingLimit != 1 {
		t.Errorf("lowered assessment settings = %#v, want disabled with limit 1", view)
	}
}

func TestConfigureAssessmentRejectsANonPositiveLimitWithoutChangingSavedSettings(t *testing.T) {
	t.Parallel()

	settings, _ := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}

	err := settings.ConfigureAssessment(t.Context(), true, 0)
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("configure invalid assessment limit error = %v, want settings rejection", err)
	}
	if rejection.Code != "assessment_processing_limit_invalid" {
		t.Errorf("configure invalid assessment limit code = %q", rejection.Code)
	}
	view, getErr := settings.Get(t.Context())
	if getErr != nil {
		t.Fatalf("get settings after invalid configure: %v", getErr)
	}
	assertSafeDefaults(t, view)
}

func assertSafeDefaults(t *testing.T, view View) {
	t.Helper()
	if view.AutomaticAssessmentEnabled {
		t.Error("automatic assessment is enabled, want disabled")
	}
	if view.AssessmentProcessingLimit != 5 {
		t.Errorf("assessment processing limit = %d, want 5", view.AssessmentProcessingLimit)
	}
	if view.AutomaticOutreachEnabled {
		t.Error("automatic outreach is enabled, want disabled")
	}
	if view.OutreachGreeting != nil {
		t.Errorf("outreach greeting = %q, want unconfigured", *view.OutreachGreeting)
	}
	if len(view.OutreachTimeWindows) != 0 {
		t.Errorf("outreach time windows = %#v, want no restrictions", view.OutreachTimeWindows)
	}
	if view.OutreachTimeDescription != "全天可打招呼" {
		t.Errorf("outreach time description = %q, want 全天可打招呼", view.OutreachTimeDescription)
	}
}

func TestQueueRealOutreachChecksSettingsBeforeDelegatingToTheJobPool(t *testing.T) {
	t.Parallel()

	settings, db := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}

	_, err := settings.QueueRealOutreach(t.Context(), nil, RealOutreachConfirmation{})
	var rejection *Rejection
	ok := errors.As(err, &rejection)
	if !ok {
		t.Fatalf("queue without greeting error = %v, want settings rejection", err)
	}
	if rejection.Code != "outreach_greeting_required" {
		t.Errorf("queue without greeting code = %q, want outreach_greeting_required", rejection.Code)
	}

	if _, err := db.ExecContext(t.Context(), `
		UPDATE automation_settings
		SET outreach_greeting_text = '您好，想和您聊聊这个岗位'
		WHERE id = 1
	`); err != nil {
		t.Fatalf("configure greeting fixture: %v", err)
	}

	_, err = settings.QueueRealOutreach(t.Context(), nil, RealOutreachConfirmation{})
	var poolRejection *jobpool.Rejection
	ok = errors.As(err, &poolRejection)
	if !ok {
		t.Fatalf("queue with greeting error = %v, want job pool rejection", err)
	}
	if poolRejection.Code != "outreach_unavailable" {
		t.Errorf("queue with greeting code = %q, want outreach_unavailable", poolRejection.Code)
	}
}

func TestQueueRealOutreachReturnsTheJobPoolBatchResult(t *testing.T) {
	t.Parallel()

	settings, db := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		UPDATE automation_settings
		SET outreach_greeting_text = '您好，想和您聊聊这个岗位'
		WHERE id = 1
	`); err != nil {
		t.Fatalf("configure greeting fixture: %v", err)
	}
	job, err := settings.pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: "boss-job-1", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatalf("observe eligible outreach job: %v", err)
	}
	if err := settings.pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: job.ID, ExpectedJDHash: job.JDHash,
		Verdict: jobpool.HumanVerdictSuitable,
	}}); err != nil {
		t.Fatalf("review eligible outreach job: %v", err)
	}

	result, err := settings.QueueRealOutreach(
		t.Context(), []int64{job.ID, 999}, RealOutreachConfirmation{
			JobCount:        1,
			GreetingText:    "您好，想和您聊聊这个岗位",
			TimeDescription: "全天可打招呼",
			Confirmed:       true,
		},
	)
	if err != nil {
		t.Fatalf("queue real outreach: %v", err)
	}
	if result.Succeeded != 1 || len(result.Skipped) != 1 || result.Skipped[0].JobID != 999 {
		t.Errorf("queue real outreach result = %#v, want one success and missing job 999", result)
	}
}

func TestConfigureOutreachSortsWindowsAndUsesAsiaShanghai(t *testing.T) {
	t.Parallel()

	settings, _ := openTestSettings(t)
	mustEnsureSettingsDefaults(t, settings)
	if err := settings.ConfigureOutreach(t.Context(), true, "  您好，想聊聊  ", []OutreachTimeWindow{
		{Start: "14:00", End: "18:00"},
		{Start: "09:00", End: "12:00"},
	}); err != nil {
		t.Fatalf("configure outreach: %v", err)
	}

	view := mustGetSettingsView(t, settings)
	assertConfiguredOutreachView(t, view)
}

func mustEnsureSettingsDefaults(t *testing.T, settings *Settings) {
	t.Helper()
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}
}

func assertConfiguredOutreachView(t *testing.T, view View) {
	t.Helper()
	if !view.AutomaticOutreachEnabled || view.OutreachGreeting == nil || *view.OutreachGreeting != "您好，想聊聊" {
		t.Errorf("outreach settings = %#v, want enabled and normalized greeting", view)
	}
	if want := []OutreachTimeWindow{{Start: "09:00", End: "12:00"}, {Start: "14:00", End: "18:00"}}; !reflect.DeepEqual(view.OutreachTimeWindows, want) {
		t.Errorf("outreach windows = %#v, want %#v", view.OutreachTimeWindows, want)
	}
	if view.OutreachTimeDescription != "09:00-12:00、14:00-18:00（Asia/Shanghai）" {
		t.Errorf("outreach time description = %q", view.OutreachTimeDescription)
	}
	if !view.AllowsOutreachAt(time.Date(2026, 9, 1, 2, 30, 0, 0, time.UTC)) {
		t.Error("02:30 UTC (10:30 Asia/Shanghai) is outside outreach window")
	}
	if view.AllowsOutreachAt(time.Date(2026, 9, 1, 4, 30, 0, 0, time.UTC)) {
		t.Error("04:30 UTC (12:30 Asia/Shanghai) is inside outreach window")
	}
}

func TestConfigureOutreachRejectsOverlappingWindowsAndMissingGreetingWhenEnabled(t *testing.T) {
	t.Parallel()

	settings, _ := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}

	err := settings.ConfigureOutreach(t.Context(), true, "", []OutreachTimeWindow{{Start: "09:00", End: "12:00"}})
	assertSettingsRejectionCode(t, err, "outreach_greeting_required")
	err = settings.ConfigureOutreach(t.Context(), false, "您好", []OutreachTimeWindow{
		{Start: "09:00", End: "12:00"}, {Start: "11:59", End: "13:00"},
	})
	assertSettingsRejectionCode(t, err, "outreach_time_windows_overlap")
	assertSafeDefaults(t, mustGetSettingsView(t, settings))
}

func TestQueueRealOutreachRequiresCurrentExplicitConfirmation(t *testing.T) {
	t.Parallel()

	settings, db := openTestSettings(t)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure safe defaults: %v", err)
	}
	if err := settings.ConfigureOutreach(t.Context(), false, "您好，想和您聊聊这个岗位", nil); err != nil {
		t.Fatalf("configure outreach greeting: %v", err)
	}
	job, err := settings.pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: "boss-job-confirmation", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-confirmation.html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatalf("observe eligible outreach job: %v", err)
	}
	if err := settings.pool.Review(t.Context(), []jobpool.ReviewDecision{{JobID: job.ID, ExpectedJDHash: job.JDHash, Verdict: jobpool.HumanVerdictSuitable}}); err != nil {
		t.Fatalf("review eligible outreach job: %v", err)
	}

	_, err = settings.QueueRealOutreach(t.Context(), []int64{job.ID}, RealOutreachConfirmation{})
	assertSettingsRejectionCode(t, err, "outreach_confirmation_required")
	_, err = settings.QueueRealOutreach(t.Context(), []int64{job.ID}, RealOutreachConfirmation{
		JobCount: 1, GreetingText: "旧招呼语", TimeDescription: "全天可打招呼", Confirmed: true,
	})
	assertSettingsRejectionCode(t, err, "outreach_confirmation_stale")
	result, err := settings.QueueRealOutreach(t.Context(), []int64{job.ID}, RealOutreachConfirmation{
		JobCount: 1, GreetingText: "您好，想和您聊聊这个岗位", TimeDescription: "全天可打招呼", Confirmed: true,
	})
	if err != nil || result.Succeeded != 1 {
		t.Fatalf("queue confirmed outreach = %#v, err=%v; want one success", result, err)
	}
	var status string
	if err := db.QueryRowContext(t.Context(), `SELECT outreach_status FROM platform_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
		t.Fatalf("read queued outreach status: %v", err)
	}
	if status != string(jobpool.OutreachStatusPending) {
		t.Errorf("queued outreach status = %q, want pending", status)
	}
}

func assertSettingsRejectionCode(t *testing.T, err error, want string) {
	t.Helper()
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("error = %v, want settings rejection %q", err, want)
	}
	if rejection.Code != want {
		t.Fatalf("rejection code = %q, want %q", rejection.Code, want)
	}
}

func mustGetSettingsView(t *testing.T, settings *Settings) View {
	t.Helper()
	view, err := settings.Get(t.Context())
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	return view
}
