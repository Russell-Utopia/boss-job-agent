package onlineresume

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

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

// Versions owns the saved BOSS online resume versions.
type Versions struct {
	db      *sql.DB
	queries *sqlitedb.Queries
	reader  OnlineResume
	logs    *runlog.Log
	now     func() time.Time
}

// OnlineResume is the one-shot BOSS read owned by this module. Implementations
// return the complete resume or an error; partial resumes are never valid.
type OnlineResume interface {
	Read(context.Context) (ResumeContent, error)
}

type ReadErrorCategory string

const (
	ReadErrorTransient             ReadErrorCategory = "transient"
	ReadErrorAuthenticationExpired ReadErrorCategory = "authentication_expired"
	ReadErrorVerificationRequired  ReadErrorCategory = "verification_required"
	ReadErrorPlatformLimited       ReadErrorCategory = "platform_limited"
	ReadErrorInvalidResponse       ReadErrorCategory = "invalid_response"
	ReadErrorInvalidProtocol       ReadErrorCategory = "invalid_protocol"
	ReadErrorUnknown               ReadErrorCategory = "unknown"
)

// ReadError lets the module make a stable user-facing and runlog decision
// without depending on WebBridge or browser-specific error types.
type ReadError struct {
	Category   ReadErrorCategory
	UserReason string
	Cause      error
}

func (e *ReadError) Error() string {
	return e.UserReason
}

func (e *ReadError) Unwrap() error {
	return e.Cause
}

// ResumeContent is the complete business input saved for discovery and
// assessment. Its shape intentionally has no contact-information fields.
type ResumeContent struct {
	JobIntentions      []JobIntention `json:"jobIntentions"`
	WorkExperiences    []string       `json:"workExperiences"`
	ProjectExperiences []string       `json:"projectExperiences"`
	Educations         []string       `json:"educations"`
	Skills             []string       `json:"skills"`
}

// JobIntention is one search scope source from the BOSS online resume.
type JobIntention struct {
	Role           string `json:"role"`
	City           string `json:"city"`
	Salary         string `json:"salary"`
	EmploymentType string `json:"employmentType"`
}

// Version is one immutable saved online resume version.
type Version struct {
	ID        int64         `json:"-"`
	Version   int           `json:"version"`
	CreatedAt time.Time     `json:"createdAt"`
	Content   ResumeContent `json:"-"`
}

type RefreshStatus string

const (
	RefreshCreated   RefreshStatus = "created"
	RefreshUnchanged RefreshStatus = "unchanged"
)

type RefreshResult struct {
	Status  RefreshStatus `json:"status"`
	Current Version       `json:"current"`
}

// Rejection is safe to render to the user while preserving the diagnostic
// cause for runlog and internal error handling.
type Rejection struct {
	Code   string
	Reason string
	cause  error
}

func (r *Rejection) Error() string {
	return r.Reason
}

func (r *Rejection) Unwrap() error {
	return r.cause
}

func (r *Rejection) RejectionCode() string {
	return r.Code
}

func (r *Rejection) RejectionReason() string {
	return r.Reason
}

func New(db *sql.DB, reader OnlineResume, logs *runlog.Log, now func() time.Time) *Versions {
	return &Versions{db: db, queries: sqlitedb.New(db), reader: reader, logs: logs, now: now}
}

func (v *Versions) GetCurrent(ctx context.Context) (*Version, error) {
	row, err := v.queries.GetCurrentOnlineResume(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query current online resume: %w", err)
	}
	current, err := versionFromRow(row.ID, row.VersionNo, row.ResumeJson, row.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &current, nil
}

// Get returns an immutable saved resume version by its internal reference.
// Policy validation uses this to keep the resume used for a page-session draft
// stable even if the user refreshes the online resume before validating it.
func (v *Versions) Get(ctx context.Context, id int64) (*Version, error) {
	if id <= 0 {
		return nil, fmt.Errorf("query online resume version: ID must be positive")
	}
	row, err := v.queries.GetOnlineResumeVersion(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query online resume version %d: %w", id, err)
	}
	version, err := versionFromRow(row.ID, row.VersionNo, row.ResumeJson, row.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (v *Versions) RefreshFromBoss(ctx context.Context) (RefreshResult, error) {
	attempt := runlog.Attempt{
		Flow:      runlog.FlowOnlineResume,
		Operation: runlog.OperationReadOnlineResume,
		AttemptNo: 1,
	}
	trace, err := v.logs.Start(ctx, attempt)
	if err != nil {
		if errors.Is(err, runlog.ErrUnavailable) {
			return RefreshResult{}, &Rejection{
				Code:   "runlog_unavailable",
				Reason: "运行日志不可用，恢复前不会访问 BOSS",
				cause:  err,
			}
		}
		return RefreshResult{}, fmt.Errorf("start online resume read: %w", err)
	}
	content, readErr := v.reader.Read(ctx)
	if readErr != nil {
		errorCategory, userReason := describeReadError(readErr)
		finishErr := v.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome:       runlog.OutcomeFailed,
			ErrorCategory: errorCategory,
			Err:           readErr,
		})
		return RefreshResult{}, &Rejection{
			Code:   "online_resume_read_failed",
			Reason: userReason,
			cause:  errors.Join(fmt.Errorf("read BOSS online resume: %w", readErr), finishErr),
		}
	}
	encoded, hash, contentErr := encodeContent(content)
	if contentErr != nil {
		finishErr := v.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome:       runlog.OutcomeFailed,
			ErrorCategory: runlog.ErrorCategoryInvalidResponse,
			Err:           contentErr,
		})
		return RefreshResult{}, &Rejection{
			Code:   "online_resume_incomplete",
			Reason: "BOSS 在线简历读取不完整，已保留上一次可靠版本",
			cause:  errors.Join(contentErr, finishErr),
		}
	}
	if err := v.logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		if errors.Is(err, runlog.ErrUnavailable) {
			return RefreshResult{}, &Rejection{
				Code:   "runlog_unavailable",
				Reason: "运行日志写入失败，未保存本次在线简历",
				cause:  err,
			}
		}
		return RefreshResult{}, fmt.Errorf("finish online resume read log: %w", err)
	}
	return v.save(ctx, encoded, hash)
}

func describeReadError(err error) (runlog.ErrorCategory, string) {
	category := runlog.ErrorCategoryUnknown
	reason := "读取 BOSS 在线简历失败，已保留上一次可靠版本"
	var readErr *ReadError
	if !errors.As(err, &readErr) {
		return category, reason
	}
	if readErr.UserReason != "" {
		reason = readErr.UserReason
	}
	switch readErr.Category {
	case ReadErrorTransient:
		category = runlog.ErrorCategoryTransient
	case ReadErrorAuthenticationExpired:
		category = runlog.ErrorCategoryAuthenticationExpired
	case ReadErrorVerificationRequired:
		category = runlog.ErrorCategoryVerificationRequired
	case ReadErrorPlatformLimited:
		category = runlog.ErrorCategoryPlatformLimited
	case ReadErrorInvalidResponse:
		category = runlog.ErrorCategoryInvalidResponse
	case ReadErrorInvalidProtocol:
		category = runlog.ErrorCategoryInvalidProtocol
	}
	return category, reason
}

func (v *Versions) save(ctx context.Context, encoded, hash string) (_ RefreshResult, retErr error) {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("begin online resume refresh: %w", err)
	}
	defer rollbackFailedRefresh(tx, &retErr)
	queries := v.queries.WithTx(tx)
	current, err := loadCurrentForRefresh(ctx, queries)
	if err != nil {
		return RefreshResult{}, err
	}
	if current != nil && current.ResumeHash == hash {
		return commitUnchangedRefresh(tx, *current)
	}
	return v.createCurrentVersion(ctx, tx, queries, encoded, hash)
}

func rollbackFailedRefresh(tx *sql.Tx, retErr *error) {
	if *retErr != nil {
		*retErr = errors.Join(*retErr, tx.Rollback())
	}
}

func loadCurrentForRefresh(
	ctx context.Context,
	queries *sqlitedb.Queries,
) (*sqlitedb.GetCurrentOnlineResumeRow, error) {
	current, err := queries.GetCurrentOnlineResume(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query current online resume while refreshing: %w", err)
	}
	return &current, nil
}

func commitUnchangedRefresh(tx *sql.Tx, current sqlitedb.GetCurrentOnlineResumeRow) (RefreshResult, error) {
	version, err := versionFromRow(current.ID, current.VersionNo, current.ResumeJson, current.CreatedAt)
	if err != nil {
		return RefreshResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RefreshResult{}, fmt.Errorf("commit unchanged online resume refresh: %w", err)
	}
	return RefreshResult{Status: RefreshUnchanged, Current: version}, nil
}

func (v *Versions) createCurrentVersion(
	ctx context.Context,
	tx *sql.Tx,
	queries *sqlitedb.Queries,
	encoded string,
	hash string,
) (RefreshResult, error) {
	nextVersion, err := queries.GetNextOnlineResumeVersionNumber(ctx)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("query next online resume version: %w", err)
	}
	if err := queries.ClearCurrentOnlineResume(ctx); err != nil {
		return RefreshResult{}, fmt.Errorf("clear current online resume: %w", err)
	}
	created, err := queries.CreateCurrentOnlineResume(ctx, sqlitedb.CreateCurrentOnlineResumeParams{
		VersionNo:  nextVersion,
		ResumeJson: encoded,
		ResumeHash: hash,
		CreatedAt:  v.now().UnixMilli(),
	})
	if err != nil {
		return RefreshResult{}, fmt.Errorf("create online resume version: %w", err)
	}
	version, err := versionFromRow(created.ID, created.VersionNo, created.ResumeJson, created.CreatedAt)
	if err != nil {
		return RefreshResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RefreshResult{}, fmt.Errorf("commit online resume refresh: %w", err)
	}
	return RefreshResult{Status: RefreshCreated, Current: version}, nil
}

func encodeContent(content ResumeContent) (string, string, error) {
	content = normalizeContent(content)
	if err := ValidateContent(content); err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", "", fmt.Errorf("encode online resume content: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return string(encoded), hex.EncodeToString(digest[:]), nil
}

// ValidateContent owns the complete online-resume contract shared by the
// production adapter and the versioning boundary.
func ValidateContent(content ResumeContent) error {
	if err := validateJobIntentions(content.JobIntentions); err != nil {
		return err
	}
	sections := []struct {
		name   string
		values []string
	}{
		{name: "work experiences", values: content.WorkExperiences},
		{name: "project experiences", values: content.ProjectExperiences},
		{name: "educations", values: content.Educations},
		{name: "skills", values: content.Skills},
	}
	for _, section := range sections {
		if err := validateTextSection(section.name, section.values); err != nil {
			return err
		}
	}
	return nil
}

func validateJobIntentions(intentions []JobIntention) error {
	if len(intentions) == 0 {
		return errors.New("online resume has no complete job intention")
	}
	for index, intention := range intentions {
		if intention.Role == "" || intention.City == "" || intention.Salary == "" || intention.EmploymentType == "" {
			return fmt.Errorf("online resume job intention %d is incomplete", index+1)
		}
	}
	return nil
}

func validateTextSection(name string, values []string) error {
	if values == nil {
		return fmt.Errorf("online resume %s section was not read", name)
	}
	for index, value := range values {
		if value == "" {
			return fmt.Errorf("online resume %s item %d is empty", name, index+1)
		}
	}
	return nil
}

func normalizeContent(content ResumeContent) ResumeContent {
	for index := range content.JobIntentions {
		content.JobIntentions[index].Role = normalizeText(content.JobIntentions[index].Role)
		content.JobIntentions[index].City = normalizeText(content.JobIntentions[index].City)
		content.JobIntentions[index].Salary = normalizeText(content.JobIntentions[index].Salary)
		content.JobIntentions[index].EmploymentType = normalizeText(content.JobIntentions[index].EmploymentType)
	}
	normalizeTexts(content.WorkExperiences)
	normalizeTexts(content.ProjectExperiences)
	normalizeTexts(content.Educations)
	normalizeTexts(content.Skills)
	return content
}

func normalizeTexts(values []string) {
	for index := range values {
		values[index] = normalizeText(values[index])
	}
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

func versionFromRow(id, versionNo int64, resumeJSON string, createdAt int64) (Version, error) {
	var content ResumeContent
	if err := json.Unmarshal([]byte(resumeJSON), &content); err != nil {
		return Version{}, fmt.Errorf("decode saved online resume version %d: %w", versionNo, err)
	}
	return Version{
		ID:        id,
		Version:   int(versionNo),
		CreatedAt: time.UnixMilli(createdAt),
		Content:   content,
	}, nil
}
