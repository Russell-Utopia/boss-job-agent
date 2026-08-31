package jobpool

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool/internal/sqlitedb"
)

// Pool owns the global platform job state machine.
type Pool struct {
	queries *sqlitedb.Queries
}

type PlatformStatus string

const (
	PlatformStatusOpen   PlatformStatus = "open"
	PlatformStatusClosed PlatformStatus = "closed"
)

type Observation struct {
	PlatformJobID        string
	CanonicalURL         string
	JobTitle             string
	CompanyName          string
	City                 string
	Salary               string
	Responsibilities     string
	Requirements         string
	PlatformStatus       PlatformStatus
	PlatformClosedReason string
	ObservedAt           time.Time
}

type JobView struct {
	ID                   int64
	PlatformJobID        string
	CanonicalURL         string
	JobTitle             string
	CompanyName          string
	City                 string
	Salary               string
	Responsibilities     string
	Requirements         string
	PlatformStatus       PlatformStatus
	PlatformClosedReason string
	FirstSeenAt          time.Time
	LastSeenAt           time.Time
}

type OutreachAuthorization struct {
	GreetingText    string
	TimeDescription string
}

type ActionAvailability struct {
	Allowed bool
	Code    string
	Reason  string
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

func New(db *sql.DB) *Pool {
	return &Pool{queries: sqlitedb.New(db)}
}

func (p *Pool) Observe(ctx context.Context, runID int64, observation Observation) (JobView, error) {
	if runID <= 0 {
		return JobView{}, fmt.Errorf("observe platform job: discovery run ID must be positive")
	}
	jdJSON, jdHash, err := encodeJudgmentContent(observation)
	if err != nil {
		return JobView{}, err
	}
	closedReason := sql.NullString{}
	if observation.PlatformClosedReason != "" {
		closedReason = sql.NullString{String: observation.PlatformClosedReason, Valid: true}
	}
	row, err := p.queries.ObservePlatformJob(ctx, sqlitedb.ObservePlatformJobParams{
		PlatformJobID:           observation.PlatformJobID,
		CanonicalUrl:            observation.CanonicalURL,
		JobTitle:                observation.JobTitle,
		CompanyName:             sql.NullString{String: observation.CompanyName, Valid: true},
		CityText:                sql.NullString{String: observation.City, Valid: true},
		SalaryText:              sql.NullString{String: observation.Salary, Valid: true},
		JdJson:                  jdJSON,
		JdHash:                  jdHash,
		PlatformStatus:          string(observation.PlatformStatus),
		PlatformClosedReason:    closedReason,
		PlatformStatusCheckedAt: observation.ObservedAt.UnixMilli(),
		FirstSeenAt:             observation.ObservedAt.UnixMilli(),
		LastSeenAt:              observation.ObservedAt.UnixMilli(),
		UpdatedAt:               observation.ObservedAt.UnixMilli(),
	})
	if err != nil {
		return JobView{}, fmt.Errorf("observe platform job %q: %w", observation.PlatformJobID, err)
	}
	return jobViewFromObservedRow(row)
}

func (p *Pool) ListJobs(ctx context.Context) ([]JobView, error) {
	rows, err := p.queries.ListPlatformJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list platform jobs: %w", err)
	}
	jobs := make([]JobView, 0, len(rows))
	for _, row := range rows {
		job, err := jobViewFromListRow(row)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

type judgmentContent struct {
	JobTitle         string `json:"jobTitle"`
	CompanyName      string `json:"companyName"`
	City             string `json:"city"`
	Salary           string `json:"salary"`
	Responsibilities string `json:"responsibilities"`
	Requirements     string `json:"requirements"`
}

func encodeJudgmentContent(observation Observation) (string, string, error) {
	content := judgmentContent{
		JobTitle:         normalizeText(observation.JobTitle),
		CompanyName:      normalizeText(observation.CompanyName),
		City:             normalizeText(observation.City),
		Salary:           normalizeText(observation.Salary),
		Responsibilities: normalizeText(observation.Responsibilities),
		Requirements:     normalizeText(observation.Requirements),
	}
	if err := validateObservationContent(observation, content); err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", "", fmt.Errorf("encode platform job %q JD: %w", observation.PlatformJobID, err)
	}
	digest := sha256.Sum256(encoded)
	return string(encoded), hex.EncodeToString(digest[:]), nil
}

func validateObservationContent(observation Observation, content judgmentContent) error {
	if observation.PlatformJobID == "" {
		return fmt.Errorf("observe platform job: stable platform ID and canonical URL are required")
	}
	if observation.CanonicalURL == "" {
		return fmt.Errorf("observe platform job: stable platform ID and canonical URL are required")
	}
	if hasEmptyText(content.JobTitle, content.CompanyName, content.City, content.Salary) {
		return fmt.Errorf("observe platform job %q: basic job information is incomplete", observation.PlatformJobID)
	}
	if hasEmptyText(content.Responsibilities, content.Requirements) {
		return fmt.Errorf("observe platform job %q: complete JD is required", observation.PlatformJobID)
	}
	if err := validatePlatformStatus(observation); err != nil {
		return err
	}
	if observation.ObservedAt.IsZero() {
		return fmt.Errorf("observe platform job %q: reliable observation time is required", observation.PlatformJobID)
	}
	return nil
}

func hasEmptyText(values ...string) bool {
	for _, value := range values {
		if value == "" {
			return true
		}
	}
	return false
}

func validatePlatformStatus(observation Observation) error {
	switch observation.PlatformStatus {
	case PlatformStatusOpen:
		if observation.PlatformClosedReason != "" {
			return fmt.Errorf("observe platform job %q: open job cannot have a closed reason", observation.PlatformJobID)
		}
	case PlatformStatusClosed:
		if normalizeText(observation.PlatformClosedReason) == "" {
			return fmt.Errorf("observe platform job %q: closed job requires a reliable reason", observation.PlatformJobID)
		}
	default:
		return fmt.Errorf("observe platform job %q: reliable platform status is required", observation.PlatformJobID)
	}
	return nil
}

func normalizeText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(lines[index])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func jobViewFromObservedRow(row sqlitedb.ObservePlatformJobRow) (JobView, error) {
	return newJobView(
		row.ID, row.PlatformJobID, row.CanonicalUrl, row.JobTitle,
		row.CompanyName, row.CityText, row.SalaryText, row.JdJson,
		row.PlatformStatus, row.PlatformClosedReason, row.FirstSeenAt, row.LastSeenAt,
	)
}

func jobViewFromListRow(row sqlitedb.ListPlatformJobsRow) (JobView, error) {
	return newJobView(
		row.ID, row.PlatformJobID, row.CanonicalUrl, row.JobTitle,
		row.CompanyName, row.CityText, row.SalaryText, row.JdJson,
		row.PlatformStatus, row.PlatformClosedReason, row.FirstSeenAt, row.LastSeenAt,
	)
}

func newJobView(
	id int64,
	platformJobID, canonicalURL, jobTitle string,
	companyName, city, salary sql.NullString,
	jdJSON, platformStatus string,
	closedReason sql.NullString,
	firstSeenAt, lastSeenAt int64,
) (JobView, error) {
	var content judgmentContent
	if err := json.Unmarshal([]byte(jdJSON), &content); err != nil {
		return JobView{}, fmt.Errorf("decode platform job %q JD: %w", platformJobID, err)
	}
	return JobView{
		ID:                   id,
		PlatformJobID:        platformJobID,
		CanonicalURL:         canonicalURL,
		JobTitle:             jobTitle,
		CompanyName:          companyName.String,
		City:                 city.String,
		Salary:               salary.String,
		Responsibilities:     content.Responsibilities,
		Requirements:         content.Requirements,
		PlatformStatus:       PlatformStatus(platformStatus),
		PlatformClosedReason: closedReason.String,
		FirstSeenAt:          time.UnixMilli(firstSeenAt),
		LastSeenAt:           time.UnixMilli(lastSeenAt),
	}, nil
}

func (p *Pool) OutreachAvailability(_ context.Context) ActionAvailability {
	return ActionAvailability{
		Code:   "outreach_unavailable",
		Reason: "当前没有可真实打招呼的岗位",
	}
}

func (p *Pool) QueueAuthorizedOutreach(
	ctx context.Context,
	_ []int64,
	_ OutreachAuthorization,
) error {
	availability := p.OutreachAvailability(ctx)
	return &Rejection{Code: availability.Code, Reason: availability.Reason}
}
