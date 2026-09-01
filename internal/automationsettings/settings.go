package automationsettings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
)

// Settings owns the local instance's automation settings.
type Settings struct {
	queries *sqlitedb.Queries
	pool    *jobpool.Pool
	mu      sync.RWMutex
}

type View struct {
	AutomaticAssessmentEnabled bool                 `json:"automaticAssessmentEnabled"`
	AssessmentProcessingLimit  int                  `json:"assessmentProcessingLimit"`
	AutomaticOutreachEnabled   bool                 `json:"automaticOutreachEnabled"`
	OutreachGreeting           *string              `json:"outreachGreeting"`
	OutreachTimeWindows        []OutreachTimeWindow `json:"outreachTimeWindows"`
	OutreachTimeDescription    string               `json:"outreachTimeDescription"`
}

type OutreachTimeWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type OutreachChangeImpact struct {
	AutomaticOutreachEnabled bool                 `json:"automaticOutreachEnabled"`
	EligibleJobCount         int                  `json:"eligibleJobCount"`
	GreetingText             string               `json:"greetingText"`
	TimeDescription          string               `json:"timeDescription"`
	OutreachTimeWindows      []OutreachTimeWindow `json:"outreachTimeWindows"`
}

type RealOutreachConfirmation struct {
	JobCount        int    `json:"jobCount"`
	GreetingText    string `json:"greetingText"`
	TimeDescription string `json:"timeDescription"`
	Confirmed       bool   `json:"confirmed"`
}

// OutreachSettingsConfirmation proves that the user saw the current impact
// of enabling automatic outreach before the setting is persisted.
type OutreachSettingsConfirmation struct {
	EligibleJobCount int    `json:"eligibleJobCount"`
	GreetingText     string `json:"greetingText"`
	TimeDescription  string `json:"timeDescription"`
	Confirmed        bool   `json:"confirmed"`
}

type ActionAvailability struct {
	Allowed bool   `json:"allowed"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type Rejection struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func (r *Rejection) Error() string {
	return r.Reason
}

func (r *Rejection) RejectionCode() string {
	return r.Code
}

func (r *Rejection) RejectionReason() string {
	return r.Reason
}

func New(db *sql.DB, pool *jobpool.Pool) *Settings {
	return &Settings{queries: sqlitedb.New(db), pool: pool}
}

// EnsureSafeDefaults creates the singleton row without changing saved settings.
func (s *Settings) EnsureSafeDefaults(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.queries.EnsureSafeDefaults(ctx, now.UnixMilli()); err != nil {
		return fmt.Errorf("create safe automation settings: %w", err)
	}
	return nil
}

func (s *Settings) Get(ctx context.Context) (View, error) {
	return s.get(ctx)
}

// AllowsOutreachAt evaluates the configured daily half-open windows in the
// user's local China Standard Time. An empty list means the whole day.
func (v View) AllowsOutreachAt(now time.Time) bool {
	if len(v.OutreachTimeWindows) == 0 {
		return true
	}
	local := now.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	minutes := local.Hour()*60 + local.Minute()
	for _, window := range v.OutreachTimeWindows {
		start, _ := parseOutreachClock(window.Start)
		end, _ := parseOutreachClock(window.End)
		if minutes >= start && minutes < end {
			return true
		}
	}
	return false
}

// ConfigureAssessment controls admission of new assessment work and the
// global number of platform jobs that may be processing at once.
func (s *Settings) ConfigureAssessment(ctx context.Context, enabled bool, processingLimit int) error {
	if processingLimit <= 0 {
		return &Rejection{
			Code:   "assessment_processing_limit_invalid",
			Reason: "AI 同时鉴定数必须是正整数",
		}
	}
	updated, err := s.queries.ConfigureAssessment(ctx, sqlitedb.ConfigureAssessmentParams{
		AutomaticAssessmentEnabled: boolInt64(enabled),
		AssessmentProcessingLimit:  int64(processingLimit),
		UpdatedAt:                  time.Now().UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("configure assessment automation: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("configure assessment automation: safe settings are not initialized")
	}
	return nil
}

// ConfigureOutreach validates and persists the instance-level real outreach
// authorization settings. A greeting is retained while automatic outreach is
// disabled, so a later explicit enable does not need to recreate it.
func (s *Settings) ConfigureOutreach(
	ctx context.Context,
	enabled bool,
	greetingText string,
	windows []OutreachTimeWindow,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled {
		current, err := s.get(ctx)
		if err != nil {
			return err
		}
		if !current.AutomaticOutreachEnabled {
			return &Rejection{
				Code:   "outreach_confirmation_required",
				Reason: "首次开启自动打招呼必须先预览并确认影响",
			}
		}
	}
	return s.configureOutreach(ctx, enabled, greetingText, windows)
}

// ConfigureOutreachWithConfirmation is the user-facing configuration seam.
// Enabling automatic outreach requires a fresh confirmation of the current
// eligible-job count, complete greeting and time rule. Disabling or editing
// an already enabled setting does not revoke work that is already queued.
func (s *Settings) ConfigureOutreachWithConfirmation(
	ctx context.Context,
	enabled bool,
	greetingText string,
	windows []OutreachTimeWindow,
	confirmation OutreachSettingsConfirmation,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled {
		current, err := s.get(ctx)
		if err != nil {
			return err
		}
		if current.AutomaticOutreachEnabled {
			return s.configureOutreach(ctx, enabled, greetingText, windows)
		}
		impact, err := s.PreviewOutreachConfiguration(ctx, enabled, greetingText, windows)
		if err != nil {
			return err
		}
		if !confirmation.Confirmed {
			return &Rejection{
				Code:   "outreach_confirmation_required",
				Reason: "开启自动打招呼前必须确认当前可入队岗位、完整招呼语和时间规则",
			}
		}
		if confirmation.EligibleJobCount != impact.EligibleJobCount ||
			strings.Join(strings.Fields(confirmation.GreetingText), " ") != impact.GreetingText ||
			strings.TrimSpace(confirmation.TimeDescription) != impact.TimeDescription {
			return &Rejection{
				Code:   "outreach_confirmation_stale",
				Reason: "岗位资格、完整招呼语或时间规则已变化，请刷新后重新确认",
			}
		}
	}
	return s.configureOutreach(ctx, enabled, greetingText, windows)
}

// AdmitAutomaticOutreach reads the current setting and, while holding the
// same lock used by configuration, admits only newly eligible automatic work.
// A non-nil view is returned with an admission error so the caller can still
// process already queued work using the current time rule.
func (s *Settings) AdmitAutomaticOutreach(ctx context.Context, limit int) (*View, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	view, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	if !view.AutomaticOutreachEnabled {
		return &view, nil
	}
	if view.OutreachGreeting == nil {
		return &view, errors.New("automatic outreach is enabled without a fixed greeting")
	}
	if _, err := s.pool.AdmitOutreach(ctx, jobpool.OutreachAuthorization{
		GreetingText:    *view.OutreachGreeting,
		TimeDescription: view.OutreachTimeDescription,
	}, limit); err != nil {
		return &view, fmt.Errorf("admit automatic outreach: %w", err)
	}
	return &view, nil
}

func (s *Settings) configureOutreach(
	ctx context.Context,
	enabled bool,
	greetingText string,
	windows []OutreachTimeWindow,
) error {
	greeting := strings.Join(strings.Fields(greetingText), " ")
	if enabled && greeting == "" {
		return &Rejection{Code: "outreach_greeting_required", Reason: "开启自动打招呼前必须配置固定招呼语"}
	}
	normalizedWindows, err := normalizeOutreachTimeWindows(windows)
	if err != nil {
		return err
	}
	windowsJSON, err := json.Marshal(normalizedWindows)
	if err != nil {
		return fmt.Errorf("encode outreach time windows: %w", err)
	}
	var greetingValue sql.NullString
	if greeting != "" {
		greetingValue = sql.NullString{String: greeting, Valid: true}
	}
	updated, err := s.queries.ConfigureOutreach(ctx, sqlitedb.ConfigureOutreachParams{
		AutomaticOutreachEnabled: boolInt64(enabled), OutreachGreetingText: greetingValue,
		OutreachTimeWindowsJson: string(windowsJSON), UpdatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("configure outreach automation: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("configure outreach automation: safe settings are not initialized")
	}
	return nil
}

func normalizeOutreachTimeWindows(windows []OutreachTimeWindow) ([]OutreachTimeWindow, error) {
	normalized := make([]OutreachTimeWindow, 0, len(windows))
	for index, window := range windows {
		start, err := parseOutreachClock(window.Start)
		if err != nil {
			return nil, &Rejection{Code: "outreach_time_window_invalid", Reason: fmt.Sprintf("第 %d 个打招呼时间窗无效：%v", index+1, err)}
		}
		end, err := parseOutreachClock(window.End)
		if err != nil {
			return nil, &Rejection{Code: "outreach_time_window_invalid", Reason: fmt.Sprintf("第 %d 个打招呼时间窗无效：%v", index+1, err)}
		}
		if start >= end {
			return nil, &Rejection{Code: "outreach_time_window_invalid", Reason: fmt.Sprintf("第 %d 个打招呼时间窗必须从早到晚", index+1)}
		}
		normalized = append(normalized, OutreachTimeWindow{
			Start: formatOutreachClock(start), End: formatOutreachClock(end),
		})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Start < normalized[j].Start })
	for index := 1; index < len(normalized); index++ {
		_, previousEnd := mustParseOutreachWindow(normalized[index-1])
		currentStart, _ := mustParseOutreachWindow(normalized[index])
		if currentStart < previousEnd {
			return nil, &Rejection{Code: "outreach_time_windows_overlap", Reason: "打招呼时间窗不能互相重叠"}
		}
	}
	return normalized, nil
}

func parseOutreachClock(value string) (int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, errors.New("时间必须使用 HH:MM 格式")
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func formatOutreachClock(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

func mustParseOutreachWindow(window OutreachTimeWindow) (int, int) {
	start, _ := parseOutreachClock(window.Start)
	end, _ := parseOutreachClock(window.End)
	return start, end
}

func outreachTimeDescription(windows []OutreachTimeWindow) string {
	if len(windows) == 0 {
		return "全天可打招呼"
	}
	parts := make([]string, 0, len(windows))
	for _, window := range windows {
		parts = append(parts, window.Start+"-"+window.End)
	}
	return strings.Join(parts, "、") + "（Asia/Shanghai）"
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

// GetDiscoveryHints keeps missing downstream settings from blocking job
// discovery while still reporting other storage failures.
func (s *Settings) GetDiscoveryHints(ctx context.Context) (View, error) {
	view, err := s.Get(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return View{}, nil
	}
	return view, err
}

func (s *Settings) QueueRealOutreach(
	ctx context.Context,
	jobIDs []int64,
	confirmation RealOutreachConfirmation,
) (jobpool.BatchActionResult, error) {
	view, err := s.Get(ctx)
	if err != nil {
		return jobpool.BatchActionResult{}, err
	}
	if view.OutreachGreeting == nil {
		return jobpool.BatchActionResult{}, &Rejection{
			Code:   "outreach_greeting_required",
			Reason: "请先配置固定招呼语，再真实打招呼",
		}
	}
	if len(jobIDs) > 0 {
		if !confirmation.Confirmed {
			return jobpool.BatchActionResult{}, &Rejection{
				Code: "outreach_confirmation_required", Reason: "请确认本批岗位、完整招呼语和当前时间规则",
			}
		}
		eligibleCount, err := s.pool.CountEligibleOutreach(ctx, jobIDs)
		if err != nil {
			return jobpool.BatchActionResult{}, err
		}
		if confirmation.JobCount != eligibleCount ||
			strings.Join(strings.Fields(confirmation.GreetingText), " ") != *view.OutreachGreeting ||
			strings.TrimSpace(confirmation.TimeDescription) != view.OutreachTimeDescription {
			return jobpool.BatchActionResult{}, &Rejection{
				Code: "outreach_confirmation_stale", Reason: "岗位资格、完整招呼语或时间规则已变化，请刷新后重新确认",
			}
		}
	}
	return s.pool.QueueAuthorizedOutreach(ctx, jobIDs, jobpool.OutreachAuthorization{
		GreetingText:    *view.OutreachGreeting,
		TimeDescription: view.OutreachTimeDescription,
	})
}

func (s *Settings) PreviewOutreachChange(ctx context.Context, automaticEnabled bool) (OutreachChangeImpact, error) {
	view, err := s.get(ctx)
	if err != nil {
		return OutreachChangeImpact{}, err
	}
	greeting := ""
	if view.OutreachGreeting != nil {
		greeting = *view.OutreachGreeting
	}
	return s.PreviewOutreachConfiguration(ctx, automaticEnabled, greeting, view.OutreachTimeWindows)
}

func (s *Settings) get(ctx context.Context) (View, error) {
	row, err := s.queries.GetAutomationSettings(ctx)
	if err != nil {
		return View{}, fmt.Errorf("query automation settings: %w", err)
	}
	var windows []OutreachTimeWindow
	if err := json.Unmarshal([]byte(row.OutreachTimeWindowsJson), &windows); err != nil {
		return View{}, fmt.Errorf("decode outreach time windows: %w", err)
	}
	windows, err = normalizeOutreachTimeWindows(windows)
	if err != nil {
		return View{}, fmt.Errorf("validate saved outreach time windows: %w", err)
	}
	var greeting *string
	if row.OutreachGreetingText.Valid {
		greeting = &row.OutreachGreetingText.String
	}
	timeDescription := outreachTimeDescription(windows)
	return View{
		AutomaticAssessmentEnabled: row.AutomaticAssessmentEnabled == 1,
		AssessmentProcessingLimit:  int(row.AssessmentProcessingLimit),
		AutomaticOutreachEnabled:   row.AutomaticOutreachEnabled == 1,
		OutreachGreeting:           greeting,
		OutreachTimeWindows:        windows,
		OutreachTimeDescription:    timeDescription,
	}, nil
}

// PreviewOutreachConfiguration applies the same normalization and eligibility
// rules as configuration, but never changes settings or platform jobs.
func (s *Settings) PreviewOutreachConfiguration(
	ctx context.Context,
	automaticEnabled bool,
	greetingText string,
	windows []OutreachTimeWindow,
) (OutreachChangeImpact, error) {
	greeting := strings.Join(strings.Fields(greetingText), " ")
	if automaticEnabled && greeting == "" {
		return OutreachChangeImpact{}, &Rejection{
			Code: "outreach_greeting_required", Reason: "开启自动打招呼前必须配置固定招呼语",
		}
	}
	normalizedWindows, err := normalizeOutreachTimeWindows(windows)
	if err != nil {
		return OutreachChangeImpact{}, err
	}
	count, err := s.pool.CountEligibleOutreach(ctx, nil)
	if err != nil {
		return OutreachChangeImpact{}, err
	}
	impact := OutreachChangeImpact{
		AutomaticOutreachEnabled: automaticEnabled,
		EligibleJobCount:         count,
		GreetingText:             greeting,
		TimeDescription:          outreachTimeDescription(normalizedWindows),
		OutreachTimeWindows:      append([]OutreachTimeWindow(nil), normalizedWindows...),
	}
	return impact, nil
}

func (s *Settings) QueueRealOutreachAvailability(ctx context.Context) (ActionAvailability, error) {
	view, err := s.Get(ctx)
	if err != nil {
		return ActionAvailability{}, err
	}
	if view.OutreachGreeting == nil {
		return ActionAvailability{
			Code:   "outreach_greeting_required",
			Reason: "请先配置固定招呼语，再真实打招呼",
		}, nil
	}
	availability := s.pool.OutreachAvailability(ctx)
	return ActionAvailability{
		Allowed: availability.Allowed,
		Code:    availability.Code,
		Reason:  availability.Reason,
	}, nil
}
