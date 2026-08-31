package discovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

const inlineWorkerLease = 10 * time.Minute

// Service owns job discovery runs.
type Service struct {
	resumeVersions *onlineresume.Versions
	pool           *jobpool.Pool
	discovery      JobDiscovery
	logs           *runlog.Log
	now            func() time.Time
	queries        *sqlitedb.Queries
}

// JobDiscovery is the one-page BOSS search owned by this module.
type JobDiscovery interface {
	FetchPage(context.Context, SearchRange, int) (DiscoveryPage, error)
}

type SearchRange struct {
	Role           string `json:"role"`
	City           string `json:"city"`
	Salary         string `json:"salary"`
	EmploymentType string `json:"employmentType"`
}

type PlatformStatus string

const (
	PlatformStatusOpen   PlatformStatus = "open"
	PlatformStatusClosed PlatformStatus = "closed"
)

type JobObservation struct {
	PlatformJobID        string         `json:"platformJobId"`
	CanonicalURL         string         `json:"canonicalUrl"`
	JobTitle             string         `json:"jobTitle"`
	CompanyName          string         `json:"companyName"`
	City                 string         `json:"city"`
	Salary               string         `json:"salary"`
	Responsibilities     string         `json:"responsibilities"`
	Requirements         string         `json:"requirements"`
	PlatformStatus       PlatformStatus `json:"platformStatus"`
	PlatformClosedReason string         `json:"platformClosedReason,omitempty"`
}

type DiscoveryPage struct {
	Observations []JobObservation `json:"observations"`
	HasMore      bool             `json:"hasMore"`
}

type FetchErrorCategory string

const (
	FetchErrorTransient             FetchErrorCategory = "transient"
	FetchErrorAuthenticationExpired FetchErrorCategory = "authentication_expired"
	FetchErrorVerificationRequired  FetchErrorCategory = "verification_required"
	FetchErrorPlatformLimited       FetchErrorCategory = "platform_limited"
	FetchErrorInvalidResponse       FetchErrorCategory = "invalid_response"
	FetchErrorInvalidProtocol       FetchErrorCategory = "invalid_protocol"
)

type FetchError struct {
	Category FetchErrorCategory
	Cause    error
}

func (e *FetchError) Error() string {
	return e.Cause.Error()
}

func (e *FetchError) Unwrap() error {
	return e.Cause
}

type Status string

const (
	StatusRunning   Status = "running"
	StatusFailed    Status = "failed"
	StatusCompleted Status = "completed"
)

type RunView struct {
	ID             int64       `json:"-"`
	ResumeVersion  int         `json:"resumeVersion"`
	SearchRange    SearchRange `json:"searchRange"`
	Status         Status      `json:"status"`
	CurrentPage    int         `json:"currentPage"`
	DiscoveredJobs int         `json:"discoveredJobs"`
}

type ActiveResumeUse struct {
	DiscoveryRunID int64 `json:"-"`
	ResumeVersion  int   `json:"resumeVersion"`
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

func New(
	db *sql.DB,
	resumeVersions *onlineresume.Versions,
	pool *jobpool.Pool,
	discovery JobDiscovery,
	logs *runlog.Log,
	now func() time.Time,
) *Service {
	return &Service{
		resumeVersions: resumeVersions,
		pool:           pool,
		discovery:      discovery,
		logs:           logs,
		now:            now,
		queries:        sqlitedb.New(db),
	}
}

func (s *Service) GetActiveResumeUse(ctx context.Context) (*ActiveResumeUse, error) {
	row, err := s.queries.GetActiveDiscoveryResumeUse(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query active discovery resume use: %w", err)
	}
	return &ActiveResumeUse{
		DiscoveryRunID: row.DiscoveryRunID,
		ResumeVersion:  int(row.ResumeVersionNo),
	}, nil
}

func (s *Service) GetLatestRun(ctx context.Context) (*RunView, error) {
	row, err := s.queries.GetLatestDiscoveryRun(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query latest discovery run: %w", err)
	}
	searchRange, err := searchRangeFromSavedResume(row.ResumeJson)
	if err != nil {
		return nil, err
	}
	jobs, err := s.pool.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	return &RunView{
		ID:             row.ID,
		ResumeVersion:  int(row.ResumeVersionNo),
		SearchRange:    searchRange,
		Status:         Status(row.Status),
		CurrentPage:    int(row.NextPage.Int64),
		DiscoveredJobs: len(jobs),
	}, nil
}

func (s *Service) StartAvailability(ctx context.Context) (ActionAvailability, error) {
	current, err := s.resumeVersions.GetCurrent(ctx)
	if err != nil {
		return ActionAvailability{}, err
	}
	if current == nil {
		return ActionAvailability{
			Code:   "online_resume_required",
			Reason: "请先刷新在线简历，再开始岗位发现",
		}, nil
	}
	if len(current.Content.JobIntentions) != 1 {
		return ActionAvailability{
			Code:   "single_search_range_required",
			Reason: "当前岗位发现切片要求在线简历只有一个搜索范围",
		}, nil
	}
	active, err := s.GetActiveResumeUse(ctx)
	if err != nil {
		return ActionAvailability{}, err
	}
	if active != nil {
		return ActionAvailability{
			Code:   "unfinished_discovery_exists",
			Reason: "请先处理当前未结束的岗位发现运行",
		}, nil
	}
	return ActionAvailability{Allowed: true}, nil
}

func (s *Service) Start(ctx context.Context) (int64, error) {
	availability, err := s.StartAvailability(ctx)
	if err != nil {
		return 0, err
	}
	if !availability.Allowed {
		return 0, &Rejection{Code: availability.Code, Reason: availability.Reason}
	}
	current, err := s.resumeVersions.GetCurrent(ctx)
	if err != nil {
		return 0, err
	}
	searchRange := searchRangeFromIntention(current.Content.JobIntentions[0])
	runID, err := s.createRun(ctx, current.ID, searchRange)
	if err != nil {
		return 0, err
	}
	return runID, s.completeRange(ctx, runID, searchRange)
}

func (s *Service) createRun(
	ctx context.Context,
	resumeVersionID int64,
	searchRange SearchRange,
) (int64, error) {
	now := s.now()
	runID, err := s.queries.CreateSingleRangeDiscoveryRun(ctx, sqlitedb.CreateSingleRangeDiscoveryRunParams{
		ResumeVersionID: sql.NullInt64{Int64: resumeVersionID, Valid: true},
		CurrentRole:     sql.NullString{String: searchRange.Role, Valid: true},
		CurrentCity:     sql.NullString{String: searchRange.City, Valid: true},
		WorkerOwner:     sql.NullString{String: "inline-t03", Valid: true},
		WorkerLeaseUntil: sql.NullInt64{
			Int64: now.Add(inlineWorkerLease).UnixMilli(), Valid: true,
		},
		CreatedAt:  now.UnixMilli(),
		PreparedAt: sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		UpdatedAt:  now.UnixMilli(),
	})
	if err != nil {
		return 0, fmt.Errorf("create single-range discovery run: %w", err)
	}
	return runID, nil
}

func (s *Service) completeRange(ctx context.Context, runID int64, searchRange SearchRange) error {
	for pageNo := 1; ; pageNo++ {
		page, err := s.fetchReliablePage(ctx, runID, searchRange, pageNo)
		if err != nil {
			return s.failRun(ctx, runID, err)
		}
		if err := s.observePage(ctx, runID, page); err != nil {
			return s.failRun(ctx, runID, err)
		}
		if !page.HasMore {
			return s.completeRun(ctx, runID, pageNo)
		}
		if err := s.advancePage(ctx, runID, pageNo); err != nil {
			return s.failRun(ctx, runID, err)
		}
	}
}

func (s *Service) fetchReliablePage(
	ctx context.Context,
	runID int64,
	searchRange SearchRange,
	pageNo int,
) (DiscoveryPage, error) {
	attempt := runlog.Attempt{
		Flow:           runlog.FlowDiscovery,
		Operation:      runlog.OperationFetchPage,
		DiscoveryRunID: runID,
		AttemptNo:      1,
		SearchRole:     searchRange.Role,
		SearchCity:     searchRange.City,
		PageNo:         pageNo,
	}
	trace, err := s.logs.Start(ctx, attempt)
	if err != nil {
		return DiscoveryPage{}, fmt.Errorf("start discovery page trace: %w", err)
	}
	page, fetchErr := s.discovery.FetchPage(ctx, searchRange, pageNo)
	if fetchErr != nil {
		category := fetchErrorCategory(fetchErr)
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: category, Err: fetchErr,
		})
		return DiscoveryPage{}, errors.Join(fmt.Errorf("fetch discovery page %d: %w", pageNo, fetchErr), finishErr)
	}
	if validationErr := ValidatePage(page); validationErr != nil {
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: runlog.ErrorCategoryInvalidResponse, Err: validationErr,
		})
		return DiscoveryPage{}, errors.Join(validationErr, finishErr)
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		return DiscoveryPage{}, fmt.Errorf("finish discovery page trace: %w", err)
	}
	return page, nil
}

func (s *Service) observePage(ctx context.Context, runID int64, page DiscoveryPage) error {
	observedAt := s.now()
	for _, observation := range page.Observations {
		if _, err := s.pool.Observe(ctx, runID, toPoolObservation(observation, observedAt)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) advancePage(ctx context.Context, runID int64, currentPage int) error {
	now := s.now().UnixMilli()
	_, err := s.queries.AdvanceSingleRangeDiscoveryPage(ctx, sqlitedb.AdvanceSingleRangeDiscoveryPageParams{
		NextPage:       sql.NullInt64{Int64: int64(currentPage + 1), Valid: true},
		LastProgressAt: sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:      now,
		ID:             runID,
		NextPage_2:     sql.NullInt64{Int64: int64(currentPage), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("advance discovery run %d from page %d: %w", runID, currentPage, err)
	}
	return nil
}

func (s *Service) completeRun(ctx context.Context, runID int64, currentPage int) error {
	now := s.now().UnixMilli()
	_, err := s.queries.CompleteSingleRangeDiscoveryRun(ctx, sqlitedb.CompleteSingleRangeDiscoveryRunParams{
		LastProgressAt: sql.NullInt64{Int64: now, Valid: true},
		FinishedAt:     sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:      now,
		ID:             runID,
		NextPage:       sql.NullInt64{Int64: int64(currentPage), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("complete discovery run %d: %w", runID, err)
	}
	return nil
}

func (s *Service) failRun(ctx context.Context, runID int64, cause error) error {
	_, err := s.queries.FailSingleRangeDiscoveryRun(ctx, sqlitedb.FailSingleRangeDiscoveryRunParams{
		UpdatedAt: s.now().UnixMilli(),
		ID:        runID,
	})
	if err != nil {
		return errors.Join(cause, fmt.Errorf("mark discovery run %d failed: %w", runID, err))
	}
	return cause
}

// ValidatePage enforces the complete-page contract implemented by production
// and controlled JobDiscovery adapters.
func ValidatePage(page DiscoveryPage) error {
	for index, observation := range page.Observations {
		if err := validateObservation(observation); err != nil {
			return fmt.Errorf("discovery page observation %d is unreliable: %w", index+1, err)
		}
	}
	return nil
}

func validateObservation(observation JobObservation) error {
	required := []string{
		observation.PlatformJobID, observation.CanonicalURL, observation.JobTitle,
		observation.CompanyName, observation.City, observation.Salary,
		observation.Responsibilities, observation.Requirements,
	}
	for _, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("stable ID, basic information, and complete JD are required")
		}
	}
	switch observation.PlatformStatus {
	case PlatformStatusOpen:
		if observation.PlatformClosedReason != "" {
			return fmt.Errorf("open platform job cannot have a closed reason")
		}
	case PlatformStatusClosed:
		if strings.TrimSpace(observation.PlatformClosedReason) == "" {
			return fmt.Errorf("closed platform job requires a reliable reason")
		}
	default:
		return fmt.Errorf("reliable platform status is required")
	}
	return nil
}

func toPoolObservation(observation JobObservation, observedAt time.Time) jobpool.Observation {
	return jobpool.Observation{
		PlatformJobID:        observation.PlatformJobID,
		CanonicalURL:         observation.CanonicalURL,
		JobTitle:             observation.JobTitle,
		CompanyName:          observation.CompanyName,
		City:                 observation.City,
		Salary:               observation.Salary,
		Responsibilities:     observation.Responsibilities,
		Requirements:         observation.Requirements,
		PlatformStatus:       jobpool.PlatformStatus(observation.PlatformStatus),
		PlatformClosedReason: observation.PlatformClosedReason,
		ObservedAt:           observedAt,
	}
}

func searchRangeFromIntention(intention onlineresume.JobIntention) SearchRange {
	return SearchRange{
		Role:           intention.Role,
		City:           intention.City,
		Salary:         intention.Salary,
		EmploymentType: intention.EmploymentType,
	}
}

func searchRangeFromSavedResume(resumeJSON string) (SearchRange, error) {
	var content onlineresume.ResumeContent
	if err := json.Unmarshal([]byte(resumeJSON), &content); err != nil {
		return SearchRange{}, fmt.Errorf("decode discovery online resume: %w", err)
	}
	if len(content.JobIntentions) != 1 {
		return SearchRange{}, fmt.Errorf("single-range discovery resume has %d job intentions", len(content.JobIntentions))
	}
	return searchRangeFromIntention(content.JobIntentions[0]), nil
}

func fetchErrorCategory(err error) runlog.ErrorCategory {
	var fetchErr *FetchError
	if !errors.As(err, &fetchErr) {
		return runlog.ErrorCategoryUnknown
	}
	switch fetchErr.Category {
	case FetchErrorTransient:
		return runlog.ErrorCategoryTransient
	case FetchErrorAuthenticationExpired:
		return runlog.ErrorCategoryAuthenticationExpired
	case FetchErrorVerificationRequired:
		return runlog.ErrorCategoryVerificationRequired
	case FetchErrorPlatformLimited:
		return runlog.ErrorCategoryPlatformLimited
	case FetchErrorInvalidResponse:
		return runlog.ErrorCategoryInvalidResponse
	case FetchErrorInvalidProtocol:
		return runlog.ErrorCategoryInvalidProtocol
	default:
		return runlog.ErrorCategoryUnknown
	}
}
