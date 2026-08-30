package application

import (
	"context"
	"testing"
	"time"
)

func TestFirstStartupRestoresSafeDefaults(t *testing.T) {
	t.Parallel()

	app, err := Open(context.Background(), Config{
		DatabasePath: ":memory:",
		Now: func() time.Time {
			return time.Date(2026, time.August, 28, 9, 30, 0, 0, time.Local)
		},
	})
	if err != nil {
		t.Fatalf("open application: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("close application: %v", err)
		}
	})

	state, err := app.StartupState(context.Background())
	if err != nil {
		t.Fatalf("query startup state: %v", err)
	}

	if state.CurrentResume != nil {
		t.Fatalf("current resume = %#v, want no online resume version", state.CurrentResume)
	}
	if state.ActivePolicy.Version != 1 {
		t.Errorf("active policy version = %d, want 1", state.ActivePolicy.Version)
	}
	if got := len(state.ActivePolicy.Rules); got != 4 {
		t.Errorf("default policy rule count = %d, want 4", got)
	}
	if state.Automation.AutomaticAssessmentEnabled {
		t.Error("automatic assessment is enabled, want disabled")
	}
	if state.Automation.AssessmentProcessingLimit != 5 {
		t.Errorf("assessment processing limit = %d, want 5", state.Automation.AssessmentProcessingLimit)
	}
	if state.Automation.AutomaticOutreachEnabled {
		t.Error("automatic outreach is enabled, want disabled")
	}
	if state.Automation.OutreachGreeting != nil {
		t.Errorf("outreach greeting = %q, want unconfigured", *state.Automation.OutreachGreeting)
	}
	if len(state.Automation.OutreachTimeWindows) != 0 {
		t.Errorf("outreach time windows = %#v, want no restrictions", state.Automation.OutreachTimeWindows)
	}
	if state.Automation.OutreachTimeDescription != "全天可打招呼" {
		t.Errorf("outreach time description = %q, want 全天可打招呼", state.Automation.OutreachTimeDescription)
	}
}

func TestFirstUseActionsAreRejectedWithUserFacingReasons(t *testing.T) {
	t.Parallel()

	app, err := Open(context.Background(), Config{DatabasePath: ":memory:"})
	if err != nil {
		t.Fatalf("open application: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("close application: %v", err)
		}
	})

	state, err := app.StartupState(context.Background())
	if err != nil {
		t.Fatalf("query startup state: %v", err)
	}
	assertUnavailableAction(t, state.Actions.StartDiscovery, "online_resume_required", "请先刷新在线简历，再开始岗位发现")
	assertUnavailableAction(t, state.Actions.QueueRealOutreach, "outreach_greeting_required", "请先配置固定招呼语，再真实打招呼")

	commands := []struct {
		name string
		run  func(context.Context) error
		code string
	}{
		{name: "start discovery", run: app.StartDiscovery, code: "online_resume_required"},
		{name: "queue real outreach", run: func(ctx context.Context) error {
			return app.QueueRealOutreach(ctx, nil, RealOutreachConfirmation{})
		}, code: "outreach_greeting_required"},
	}

	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			err := command.run(context.Background())
			rejection, ok := AsRejection(err)
			if !ok {
				t.Fatalf("command error = %v, want a business rejection", err)
			}
			if rejection.Code != command.code {
				t.Errorf("rejection code = %q, want %q", rejection.Code, command.code)
			}
			if rejection.Reason == "" {
				t.Error("rejection reason is empty")
			}
		})
	}
}

func TestStartDiscoveryDoesNotDependOnDownstreamPolicyOrAutomationSettings(t *testing.T) {
	t.Parallel()

	app, err := Open(context.Background(), Config{DatabasePath: ":memory:"})
	if err != nil {
		t.Fatalf("open application: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if _, err := app.db.Exec(`DELETE FROM assessment_policy_versions`); err != nil {
		t.Fatalf("remove policy fixture: %v", err)
	}
	if _, err := app.db.Exec(`DELETE FROM automation_settings`); err != nil {
		t.Fatalf("remove automation fixture: %v", err)
	}

	err = app.StartDiscovery(context.Background())
	rejection, ok := AsRejection(err)
	if !ok {
		t.Fatalf("start discovery error = %v, want a business rejection", err)
	}
	if rejection.Code != "online_resume_required" {
		t.Errorf("rejection code = %q, want online_resume_required", rejection.Code)
	}
}

func assertUnavailableAction(t *testing.T, action ActionAvailability, code, reason string) {
	t.Helper()
	if action.Allowed {
		t.Fatal("action is allowed, want disabled")
	}
	if action.Code != code {
		t.Errorf("action code = %q, want %q", action.Code, code)
	}
	if action.Reason != reason {
		t.Errorf("action reason = %q, want %q", action.Reason, reason)
	}
}
