package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/advice"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type OutreachMode string

const (
	OutreachModeSimulation OutreachMode = "simulation"
	OutreachModeReal       OutreachMode = "real"
)

type Config struct {
	DatabasePath string
	Now          func() time.Time
}

type Application struct {
	db *sql.DB
}

type StartupState struct {
	CurrentResume *OnlineResumeVersion `json:"currentResume"`
	ActivePolicy  AssessmentPolicy     `json:"activePolicy"`
	Automation    AutomationSettings   `json:"automation"`
	Actions       FirstUseActions      `json:"actions"`
}

type OnlineResumeVersion struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
}

type AssessmentPolicy struct {
	Version int      `json:"version"`
	Name    string   `json:"name"`
	Rules   []string `json:"rules"`
}

type AutomationSettings struct {
	AutomaticAssessmentEnabled bool                 `json:"automaticAssessmentEnabled"`
	AssessmentProcessingLimit  int                  `json:"assessmentProcessingLimit"`
	AutomaticOutreachEnabled   bool                 `json:"automaticOutreachEnabled"`
	AutomaticOutreachMode      OutreachMode         `json:"automaticOutreachMode"`
	AutomaticOutreachModeText  string               `json:"automaticOutreachModeText"`
	OutreachGreeting           *string              `json:"outreachGreeting"`
	OutreachTimeWindows        []OutreachTimeWindow `json:"outreachTimeWindows"`
	OutreachTimeDescription    string               `json:"outreachTimeDescription"`
}

type OutreachTimeWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type FirstUseActions struct {
	StartDiscovery          ActionAvailability `json:"startDiscovery"`
	QueueSimulationOutreach ActionAvailability `json:"queueSimulationOutreach"`
	QueueRealOutreach       ActionAvailability `json:"queueRealOutreach"`
}

type ActionAvailability struct {
	Allowed bool   `json:"allowed"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type RealOutreachConfirmation struct {
	JobCount        int    `json:"jobCount"`
	GreetingText    string `json:"greetingText"`
	TimeDescription string `json:"timeDescription"`
	Confirmed       bool   `json:"confirmed"`
}

type Rejection struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func (r *Rejection) Error() string {
	return r.Reason
}

func AsRejection(err error) (*Rejection, bool) {
	var rejection *Rejection
	ok := errors.As(err, &rejection)
	return rejection, ok
}

func Open(ctx context.Context, config Config) (*Application, error) {
	if config.Now == nil {
		config.Now = time.Now
	}
	nowMillis := config.Now().UnixMilli()
	db, err := storage.Open(ctx, config.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := advice.EnsureDefaultPolicy(ctx, db, nowMillis); err != nil {
		db.Close()
		return nil, err
	}
	if err := automationsettings.EnsureSafeDefaults(ctx, db, nowMillis); err != nil {
		db.Close()
		return nil, err
	}
	return &Application{db: db}, nil
}

func (a *Application) Close() error {
	return a.db.Close()
}

func (a *Application) StartupState(ctx context.Context) (StartupState, error) {
	resume, err := a.currentResume(ctx)
	if err != nil {
		return StartupState{}, err
	}
	policy, err := a.activePolicy(ctx)
	if err != nil {
		return StartupState{}, err
	}
	automation, err := a.automationSettings(ctx)
	if err != nil {
		return StartupState{}, err
	}
	state := StartupState{
		CurrentResume: resume,
		ActivePolicy:  policy,
		Automation:    automation,
	}
	state.Actions = firstUseActions(state)
	return state, nil
}

func (a *Application) StartDiscovery(ctx context.Context) error {
	resume, err := a.currentResume(ctx)
	if err != nil {
		return err
	}
	return rejectUnavailable(discoveryAvailability(resume))
}

func (a *Application) QueueSimulationOutreach(ctx context.Context, _ []int64) error {
	automation, err := a.automationSettings(ctx)
	if err != nil {
		return err
	}
	simulation, _ := outreachAvailability(automation)
	return rejectUnavailable(simulation)
}

func (a *Application) QueueRealOutreach(ctx context.Context, _ []int64, _ RealOutreachConfirmation) error {
	automation, err := a.automationSettings(ctx)
	if err != nil {
		return err
	}
	_, real := outreachAvailability(automation)
	return rejectUnavailable(real)
}

func rejectUnavailable(action ActionAvailability) error {
	if action.Allowed {
		return nil
	}
	return &Rejection{Code: action.Code, Reason: action.Reason}
}

func firstUseActions(state StartupState) FirstUseActions {
	simulation, real := outreachAvailability(state.Automation)
	return FirstUseActions{
		StartDiscovery:          discoveryAvailability(state.CurrentResume),
		QueueSimulationOutreach: simulation,
		QueueRealOutreach:       real,
	}
}

func discoveryAvailability(resume *OnlineResumeVersion) ActionAvailability {
	if resume == nil {
		return unavailable(
			"online_resume_required",
			"请先刷新在线简历，再开始岗位发现",
		)
	}
	return unavailable(
		"discovery_unavailable",
		"当前版本尚未开放岗位发现",
	)
}

func outreachAvailability(automation AutomationSettings) (ActionAvailability, ActionAvailability) {
	actions := FirstUseActions{
		QueueSimulationOutreach: unavailable(
			"outreach_unavailable",
			"当前没有可加入模拟队列的岗位",
		),
		QueueRealOutreach: unavailable(
			"outreach_unavailable",
			"当前没有可加入真实发送队列的岗位",
		),
	}
	if automation.OutreachGreeting == nil {
		actions.QueueSimulationOutreach = unavailable(
			"outreach_greeting_required",
			"请先配置固定招呼语，再加入模拟队列",
		)
		actions.QueueRealOutreach = unavailable(
			"outreach_greeting_required",
			"请先配置固定招呼语，再加入真实发送队列",
		)
	}
	return actions.QueueSimulationOutreach, actions.QueueRealOutreach
}

func unavailable(code, reason string) ActionAvailability {
	return ActionAvailability{Code: code, Reason: reason}
}

func (a *Application) currentResume(ctx context.Context) (*OnlineResumeVersion, error) {
	var version int
	var createdAtMillis int64
	err := a.db.QueryRowContext(ctx, `
		SELECT version_no, created_at
		FROM online_resume_versions
		WHERE is_current = 1
	`).Scan(&version, &createdAtMillis)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query current online resume: %w", err)
	}
	return &OnlineResumeVersion{
		Version:   version,
		CreatedAt: time.UnixMilli(createdAtMillis),
	}, nil
}

func (a *Application) activePolicy(ctx context.Context) (AssessmentPolicy, error) {
	var version int
	var rulesJSON string
	var changeNote sql.NullString
	if err := a.db.QueryRowContext(ctx, `
		SELECT version_no, rules_json, change_note
		FROM assessment_policy_versions
		WHERE is_active = 1
	`).Scan(&version, &rulesJSON, &changeNote); err != nil {
		return AssessmentPolicy{}, fmt.Errorf("query active assessment policy: %w", err)
	}
	var document struct {
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal([]byte(rulesJSON), &document); err != nil {
		return AssessmentPolicy{}, fmt.Errorf("decode active assessment policy: %w", err)
	}
	name := fmt.Sprintf("策略 v%d", version)
	if version == 1 && changeNote.String == "系统默认策略" {
		name = "默认策略 v1"
	}
	return AssessmentPolicy{Version: version, Name: name, Rules: document.Rules}, nil
}

func (a *Application) automationSettings(ctx context.Context) (AutomationSettings, error) {
	var automaticAssessment int
	var processingLimit int
	var automaticOutreach int
	var mode string
	var greeting sql.NullString
	var windowsJSON string
	if err := a.db.QueryRowContext(ctx, `
		SELECT
			automatic_assessment_enabled,
			assessment_processing_limit,
			automatic_outreach_enabled,
			automatic_outreach_mode,
			outreach_greeting_text,
			outreach_time_windows_json
		FROM automation_settings
		WHERE id = 1
	`).Scan(
		&automaticAssessment,
		&processingLimit,
		&automaticOutreach,
		&mode,
		&greeting,
		&windowsJSON,
	); err != nil {
		return AutomationSettings{}, fmt.Errorf("query automation settings: %w", err)
	}

	var windows []OutreachTimeWindow
	if err := json.Unmarshal([]byte(windowsJSON), &windows); err != nil {
		return AutomationSettings{}, fmt.Errorf("decode outreach time windows: %w", err)
	}
	var greetingValue *string
	if greeting.Valid {
		greetingValue = &greeting.String
	}
	timeDescription := "全天可发送"
	if len(windows) > 0 {
		timeDescription = "按已配置时间段发送"
	}

	modeText := "Simulation"
	if OutreachMode(mode) == OutreachModeReal {
		modeText = "Real"
	}

	return AutomationSettings{
		AutomaticAssessmentEnabled: automaticAssessment == 1,
		AssessmentProcessingLimit:  processingLimit,
		AutomaticOutreachEnabled:   automaticOutreach == 1,
		AutomaticOutreachMode:      OutreachMode(mode),
		AutomaticOutreachModeText:  modeText,
		OutreachGreeting:           greetingValue,
		OutreachTimeWindows:        windows,
		OutreachTimeDescription:    timeDescription,
	}, nil
}
