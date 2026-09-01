package jobpool

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool/internal/sqlitedb"
)

// Pool owns the global platform job state machine.
type Pool struct {
	db      *sql.DB
	queries *sqlitedb.Queries
	now     func() time.Time
}

const (
	unattendedAttemptLimit           int64 = 3
	assessmentRetriesExhaustedReason       = "自动重试已达上限，请手工重试"
)

type PlatformStatus string

const (
	PlatformStatusOpen   PlatformStatus = "open"
	PlatformStatusClosed PlatformStatus = "closed"
)

type AssessmentStatus string

const (
	AssessmentStatusNotQueued             AssessmentStatus = "not_queued"
	AssessmentStatusPending               AssessmentStatus = "pending"
	AssessmentStatusProcessing            AssessmentStatus = "processing"
	AssessmentStatusSuitable              AssessmentStatus = "suitable"
	AssessmentStatusUnsuitable            AssessmentStatus = "unsuitable"
	AssessmentStatusNeedsUserConfirmation AssessmentStatus = "needs_user_confirmation"
	AssessmentStatusFailed                AssessmentStatus = "failed"
)

type HumanVerdict string

const (
	HumanVerdictSuitable   HumanVerdict = "suitable"
	HumanVerdictUnsuitable HumanVerdict = "unsuitable"
)

type OutreachStatus string

const (
	OutreachStatusNotQueued         OutreachStatus = "not_queued"
	OutreachStatusPending           OutreachStatus = "pending"
	OutreachStatusProcessing        OutreachStatus = "processing"
	OutreachStatusContacted         OutreachStatus = "contacted"
	OutreachStatusPossiblyContacted OutreachStatus = "possibly_contacted"
	OutreachStatusFailed            OutreachStatus = "failed"
)

type OutreachMode string

const (
	OutreachModeContact   OutreachMode = "contact"
	OutreachModeReconcile OutreachMode = "reconcile"
)

type ContactSource string

const (
	ContactSourceAgent        ContactSource = "agent"
	ContactSourceBossExisting ContactSource = "boss_existing"
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
	ID                    int64
	PlatformJobID         string
	CanonicalURL          string
	JobTitle              string
	CompanyName           string
	City                  string
	Salary                string
	Responsibilities      string
	Requirements          string
	JDHash                string
	PlatformStatus        PlatformStatus
	PlatformClosedReason  string
	AssessmentStatus      AssessmentStatus
	AssessmentJDHash      string
	AssessmentAttemptNo   int64
	AssessmentReason      string
	AssessmentEvidence    json.RawMessage
	AssessmentLeaseOwner  string
	AssessmentLeaseUntil  *time.Time
	HumanVerdict          HumanVerdict
	HumanReviewedJDHash   string
	HumanReviewedAt       *time.Time
	HumanReviewNote       string
	OutreachStatus        OutreachStatus
	OutreachGreetingText  string
	OutreachAttemptNo     int64
	OutreachEvidence      json.RawMessage
	ContactSource         ContactSource
	ContactedAt           *time.Time
	OutreachLeaseOwner    string
	OutreachLeaseUntil    *time.Time
	AssessmentAction      ActionAvailability
	AssessmentRetryAction ActionAvailability
	ReviewAction          ActionAvailability
	OutreachAction        ActionAvailability
	FirstSeenAt           time.Time
	LastSeenAt            time.Time
}

type AssessmentInputVersions struct {
	ResumeVersion    int64
	PolicyVersion    int64
	EvaluatorVersion int64
}

type JudgmentSource string

const (
	JudgmentSourceAI    JudgmentSource = "ai"
	JudgmentSourceHuman JudgmentSource = "human"
)

type JudgmentVerdict string

const (
	JudgmentVerdictSuitable   JudgmentVerdict = "suitable"
	JudgmentVerdictUnsuitable JudgmentVerdict = "unsuitable"
)

type HumanReviewStatus string

const (
	HumanReviewStatusUnreviewed HumanReviewStatus = "unreviewed"
	HumanReviewStatusSuitable   HumanReviewStatus = "suitable"
	HumanReviewStatusUnsuitable HumanReviewStatus = "unsuitable"
	HumanReviewStatusStale      HumanReviewStatus = "stale"
)

type CurrentJudgment struct {
	Available bool
	Verdict   JudgmentVerdict
	Source    JudgmentSource
	Code      string
	Reason    string
}

type JobDetailView struct {
	JobView
	AssessmentInputs  AssessmentInputVersions
	CurrentJudgment   CurrentJudgment
	HumanReviewStatus HumanReviewStatus
	SupervisionLabel  HumanVerdict
}

// HumanReviewSample is a current-JD human review that can supervise policy
// optimization. It contains the complete job input so the advisor never has
// to reach into JobPool's database or reconstruct a partial example.
type HumanReviewSample struct {
	JobID            int64        `json:"jobId"`
	PlatformJobID    string       `json:"platformJobId"`
	CanonicalURL     string       `json:"canonicalUrl"`
	JobTitle         string       `json:"jobTitle"`
	CompanyName      string       `json:"companyName"`
	City             string       `json:"city"`
	Salary           string       `json:"salary"`
	Responsibilities string       `json:"responsibilities"`
	Requirements     string       `json:"requirements"`
	JDHash           string       `json:"jdHash"`
	Verdict          HumanVerdict `json:"verdict"`
	ReviewedAt       time.Time    `json:"reviewedAt"`
	Note             string       `json:"note"`
}

type ReviewDecision struct {
	JobID          int64
	ExpectedJDHash string
	Verdict        HumanVerdict
	Note           string
}

type SkippedAction struct {
	JobID  int64  `json:"jobId"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

type BatchActionResult struct {
	Succeeded int             `json:"succeeded"`
	Skipped   []SkippedAction `json:"skipped"`
}

type AssessmentClaim struct {
	Worker           string
	ResumeVersionID  int64
	PolicyVersionID  int64
	EvaluatorVersion int64
	ProcessingLimit  int
	ClaimedAt        time.Time
	LeaseUntil       time.Time
}

type AssessmentWork struct {
	JobID            int64
	PlatformJobID    string
	CanonicalURL     string
	JobTitle         string
	CompanyName      string
	City             string
	Salary           string
	Responsibilities string
	Requirements     string
	JDHash           string
	ResumeVersionID  int64
	PolicyVersionID  int64
	EvaluatorVersion int64
	AttemptNo        int64
	LeaseUntil       time.Time
}

type AssessmentOutcome struct {
	JobID       int64
	AttemptNo   int64
	Status      AssessmentStatus
	Reason      string
	Evidence    json.RawMessage
	RetryAt     *time.Time
	CompletedAt time.Time
}

type OutreachClaim struct {
	Worker     string
	Limit      int
	ClaimedAt  time.Time
	LeaseUntil time.Time
}

type OutreachWork struct {
	JobID            int64
	PlatformJobID    string
	CanonicalURL     string
	JobTitle         string
	CompanyName      string
	City             string
	Salary           string
	Responsibilities string
	Requirements     string
	JDHash           string
	GreetingText     string
	AttemptNo        int64
	Mode             OutreachMode
	LeaseUntil       time.Time
}

type OutreachOutcome struct {
	JobID         int64
	AttemptNo     int64
	Status        OutreachStatus
	ContactSource ContactSource
	Evidence      json.RawMessage
	RetryAt       *time.Time
	CompletedAt   time.Time
}

type OutreachAuthorization struct {
	GreetingText    string
	TimeDescription string
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

func New(db *sql.DB) *Pool {
	return newPool(db, time.Now)
}

func newPool(db *sql.DB, now func() time.Time) *Pool {
	return &Pool{db: db, queries: sqlitedb.New(db), now: now}
}

func (p *Pool) Observe(ctx context.Context, runID int64, observation Observation) (JobView, error) {
	return p.observe(ctx, p.queries, runID, observation)
}

// ObserveInTransaction lets an owning workflow atomically validate its work
// token and persist the resulting global job through JobPool's own queries.
func (p *Pool) ObserveInTransaction(
	ctx context.Context,
	transaction *sql.Tx,
	runID int64,
	observation Observation,
) (JobView, error) {
	if transaction == nil {
		return JobView{}, fmt.Errorf("observe platform job: transaction is required")
	}
	return p.observe(ctx, p.queries.WithTx(transaction), runID, observation)
}

func (p *Pool) observe(
	ctx context.Context,
	queries *sqlitedb.Queries,
	runID int64,
	observation Observation,
) (JobView, error) {
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
	row, err := queries.ObservePlatformJob(ctx, sqlitedb.ObservePlatformJobParams{
		PlatformJobID:           observation.PlatformJobID,
		CanonicalUrl:            observation.CanonicalURL,
		JobTitle:                observation.JobTitle,
		CompanyName:             sql.NullString{String: observation.CompanyName, Valid: true},
		CityText:                sql.NullString{String: observation.City, Valid: true},
		SalaryText:              optionalText(observation.Salary),
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
		job, err := jobViewFromPlatformRow(row)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// ListEffectiveHumanReviews returns only reviews whose recorded JD is still
// the current JD. Stale reviews remain visible on the job but cannot affect a
// newly generated policy candidate.
func (p *Pool) ListEffectiveHumanReviews(ctx context.Context) ([]HumanReviewSample, error) {
	rows, err := p.queries.ListEffectiveHumanReviews(ctx)
	if err != nil {
		return nil, fmt.Errorf("list effective human reviews: %w", err)
	}
	samples := make([]HumanReviewSample, 0, len(rows))
	for _, row := range rows {
		job, err := newJobView(
			row.ID, row.PlatformJobID, row.CanonicalUrl, row.JobTitle,
			row.CompanyName, row.CityText, row.SalaryText, row.JdJson, row.JdHash,
			"", sql.NullString{}, 0, 0,
		)
		if err != nil {
			return nil, fmt.Errorf("decode policy sample job %d: %w", row.ID, err)
		}
		if !row.HumanReviewedAt.Valid {
			return nil, fmt.Errorf("policy sample job %d has no review time", row.ID)
		}
		samples = append(samples, HumanReviewSample{
			JobID: job.ID, PlatformJobID: job.PlatformJobID, CanonicalURL: job.CanonicalURL,
			JobTitle: job.JobTitle, CompanyName: job.CompanyName, City: job.City, Salary: job.Salary,
			Responsibilities: job.Responsibilities, Requirements: job.Requirements, JDHash: job.JDHash,
			Verdict: HumanVerdict(row.HumanVerdict.String), ReviewedAt: time.UnixMilli(row.HumanReviewedAt.Int64),
			Note: row.HumanReviewNote.String,
		})
	}
	return samples, nil
}

func (p *Pool) GetJob(ctx context.Context, jobID int64) (JobView, error) {
	row, err := p.queries.GetPlatformJob(ctx, jobID)
	if err != nil {
		return JobView{}, fmt.Errorf("get platform job %d: %w", jobID, err)
	}
	return jobViewFromPlatformRow(row)
}

func (p *Pool) GetJobDetail(ctx context.Context, jobID int64) (JobDetailView, error) {
	transaction, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return JobDetailView{}, fmt.Errorf("get platform job %d detail: begin transaction: %w", jobID, err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	row, err := queries.GetPlatformJob(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return JobDetailView{}, &Rejection{Code: "platform_job_not_found", Reason: "岗位不存在"}
	}
	if err != nil {
		return JobDetailView{}, fmt.Errorf("get platform job %d: %w", jobID, err)
	}
	job, err := jobViewFromPlatformRow(row)
	if err != nil {
		return JobDetailView{}, err
	}
	versions, err := queries.GetAssessmentInputVersions(ctx, jobID)
	if err != nil {
		return JobDetailView{}, fmt.Errorf("get platform job %d assessment inputs: %w", jobID, err)
	}
	review := classifyHumanReview(job)
	detail := JobDetailView{
		JobView: job,
		AssessmentInputs: AssessmentInputVersions{
			ResumeVersion:    versions.ResumeVersion.Int64,
			PolicyVersion:    versions.PolicyVersion.Int64,
			EvaluatorVersion: versions.EvaluatorVersion.Int64,
		},
		CurrentJudgment:   currentJudgment(job, review),
		HumanReviewStatus: review.status,
	}
	if review.valid {
		detail.SupervisionLabel = job.HumanVerdict
	}
	if err := transaction.Commit(); err != nil {
		return JobDetailView{}, fmt.Errorf("get platform job %d detail: commit transaction: %w", jobID, err)
	}
	return detail, nil
}

type humanReviewClassification struct {
	status HumanReviewStatus
	valid  bool
}

func classifyHumanReview(job JobView) humanReviewClassification {
	if job.HumanVerdict == "" {
		return humanReviewClassification{status: HumanReviewStatusUnreviewed}
	}
	if job.HumanReviewedJDHash != job.JDHash {
		return humanReviewClassification{status: HumanReviewStatusStale}
	}
	if job.HumanVerdict == HumanVerdictSuitable {
		return humanReviewClassification{status: HumanReviewStatusSuitable, valid: true}
	}
	return humanReviewClassification{status: HumanReviewStatusUnsuitable, valid: true}
}

func currentJudgment(job JobView, review humanReviewClassification) CurrentJudgment {
	if review.status != HumanReviewStatusUnreviewed {
		if review.status == HumanReviewStatusStale {
			return CurrentJudgment{
				Source: JudgmentSourceHuman,
				Code:   "human_review_stale",
				Reason: "JD 已变化，请先重新人工复核",
			}
		}
		return CurrentJudgment{
			Available: true,
			Verdict:   JudgmentVerdict(job.HumanVerdict),
			Source:    JudgmentSourceHuman,
		}
	}
	switch job.AssessmentStatus {
	case AssessmentStatusSuitable:
		return CurrentJudgment{Available: true, Verdict: JudgmentVerdictSuitable, Source: JudgmentSourceAI}
	case AssessmentStatusUnsuitable:
		return CurrentJudgment{Available: true, Verdict: JudgmentVerdictUnsuitable, Source: JudgmentSourceAI}
	case AssessmentStatusNeedsUserConfirmation:
		return CurrentJudgment{
			Source: JudgmentSourceAI,
			Code:   "user_confirmation_required",
			Reason: "AI 无法可靠判断，请人工复核",
		}
	default:
		return CurrentJudgment{Code: "current_judgment_missing", Reason: "岗位尚无当前判断"}
	}
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
	if hasEmptyText(content.JobTitle, content.CompanyName, content.City) {
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

func optionalText(value string) sql.NullString {
	value = normalizeText(value)
	return sql.NullString{String: value, Valid: value != ""}
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
	return strings.Join(strings.Fields(value), " ")
}

func jobViewFromObservedRow(row sqlitedb.PlatformJob) (JobView, error) {
	return jobViewFromPlatformRow(row)
}

func newJobView(
	id int64,
	platformJobID, canonicalURL, jobTitle string,
	companyName, city, salary sql.NullString,
	jdJSON, jdHash, platformStatus string,
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
		JDHash:               jdHash,
		PlatformStatus:       PlatformStatus(platformStatus),
		PlatformClosedReason: closedReason.String,
		FirstSeenAt:          time.UnixMilli(firstSeenAt),
		LastSeenAt:           time.UnixMilli(lastSeenAt),
	}, nil
}

func jobViewFromPlatformRow(row sqlitedb.PlatformJob) (JobView, error) {
	job, err := newJobView(
		row.ID, row.PlatformJobID, row.CanonicalUrl, row.JobTitle,
		row.CompanyName, row.CityText, row.SalaryText, row.JdJson, row.JdHash,
		row.PlatformStatus, row.PlatformClosedReason, row.FirstSeenAt, row.LastSeenAt,
	)
	if err != nil {
		return JobView{}, err
	}
	job.AssessmentStatus = AssessmentStatus(row.AssessmentStatus)
	job.AssessmentJDHash = row.AssessmentJdHash.String
	job.AssessmentAttemptNo = row.AssessmentAttemptNo
	job.AssessmentReason = row.AssessmentReason.String
	job.AssessmentEvidence = json.RawMessage(row.AssessmentEvidenceJson.String)
	if row.LeaseStage.String == "assessment" {
		job.AssessmentLeaseOwner = row.LeaseOwner.String
		job.AssessmentLeaseUntil = timePointer(row.LeaseUntil)
	}
	job.HumanVerdict = HumanVerdict(row.HumanVerdict.String)
	job.HumanReviewedJDHash = row.HumanReviewedJdHash.String
	job.HumanReviewedAt = timePointer(row.HumanReviewedAt)
	job.HumanReviewNote = row.HumanReviewNote.String
	job.OutreachStatus = OutreachStatus(row.OutreachStatus)
	job.OutreachGreetingText = row.OutreachGreetingText.String
	job.OutreachAttemptNo = row.OutreachAttemptNo
	job.OutreachEvidence = json.RawMessage(row.OutreachEvidenceJson.String)
	job.ContactSource = ContactSource(row.ContactSource.String)
	job.ContactedAt = timePointer(row.ContactedAt)
	if row.LeaseStage.String == "outreach" {
		job.OutreachLeaseOwner = row.LeaseOwner.String
		job.OutreachLeaseUntil = timePointer(row.LeaseUntil)
	}
	job.AssessmentAction = assessmentActionAvailability(row)
	job.AssessmentRetryAction = assessmentRetryActionAvailability(row)
	job.ReviewAction = ActionAvailability{Allowed: true}
	job.OutreachAction = outreachActionAvailability(row)
	return job, nil
}

func assessmentActionAvailability(row sqlitedb.PlatformJob) ActionAvailability {
	if PlatformStatus(row.PlatformStatus) == PlatformStatusOpen &&
		OutreachStatus(row.OutreachStatus) != OutreachStatusContacted &&
		AssessmentStatus(row.AssessmentStatus) == AssessmentStatusNotQueued {
		return ActionAvailability{Allowed: true}
	}
	rejection := assessmentQueueRejection(row)
	return ActionAvailability{Code: rejection.Code, Reason: rejection.Reason}
}

func assessmentRetryActionAvailability(row sqlitedb.PlatformJob) ActionAvailability {
	if PlatformStatus(row.PlatformStatus) == PlatformStatusOpen &&
		OutreachStatus(row.OutreachStatus) != OutreachStatusContacted &&
		AssessmentStatus(row.AssessmentStatus) == AssessmentStatusFailed &&
		!row.LeaseStage.Valid {
		return ActionAvailability{Allowed: true}
	}
	rejection := assessmentQueueRejection(row)
	if AssessmentStatus(row.AssessmentStatus) != AssessmentStatusFailed {
		rejection.Code, rejection.Reason = "assessment_failure_required", "只有明确失败的 AI 鉴定可以重试"
	} else if row.LeaseStage.Valid {
		rejection.Code, rejection.Reason = "assessment_processing", "岗位正在进行 AI 鉴定"
	}
	return ActionAvailability{Code: rejection.Code, Reason: rejection.Reason}
}

func outreachActionAvailability(row sqlitedb.PlatformJob) ActionAvailability {
	if outreachEligible(row) {
		return ActionAvailability{Allowed: true}
	}
	rejection := outreachQueueRejection(row)
	return ActionAvailability{Code: rejection.Code, Reason: rejection.Reason}
}

func outreachEligible(row sqlitedb.PlatformJob) bool {
	if PlatformStatus(row.PlatformStatus) != PlatformStatusOpen ||
		OutreachStatus(row.OutreachStatus) != OutreachStatusNotQueued {
		return false
	}
	if row.HumanVerdict.Valid {
		return HumanVerdict(row.HumanVerdict.String) == HumanVerdictSuitable &&
			row.HumanReviewedJdHash.String == row.JdHash
	}
	return AssessmentStatus(row.AssessmentStatus) == AssessmentStatusSuitable
}

func timePointer(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	parsed := time.UnixMilli(value.Int64)
	return &parsed
}

func (p *Pool) Review(ctx context.Context, decisions []ReviewDecision) error {
	if err := validateReviewDecisions(decisions); err != nil {
		return err
	}
	reviewedAt := p.now()
	if reviewedAt.IsZero() {
		return fmt.Errorf("review platform jobs: current time is required")
	}
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("review platform jobs: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	for _, decision := range decisions {
		if err := reviewPlatformJob(ctx, queries, decision, reviewedAt); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("review platform jobs: commit transaction: %w", err)
	}
	return nil
}

func validateReviewDecisions(decisions []ReviewDecision) error {
	for _, decision := range decisions {
		if err := validateReviewDecision(decision); err != nil {
			return err
		}
	}
	return nil
}

func reviewPlatformJob(
	ctx context.Context,
	queries *sqlitedb.Queries,
	decision ReviewDecision,
	reviewedAt time.Time,
) error {
	_, err := queries.ReviewPlatformJob(ctx, sqlitedb.ReviewPlatformJobParams{
		HumanVerdict:   sql.NullString{String: string(decision.Verdict), Valid: true},
		ReviewedAt:     sql.NullInt64{Int64: reviewedAt.UnixMilli(), Valid: true},
		ReviewNote:     nullableText(decision.Note),
		UpdatedAt:      reviewedAt.UnixMilli(),
		JobID:          decision.JobID,
		ExpectedJdHash: decision.ExpectedJDHash,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("review platform job %d: %w", decision.JobID, err)
	}
	current, currentErr := queries.GetPlatformJob(ctx, decision.JobID)
	if errors.Is(currentErr, sql.ErrNoRows) {
		return &Rejection{Code: "platform_job_not_found", Reason: "岗位不存在"}
	}
	if currentErr != nil {
		return fmt.Errorf("recheck reviewed platform job %d: %w", decision.JobID, currentErr)
	}
	if current.JdHash != decision.ExpectedJDHash {
		return &Rejection{Code: "platform_job_changed", Reason: "JD 已变化，请重新查看完整岗位后再复核"}
	}
	return fmt.Errorf("review platform job %d: update rejected", decision.JobID)
}

func validateReviewDecision(decision ReviewDecision) error {
	if decision.JobID <= 0 {
		return fmt.Errorf("review platform job: job ID must be positive")
	}
	if decision.Verdict != HumanVerdictSuitable && decision.Verdict != HumanVerdictUnsuitable {
		return fmt.Errorf("review platform job %d: verdict must be suitable or unsuitable", decision.JobID)
	}
	if decision.ExpectedJDHash == "" {
		return fmt.Errorf("review platform job %d: expected JD hash is required", decision.JobID)
	}
	return nil
}

func nullableText(value string) sql.NullString {
	value = normalizeText(value)
	return sql.NullString{String: value, Valid: value != ""}
}

func (p *Pool) AdmitAssessments(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("admit assessments: limit must be positive")
	}
	count, err := p.queries.AdmitAssessments(ctx, sqlitedb.AdmitAssessmentsParams{
		UpdatedAt: time.Now().UnixMilli(), AdmitLimit: int64(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("admit assessments: %w", err)
	}
	return int(count), nil
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func (p *Pool) AdmitOutreach(
	ctx context.Context,
	authorization OutreachAuthorization,
	limit int,
) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("admit outreach: limit must be positive")
	}
	greeting, err := validateOutreachAuthorization(authorization)
	if err != nil {
		return 0, err
	}
	count, err := p.queries.AdmitOutreach(ctx, sqlitedb.AdmitOutreachParams{
		GreetingText: sql.NullString{String: greeting, Valid: true},
		UpdatedAt:    time.Now().UnixMilli(), AdmitLimit: int64(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("admit outreach: %w", err)
	}
	return int(count), nil
}

func (p *Pool) QueueAssessments(ctx context.Context, jobIDs []int64) (BatchActionResult, error) {
	if len(jobIDs) == 0 {
		return BatchActionResult{}, &Rejection{Code: "assessment_selection_required", Reason: "请选择要进行 AI 鉴定的岗位"}
	}
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchActionResult{}, fmt.Errorf("queue assessments: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	result := BatchActionResult{}
	updatedAt := time.Now().UnixMilli()
	seen := make(map[int64]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		if _, duplicate := seen[jobID]; duplicate {
			continue
		}
		seen[jobID] = struct{}{}
		_, err := queries.QueueAssessment(ctx, sqlitedb.QueueAssessmentParams{UpdatedAt: updatedAt, JobID: jobID})
		if err == nil {
			result.Succeeded++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return BatchActionResult{}, fmt.Errorf("queue assessment for job %d: %w", jobID, err)
		}
		row, getErr := queries.GetPlatformJob(ctx, jobID)
		if errors.Is(getErr, sql.ErrNoRows) {
			result.Skipped = append(result.Skipped, SkippedAction{
				JobID: jobID, Code: "platform_job_not_found", Reason: "岗位不存在或已发生变化",
			})
			continue
		}
		if getErr != nil {
			return BatchActionResult{}, fmt.Errorf("recheck assessment job %d: %w", jobID, getErr)
		}
		result.Skipped = append(result.Skipped, assessmentQueueRejection(row))
	}
	if err := transaction.Commit(); err != nil {
		return BatchActionResult{}, fmt.Errorf("queue assessments: commit transaction: %w", err)
	}
	return result, nil
}

func (p *Pool) RetryAssessmentFailures(ctx context.Context, jobIDs []int64) (BatchActionResult, error) {
	if len(jobIDs) == 0 {
		return BatchActionResult{}, &Rejection{Code: "assessment_selection_required", Reason: "请选择要重试的 AI 鉴定失败岗位"}
	}
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchActionResult{}, fmt.Errorf("retry assessment failures: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	result := BatchActionResult{}
	updatedAt := time.Now().UnixMilli()
	for _, jobID := range uniqueJobIDs(jobIDs) {
		_, err := queries.RetryAssessmentFailure(ctx, sqlitedb.RetryAssessmentFailureParams{
			UpdatedAt: updatedAt, JobID: jobID,
		})
		if err == nil {
			result.Succeeded++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return BatchActionResult{}, fmt.Errorf("retry assessment for job %d: %w", jobID, err)
		}
		row, getErr := queries.GetPlatformJob(ctx, jobID)
		if errors.Is(getErr, sql.ErrNoRows) {
			result.Skipped = append(result.Skipped, SkippedAction{
				JobID: jobID, Code: "platform_job_not_found", Reason: "岗位不存在或已发生变化",
			})
			continue
		}
		if getErr != nil {
			return BatchActionResult{}, fmt.Errorf("recheck assessment retry job %d: %w", jobID, getErr)
		}
		availability := assessmentRetryActionAvailability(row)
		result.Skipped = append(result.Skipped, SkippedAction{
			JobID: jobID, Code: availability.Code, Reason: availability.Reason,
		})
	}
	if err := transaction.Commit(); err != nil {
		return BatchActionResult{}, fmt.Errorf("retry assessment failures: commit transaction: %w", err)
	}
	return result, nil
}

func (p *Pool) RetryOutreachFailures(ctx context.Context, jobIDs []int64) (BatchActionResult, error) {
	if len(jobIDs) == 0 {
		return BatchActionResult{}, &Rejection{Code: "outreach_selection_required", Reason: "请选择要重试的打招呼失败岗位"}
	}
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchActionResult{}, fmt.Errorf("retry outreach failures: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	result := BatchActionResult{}
	updatedAt := time.Now().UnixMilli()
	for _, jobID := range uniqueJobIDs(jobIDs) {
		succeeded, skipped, err := retryOutreachFailure(ctx, queries, jobID, updatedAt)
		if err != nil {
			return BatchActionResult{}, err
		}
		if succeeded {
			result.Succeeded++
		} else {
			result.Skipped = append(result.Skipped, skipped)
		}
	}
	if err := transaction.Commit(); err != nil {
		return BatchActionResult{}, fmt.Errorf("retry outreach failures: commit transaction: %w", err)
	}
	return result, nil
}

func retryOutreachFailure(
	ctx context.Context,
	queries *sqlitedb.Queries,
	jobID int64,
	updatedAt int64,
) (bool, SkippedAction, error) {
	_, err := queries.RetryOutreachFailure(ctx, sqlitedb.RetryOutreachFailureParams{
		UpdatedAt: updatedAt, JobID: jobID,
	})
	if err == nil {
		return true, SkippedAction{}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, SkippedAction{}, fmt.Errorf("retry outreach for job %d: %w", jobID, err)
	}
	row, err := queries.GetPlatformJob(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, jobNotFoundRejection(jobID), nil
	}
	if err != nil {
		return false, SkippedAction{}, fmt.Errorf("recheck outreach retry job %d: %w", jobID, err)
	}
	return false, outreachRetryRejection(row), nil
}

func outreachRetryRejection(row sqlitedb.PlatformJob) SkippedAction {
	rejection := outreachQueueRejection(row)
	if OutreachStatus(row.OutreachStatus) == OutreachStatusPossiblyContacted {
		rejection.Code, rejection.Reason = "outreach_reconciliation_required", "岗位可能已打招呼，只能先核对而不能重试"
	} else if OutreachStatus(row.OutreachStatus) != OutreachStatusFailed {
		rejection.Code, rejection.Reason = "outreach_failure_required", "只有确认未产生外部影响的打招呼失败可以重试"
	}
	return rejection
}

func jobNotFoundRejection(jobID int64) SkippedAction {
	return SkippedAction{JobID: jobID, Code: "platform_job_not_found", Reason: "岗位不存在或已发生变化"}
}

func uniqueJobIDs(jobIDs []int64) []int64 {
	unique := make([]int64, 0, len(jobIDs))
	seen := make(map[int64]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		if _, duplicate := seen[jobID]; duplicate {
			continue
		}
		seen[jobID] = struct{}{}
		unique = append(unique, jobID)
	}
	return unique
}

func assessmentQueueRejection(row sqlitedb.PlatformJob) SkippedAction {
	rejection := SkippedAction{JobID: row.ID, Code: "assessment_not_eligible", Reason: "当前岗位不符合 AI 鉴定条件"}
	if PlatformStatus(row.PlatformStatus) == PlatformStatusClosed {
		rejection.Code, rejection.Reason = "platform_job_closed", "岗位已关闭，不能开始 AI 鉴定"
		return rejection
	}
	if OutreachStatus(row.OutreachStatus) == OutreachStatusContacted {
		rejection.Code, rejection.Reason = "outreach_already_contacted", "岗位已打过招呼，不再进行 AI 鉴定"
		return rejection
	}
	switch AssessmentStatus(row.AssessmentStatus) {
	case AssessmentStatusPending:
		rejection.Code, rejection.Reason = "assessment_already_queued", "岗位已在等待 AI 鉴定"
	case AssessmentStatusProcessing:
		rejection.Code, rejection.Reason = "assessment_processing", "岗位正在进行 AI 鉴定"
	case AssessmentStatusSuitable, AssessmentStatusUnsuitable, AssessmentStatusNeedsUserConfirmation:
		rejection.Code, rejection.Reason = "assessment_already_completed", "岗位已有当前有效的 AI 鉴定结论"
	case AssessmentStatusFailed:
		rejection.Code, rejection.Reason = "assessment_retry_required", "AI 鉴定已失败，请使用重试操作"
	}
	return rejection
}

func (p *Pool) ClaimAssessments(ctx context.Context, claim AssessmentClaim) ([]AssessmentWork, error) {
	if err := validateAssessmentClaim(claim); err != nil {
		return nil, err
	}
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim assessments: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	if err := expireAssessmentLeases(ctx, queries, claim.ClaimedAt); err != nil {
		return nil, err
	}
	rows, err := queries.ClaimAssessments(ctx, sqlitedb.ClaimAssessmentsParams{
		ResumeVersionID:  sql.NullInt64{Int64: claim.ResumeVersionID, Valid: true},
		PolicyVersionID:  sql.NullInt64{Int64: claim.PolicyVersionID, Valid: true},
		EvaluatorVersion: sql.NullInt64{Int64: claim.EvaluatorVersion, Valid: true},
		Worker:           sql.NullString{String: claim.Worker, Valid: true},
		LeaseUntil:       sql.NullInt64{Int64: claim.LeaseUntil.UnixMilli(), Valid: true},
		UpdatedAt:        claim.ClaimedAt.UnixMilli(),
		ClaimedAt:        sql.NullInt64{Int64: claim.ClaimedAt.UnixMilli(), Valid: true},
		FailureLimit:     unattendedAttemptLimit,
		ProcessingLimit:  int64(claim.ProcessingLimit),
	})
	if err != nil {
		return nil, fmt.Errorf("claim assessments: %w", err)
	}
	work := make([]AssessmentWork, 0, len(rows))
	for _, row := range rows {
		item, err := assessmentWorkFromRow(row)
		if err != nil {
			return nil, err
		}
		work = append(work, item)
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("claim assessments: commit transaction: %w", err)
	}
	return work, nil
}

func expireAssessmentLeases(ctx context.Context, queries *sqlitedb.Queries, now time.Time) error {
	_, err := queries.ExpireAssessmentLeases(ctx, sqlitedb.ExpireAssessmentLeasesParams{
		Reason:       sql.NullString{String: "鉴定执行租约已过期", Valid: true},
		EvidenceJson: sql.NullString{String: `{"code":"assessment_lease_expired"}`, Valid: true},
		ExpiredAt:    sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		UpdatedAt:    now.UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("expire assessment leases: %w", err)
	}
	if err := stopExhaustedAssessmentRetries(ctx, queries); err != nil {
		return fmt.Errorf("stop exhausted assessment retries: %w", err)
	}
	return nil
}

func stopExhaustedAssessmentRetries(ctx context.Context, queries *sqlitedb.Queries) error {
	_, err := queries.StopAssessmentRetriesAtLimit(ctx, sqlitedb.StopAssessmentRetriesAtLimitParams{
		Reason:       nullableText(assessmentRetriesExhaustedReason),
		FailureLimit: unattendedAttemptLimit,
	})
	return err
}

func validateAssessmentClaim(claim AssessmentClaim) error {
	if normalizeText(claim.Worker) == "" {
		return fmt.Errorf("claim assessments: worker is required")
	}
	if claim.ResumeVersionID <= 0 || claim.PolicyVersionID <= 0 || claim.EvaluatorVersion <= 0 {
		return fmt.Errorf("claim assessments: resume, policy and evaluator versions must be positive")
	}
	if claim.ProcessingLimit <= 0 {
		return fmt.Errorf("claim assessments: processing limit must be positive")
	}
	if claim.ClaimedAt.IsZero() || !claim.LeaseUntil.After(claim.ClaimedAt) {
		return fmt.Errorf("claim assessments: a future lease deadline is required")
	}
	return nil
}

func assessmentWorkFromRow(row sqlitedb.ClaimAssessmentsRow) (AssessmentWork, error) {
	var content judgmentContent
	if err := json.Unmarshal([]byte(row.JdJson), &content); err != nil {
		return AssessmentWork{}, fmt.Errorf("decode assessment job %q JD: %w", row.PlatformJobID, err)
	}
	return AssessmentWork{
		JobID: row.ID, PlatformJobID: row.PlatformJobID, CanonicalURL: row.CanonicalUrl,
		JobTitle: row.JobTitle, CompanyName: row.CompanyName.String, City: row.CityText.String,
		Salary: row.SalaryText.String, Responsibilities: content.Responsibilities,
		Requirements: content.Requirements, JDHash: row.JdHash,
		ResumeVersionID:  row.AssessmentResumeVersionID.Int64,
		PolicyVersionID:  row.AssessmentPolicyVersionID.Int64,
		EvaluatorVersion: row.EvaluatorVersion.Int64, AttemptNo: row.AssessmentAttemptNo,
		LeaseUntil: time.UnixMilli(row.LeaseUntil.Int64),
	}, nil
}

func (p *Pool) FinishAssessments(ctx context.Context, outcomes []AssessmentOutcome) (BatchActionResult, error) {
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchActionResult{}, fmt.Errorf("finish assessments: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	result := BatchActionResult{}
	for _, outcome := range outcomes {
		if err := validateAssessmentOutcome(outcome); err != nil {
			return BatchActionResult{}, err
		}
		_, err := queries.FinishAssessment(ctx, sqlitedb.FinishAssessmentParams{
			ResultStatus: string(outcome.Status), Reason: nullableText(outcome.Reason),
			EvidenceJson: sql.NullString{String: string(outcome.Evidence), Valid: true},
			RetryAt:      nullableTime(outcome.RetryAt), CompletedAt: outcome.CompletedAt.UnixMilli(),
			JobID: outcome.JobID, AttemptNo: outcome.AttemptNo,
		})
		if err == nil {
			result.Succeeded++
			continue
		}
		if errors.Is(err, sql.ErrNoRows) {
			result.Skipped = append(result.Skipped, SkippedAction{
				JobID: outcome.JobID, Code: "stale_assessment_attempt", Reason: "AI 鉴定结果已过期，未写入岗位",
			})
			continue
		}
		return BatchActionResult{}, fmt.Errorf("finish assessment for job %d: %w", outcome.JobID, err)
	}
	if err := stopExhaustedAssessmentRetries(ctx, queries); err != nil {
		return BatchActionResult{}, fmt.Errorf("stop exhausted assessment retries: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return BatchActionResult{}, fmt.Errorf("finish assessments: commit transaction: %w", err)
	}
	return result, nil
}

func validateAssessmentOutcome(outcome AssessmentOutcome) error {
	if outcome.JobID <= 0 || outcome.AttemptNo <= 0 {
		return fmt.Errorf("finish assessment: job ID and attempt number must be positive")
	}
	if !validAssessmentOutcomeStatus(outcome.Status) {
		return fmt.Errorf("finish assessment for job %d: invalid result status %q", outcome.JobID, outcome.Status)
	}
	if normalizeText(outcome.Reason) == "" || !json.Valid(outcome.Evidence) || outcome.CompletedAt.IsZero() {
		return fmt.Errorf("finish assessment for job %d: reason, JSON evidence and completion time are required", outcome.JobID)
	}
	return validateRetryTime(
		outcome.Status == AssessmentStatusFailed,
		outcome.RetryAt,
		outcome.CompletedAt,
		fmt.Sprintf("finish assessment for job %d", outcome.JobID),
	)
}

func validAssessmentOutcomeStatus(status AssessmentStatus) bool {
	return status == AssessmentStatusSuitable ||
		status == AssessmentStatusUnsuitable ||
		status == AssessmentStatusNeedsUserConfirmation ||
		status == AssessmentStatusFailed
}

func nullableTime(value *time.Time) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value.UnixMilli(), Valid: true}
}

func (p *Pool) ClaimOutreach(ctx context.Context, claim OutreachClaim) ([]OutreachWork, error) {
	return p.claimOutreach(ctx, claim, true)
}

// ClaimOutreachWithinWindow always permits reconciliation, while allowing
// new contact work only when the caller's current Asia/Shanghai window is
// open. The unrestricted ClaimOutreach method remains the direct JobPool seam
// for business commands and tests.
func (p *Pool) ClaimOutreachWithinWindow(
	ctx context.Context,
	claim OutreachClaim,
	allowContact bool,
) ([]OutreachWork, error) {
	return p.claimOutreach(ctx, claim, allowContact)
}

func (p *Pool) claimOutreach(
	ctx context.Context,
	claim OutreachClaim,
	allowContact bool,
) ([]OutreachWork, error) {
	if err := validateOutreachClaim(claim); err != nil {
		return nil, err
	}
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim outreach: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	_, err = queries.ExpireOutreachLeases(ctx, sqlitedb.ExpireOutreachLeasesParams{
		UpdatedAt: claim.ClaimedAt.UnixMilli(),
		ExpiredAt: sql.NullInt64{Int64: claim.ClaimedAt.UnixMilli(), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("expire outreach leases: %w", err)
	}
	work, err := claimOutreachBatch(ctx, queries, claim, allowContact)
	if err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("claim outreach: commit transaction: %w", err)
	}
	return work, nil
}

func claimOutreachBatch(
	ctx context.Context,
	queries *sqlitedb.Queries,
	claim OutreachClaim,
	allowContact bool,
) ([]OutreachWork, error) {
	work := make([]OutreachWork, 0, claim.Limit)
	for len(work) < claim.Limit {
		item, found, err := claimNextOutreach(ctx, queries, claim, allowContact)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		work = append(work, item)
	}
	return work, nil
}

func claimNextOutreach(
	ctx context.Context,
	queries *sqlitedb.Queries,
	claim OutreachClaim,
	allowContact bool,
) (OutreachWork, bool, error) {
	candidate, err := queries.GetOutreachClaimCandidate(ctx, sqlitedb.GetOutreachClaimCandidateParams{
		ClaimedAt:    sql.NullInt64{Int64: claim.ClaimedAt.UnixMilli(), Valid: true},
		FailureLimit: unattendedAttemptLimit, AllowContact: boolInt64(allowContact),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return OutreachWork{}, false, nil
	}
	if err != nil {
		return OutreachWork{}, false, fmt.Errorf("query outreach work: %w", err)
	}
	row, err := queries.ClaimOutreachWork(ctx, sqlitedb.ClaimOutreachWorkParams{
		ClaimedAt:  sql.NullInt64{Int64: claim.ClaimedAt.UnixMilli(), Valid: true},
		Worker:     sql.NullString{String: claim.Worker, Valid: true},
		LeaseUntil: sql.NullInt64{Int64: claim.LeaseUntil.UnixMilli(), Valid: true},
		JobID:      candidate.ID, ExpectedStatus: candidate.OutreachStatus,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return OutreachWork{}, false, nil
	}
	if err != nil {
		return OutreachWork{}, false, fmt.Errorf("claim outreach for job %d: %w", candidate.ID, err)
	}
	item, err := outreachWorkFromRow(row, OutreachStatus(candidate.OutreachStatus))
	return item, true, err
}

func validateOutreachClaim(claim OutreachClaim) error {
	if normalizeText(claim.Worker) == "" {
		return fmt.Errorf("claim outreach: worker is required")
	}
	if claim.Limit <= 0 {
		return fmt.Errorf("claim outreach: limit must be positive")
	}
	if claim.ClaimedAt.IsZero() || !claim.LeaseUntil.After(claim.ClaimedAt) {
		return fmt.Errorf("claim outreach: a future lease deadline is required")
	}
	return nil
}

func outreachWorkFromRow(row sqlitedb.ClaimOutreachWorkRow, previous OutreachStatus) (OutreachWork, error) {
	var content judgmentContent
	if err := json.Unmarshal([]byte(row.JdJson), &content); err != nil {
		return OutreachWork{}, fmt.Errorf("decode outreach job %q JD: %w", row.PlatformJobID, err)
	}
	mode := OutreachModeContact
	if previous == OutreachStatusPossiblyContacted {
		mode = OutreachModeReconcile
	}
	return OutreachWork{
		JobID: row.ID, PlatformJobID: row.PlatformJobID, CanonicalURL: row.CanonicalUrl,
		JobTitle: row.JobTitle, CompanyName: row.CompanyName.String, City: row.CityText.String,
		Salary: row.SalaryText.String, Responsibilities: content.Responsibilities,
		Requirements: content.Requirements, JDHash: row.JdHash,
		GreetingText: row.OutreachGreetingText.String, AttemptNo: row.OutreachAttemptNo,
		Mode: mode, LeaseUntil: time.UnixMilli(row.LeaseUntil.Int64),
	}, nil
}

func (p *Pool) FinishOutreach(ctx context.Context, outcomes []OutreachOutcome) (BatchActionResult, error) {
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchActionResult{}, fmt.Errorf("finish outreach: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	result := BatchActionResult{}
	for _, outcome := range outcomes {
		if err := validateOutreachOutcome(outcome); err != nil {
			return BatchActionResult{}, err
		}
		_, err := queries.FinishOutreachWork(ctx, sqlitedb.FinishOutreachWorkParams{
			ResultStatus: string(outcome.Status), RetryAt: nullableTime(outcome.RetryAt),
			EvidenceJson:  sql.NullString{String: string(outcome.Evidence), Valid: true},
			ContactSource: nullableContactSource(outcome.ContactSource),
			CompletedAt:   outcome.CompletedAt.UnixMilli(), JobID: outcome.JobID, AttemptNo: outcome.AttemptNo,
		})
		if err == nil {
			result.Succeeded++
			continue
		}
		if errors.Is(err, sql.ErrNoRows) {
			result.Skipped = append(result.Skipped, SkippedAction{
				JobID: outcome.JobID, Code: "stale_outreach_attempt", Reason: "打招呼结果已过期，未写入岗位",
			})
			continue
		}
		return BatchActionResult{}, fmt.Errorf("finish outreach for job %d: %w", outcome.JobID, err)
	}
	if _, err := queries.StopOutreachRetriesAtLimit(ctx, unattendedAttemptLimit); err != nil {
		return BatchActionResult{}, fmt.Errorf("stop exhausted outreach retries: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return BatchActionResult{}, fmt.Errorf("finish outreach: commit transaction: %w", err)
	}
	return result, nil
}

func validateOutreachOutcome(outcome OutreachOutcome) error {
	if outcome.JobID <= 0 || outcome.AttemptNo <= 0 {
		return fmt.Errorf("finish outreach: job ID and attempt number must be positive")
	}
	if !validOutreachOutcomeStatus(outcome.Status) {
		return fmt.Errorf("finish outreach for job %d: invalid result status %q", outcome.JobID, outcome.Status)
	}
	if !json.Valid(outcome.Evidence) || outcome.CompletedAt.IsZero() {
		return fmt.Errorf("finish outreach for job %d: JSON evidence and completion time are required", outcome.JobID)
	}
	if err := validateContactSource(outcome); err != nil {
		return err
	}
	return validateRetryTime(
		outcome.Status == OutreachStatusFailed,
		outcome.RetryAt,
		outcome.CompletedAt,
		fmt.Sprintf("finish outreach for job %d", outcome.JobID),
	)
}

func validOutreachOutcomeStatus(status OutreachStatus) bool {
	return status == OutreachStatusContacted ||
		status == OutreachStatusPossiblyContacted ||
		status == OutreachStatusFailed
}

func validateContactSource(outcome OutreachOutcome) error {
	validSource := outcome.ContactSource == ContactSourceAgent || outcome.ContactSource == ContactSourceBossExisting
	if outcome.Status == OutreachStatusContacted && !validSource {
		return fmt.Errorf("finish outreach for job %d: contacted result requires a valid source", outcome.JobID)
	}
	if outcome.Status != OutreachStatusContacted && outcome.ContactSource != "" {
		return fmt.Errorf("finish outreach for job %d: only contacted result can have a contact source", outcome.JobID)
	}
	return nil
}

func validateRetryTime(allowed bool, retryAt *time.Time, completedAt time.Time, operation string) error {
	if retryAt == nil {
		return nil
	}
	if !allowed {
		return fmt.Errorf("%s: only a failed result can be retried", operation)
	}
	if !retryAt.After(completedAt) {
		return fmt.Errorf("%s: retry time must be after completion", operation)
	}
	return nil
}

func nullableContactSource(value ContactSource) sql.NullString {
	return sql.NullString{String: string(value), Valid: value != ""}
}

func (p *Pool) OutreachAvailability(ctx context.Context) ActionAvailability {
	if p.queries != nil {
		rows, err := p.queries.ListPlatformJobs(ctx)
		if err != nil {
			return ActionAvailability{Code: "outreach_state_unavailable", Reason: "暂时无法读取真实打招呼资格"}
		}
		for _, row := range rows {
			if outreachEligible(row) {
				return ActionAvailability{Allowed: true}
			}
		}
	}
	return ActionAvailability{Code: "outreach_unavailable", Reason: "当前没有可真实打招呼的岗位"}
}

// CountEligibleOutreach counts the current handoff candidates. A non-empty
// jobIDs list scopes the count to a user's selected batch; nil previews the
// whole global pool.
func (p *Pool) CountEligibleOutreach(ctx context.Context, jobIDs []int64) (int, error) {
	rows, err := p.queries.ListPlatformJobs(ctx)
	if err != nil {
		return 0, fmt.Errorf("count outreach candidates: %w", err)
	}
	selected := make(map[int64]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		selected[jobID] = struct{}{}
	}
	count := 0
	for _, row := range rows {
		if len(selected) > 0 {
			if _, ok := selected[row.ID]; !ok {
				continue
			}
		}
		if outreachEligible(row) {
			count++
		}
	}
	return count, nil
}

func (p *Pool) QueueAuthorizedOutreach(
	ctx context.Context,
	jobIDs []int64,
	authorization OutreachAuthorization,
) (BatchActionResult, error) {
	if len(jobIDs) == 0 {
		availability := p.OutreachAvailability(ctx)
		return BatchActionResult{}, &Rejection{Code: availability.Code, Reason: availability.Reason}
	}
	greeting, err := validateOutreachAuthorization(authorization)
	if err != nil {
		return BatchActionResult{}, err
	}
	transaction, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchActionResult{}, fmt.Errorf("queue authorized outreach: begin transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := p.queries.WithTx(transaction)
	result := BatchActionResult{}
	updatedAt := time.Now().UnixMilli()
	for _, jobID := range uniqueJobIDs(jobIDs) {
		succeeded, skipped, err := queueAuthorizedOutreach(ctx, queries, jobID, greeting, updatedAt)
		if err != nil {
			return BatchActionResult{}, err
		}
		if succeeded {
			result.Succeeded++
		} else {
			result.Skipped = append(result.Skipped, skipped)
		}
	}
	if err := transaction.Commit(); err != nil {
		return BatchActionResult{}, fmt.Errorf("queue authorized outreach: commit transaction: %w", err)
	}
	return result, nil
}

func queueAuthorizedOutreach(
	ctx context.Context,
	queries *sqlitedb.Queries,
	jobID int64,
	greeting string,
	updatedAt int64,
) (bool, SkippedAction, error) {
	_, err := queries.QueueAuthorizedOutreach(ctx, sqlitedb.QueueAuthorizedOutreachParams{
		GreetingText: sql.NullString{String: greeting, Valid: true}, UpdatedAt: updatedAt, JobID: jobID,
	})
	if err == nil {
		return true, SkippedAction{}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, SkippedAction{}, fmt.Errorf("queue authorized outreach for job %d: %w", jobID, err)
	}
	row, err := queries.GetPlatformJob(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, jobNotFoundRejection(jobID), nil
	}
	if err != nil {
		return false, SkippedAction{}, fmt.Errorf("recheck outreach job %d: %w", jobID, err)
	}
	return false, outreachQueueRejection(row), nil
}

func validateOutreachAuthorization(authorization OutreachAuthorization) (string, error) {
	greeting := normalizeText(authorization.GreetingText)
	if greeting == "" || normalizeText(authorization.TimeDescription) == "" {
		return "", fmt.Errorf("authorize outreach: greeting and time description are required")
	}
	return greeting, nil
}

func outreachQueueRejection(row sqlitedb.PlatformJob) SkippedAction {
	rejection := SkippedAction{JobID: row.ID, Code: "outreach_not_eligible", Reason: "当前岗位不符合真实打招呼条件"}
	if PlatformStatus(row.PlatformStatus) == PlatformStatusClosed {
		rejection.Code, rejection.Reason = "platform_job_closed", "岗位已关闭，不能真实打招呼"
		return rejection
	}
	switch OutreachStatus(row.OutreachStatus) {
	case OutreachStatusPending:
		rejection.Code, rejection.Reason = "outreach_already_queued", "岗位已在等待真实打招呼"
		return rejection
	case OutreachStatusProcessing:
		rejection.Code, rejection.Reason = "outreach_processing", "岗位正在打招呼"
		return rejection
	case OutreachStatusContacted:
		rejection.Code, rejection.Reason = "outreach_already_contacted", "岗位已打过招呼"
		return rejection
	case OutreachStatusPossiblyContacted:
		rejection.Code, rejection.Reason = "outreach_reconciliation_required", "岗位可能已打招呼，需先核对"
		return rejection
	case OutreachStatusFailed:
		rejection.Code, rejection.Reason = "outreach_retry_required", "岗位打招呼失败，请使用重试操作"
		return rejection
	}
	if row.HumanVerdict.Valid {
		if row.HumanReviewedJdHash.String != row.JdHash {
			rejection.Code, rejection.Reason = "human_review_stale", "JD 已变化，请先重新人工复核"
			return rejection
		}
		rejection.Code, rejection.Reason = "current_judgment_unsuitable", "人工结论为不适合，不能真实打招呼"
		return rejection
	}
	if AssessmentStatus(row.AssessmentStatus) != AssessmentStatusSuitable {
		rejection.Code, rejection.Reason = "suitable_judgment_required", "岗位尚无当前有效的适合结论"
	}
	return rejection
}
