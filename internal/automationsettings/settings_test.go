package automationsettings

import (
	"database/sql"
	"errors"
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
	if view.OutreachTimeDescription != "按已配置时间段打招呼" {
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
		t.Context(), []int64{job.ID, 999}, RealOutreachConfirmation{},
	)
	if err != nil {
		t.Fatalf("queue real outreach: %v", err)
	}
	if result.Succeeded != 1 || len(result.Skipped) != 1 || result.Skipped[0].JobID != 999 {
		t.Errorf("queue real outreach result = %#v, want one success and missing job 999", result)
	}
}
