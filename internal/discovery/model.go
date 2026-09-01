package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

// JobDiscovery is the BOSS search seam owned by this module.
type JobDiscovery interface {
	ListPage(context.Context, SearchRange, int) (JobPage, error)
	ReadJob(context.Context, string) (JobObservation, error)
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

type JobPage struct {
	PlatformJobIDs []string `json:"platformJobIds"`
	HasMore        bool     `json:"hasMore"`
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
	Evidence *FetchFailureEvidence
	Cause    error
}

// FetchFailureEvidence identifies the first failed upstream request without
// retaining request material or response content.
type FetchFailureEvidence struct {
	RequestOrdinal int
	Stage          string
	DetailOrdinal  int
	UpstreamCode   string
}

func (e *FetchError) Error() string {
	return e.Cause.Error()
}

func (e *FetchError) Unwrap() error {
	return e.Cause
}

type Status string

const (
	StatusPreparing  Status = "preparing"
	StatusRunning    Status = "running"
	StatusPaused     Status = "paused"
	StatusFailed     Status = "failed"
	StatusCompleted  Status = "completed"
	StatusEndedEarly Status = "ended_early"
)

type RunView struct {
	ID              int64       `json:"-"`
	ResumeVersion   int         `json:"resumeVersion"`
	CompletedRanges int         `json:"completedRanges"`
	TotalRanges     int         `json:"totalRanges"`
	ProgressPercent int         `json:"progressPercent"`
	CurrentRange    SearchRange `json:"currentRange"`
	NextPage        int         `json:"nextPage"`
	DiscoveredJobs  int         `json:"discoveredJobs"`
	Status          Status      `json:"status"`
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

func validateObservation(observation JobObservation) error {
	required := []string{
		observation.PlatformJobID, observation.CanonicalURL, observation.JobTitle,
		observation.CompanyName, observation.City,
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

func searchRangesFromSavedResume(resumeJSON string) ([]SearchRange, error) {
	var content onlineresume.ResumeContent
	if err := json.Unmarshal([]byte(resumeJSON), &content); err != nil {
		return nil, fmt.Errorf("decode discovery online resume: %w", err)
	}
	if len(content.JobIntentions) == 0 {
		return nil, fmt.Errorf("discovery online resume has no search ranges")
	}
	ranges := make([]SearchRange, len(content.JobIntentions))
	for index, intention := range content.JobIntentions {
		ranges[index] = searchRangeFromIntention(intention)
	}
	return ranges, nil
}

func findSearchRange(ranges []SearchRange, role, city string) (int, error) {
	for index, searchRange := range ranges {
		if searchRange.Role == role && searchRange.City == city {
			return index, nil
		}
	}
	return 0, fmt.Errorf("saved discovery checkpoint %q in %q is not in the frozen online resume", role, city)
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

func runlogFailureEvidence(err error) *runlog.ExternalFailureEvidence {
	var fetchErr *FetchError
	if !errors.As(err, &fetchErr) || fetchErr.Evidence == nil {
		return nil
	}
	return &runlog.ExternalFailureEvidence{
		RequestOrdinal: fetchErr.Evidence.RequestOrdinal,
		Stage:          fetchErr.Evidence.Stage,
		DetailOrdinal:  fetchErr.Evidence.DetailOrdinal,
		UpstreamCode:   fetchErr.Evidence.UpstreamCode,
	}
}
