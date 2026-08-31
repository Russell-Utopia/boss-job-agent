package automationsettings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
)

// Settings owns the local instance's automation settings.
type Settings struct {
	queries *sqlitedb.Queries
	pool    *jobpool.Pool
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

type RealOutreachConfirmation struct {
	JobCount        int    `json:"jobCount"`
	GreetingText    string `json:"greetingText"`
	TimeDescription string `json:"timeDescription"`
	Confirmed       bool   `json:"confirmed"`
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
	if err := s.queries.EnsureSafeDefaults(ctx, now.UnixMilli()); err != nil {
		return fmt.Errorf("create safe automation settings: %w", err)
	}
	return nil
}

func (s *Settings) Get(ctx context.Context) (View, error) {
	row, err := s.queries.GetAutomationSettings(ctx)
	if err != nil {
		return View{}, fmt.Errorf("query automation settings: %w", err)
	}
	var windows []OutreachTimeWindow
	if err := json.Unmarshal([]byte(row.OutreachTimeWindowsJson), &windows); err != nil {
		return View{}, fmt.Errorf("decode outreach time windows: %w", err)
	}
	var greeting *string
	if row.OutreachGreetingText.Valid {
		greeting = &row.OutreachGreetingText.String
	}
	timeDescription := "全天可打招呼"
	if len(windows) > 0 {
		timeDescription = "按已配置时间段打招呼"
	}
	return View{
		AutomaticAssessmentEnabled: row.AutomaticAssessmentEnabled == 1,
		AssessmentProcessingLimit:  int(row.AssessmentProcessingLimit),
		AutomaticOutreachEnabled:   row.AutomaticOutreachEnabled == 1,
		OutreachGreeting:           greeting,
		OutreachTimeWindows:        windows,
		OutreachTimeDescription:    timeDescription,
	}, nil
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
	_ RealOutreachConfirmation,
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
	return s.pool.QueueAuthorizedOutreach(ctx, jobIDs, jobpool.OutreachAuthorization{
		GreetingText:    *view.OutreachGreeting,
		TimeDescription: view.OutreachTimeDescription,
	})
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
