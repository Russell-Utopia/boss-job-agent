package assessment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

const (
	assessmentEvaluatorVersion  int64 = 1
	automaticAdmissionBatchSize       = 100
	assessmentLeaseDuration           = 10 * time.Minute
	assessmentRetryDelay              = time.Minute
	assessmentFinishTimeout           = 10 * time.Second
)

// AssessmentSubmitter is the Pi seam owned by the assessment module. Submit
// sends one complete request; AI conclusions return only through Service.Confirm.
type AssessmentSubmitter interface {
	Submit(context.Context, AssessmentRequest) error
	Close(context.Context) error
}

type SubmissionErrorCategory string

const (
	SubmissionErrorTransient       SubmissionErrorCategory = "transient"
	SubmissionErrorInvalidProtocol SubmissionErrorCategory = "invalid_protocol"
)

// SubmissionError lets the assessment module decide whether a failed Pi
// request belongs to the bounded automatic retry cycle.
type SubmissionError struct {
	Category SubmissionErrorCategory
	Err      error
}

func (e *SubmissionError) Error() string {
	return e.Err.Error()
}

func (e *SubmissionError) Unwrap() error {
	return e.Err
}

type AssessmentRequest struct {
	TraceID       string                     `json:"traceId"`
	ResumeVersion int                        `json:"resumeVersion"`
	Resume        onlineresume.ResumeContent `json:"resume"`
	Policy        Policy                     `json:"policy"`
	Jobs          []AssessmentJobInput       `json:"jobs"`
}

type AssessmentJobInput struct {
	JobID            int64  `json:"jobId"`
	AttemptNo        int64  `json:"attemptNo"`
	PlatformJobID    string `json:"platformJobId"`
	CanonicalURL     string `json:"canonicalUrl"`
	JobTitle         string `json:"jobTitle"`
	CompanyName      string `json:"companyName"`
	City             string `json:"city"`
	Salary           string `json:"salary"`
	Responsibilities string `json:"responsibilities"`
	Requirements     string `json:"requirements"`
	JDHash           string `json:"jdHash"`
}

func (s *Service) runSchedulingCycle(ctx context.Context, now time.Time) error {
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	if !s.logs.Health().Healthy {
		return nil
	}
	settings, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("read automation settings for assessment: %w", err)
	}
	if settings.AutomaticAssessmentEnabled {
		if err := s.admitAutomaticAssessments(ctx); err != nil {
			return err
		}
	}
	currentResume, err := s.resumeVersions.GetCurrent(ctx)
	if err != nil {
		return fmt.Errorf("read current online resume for assessment: %w", err)
	}
	if currentResume == nil {
		return nil
	}
	policy, err := s.GetActivePolicy(ctx)
	if err != nil {
		return err
	}
	work, err := s.pool.ClaimAssessments(ctx, jobpool.AssessmentClaim{
		Worker:           "assessment-worker-1",
		ResumeVersionID:  currentResume.ID,
		PolicyVersionID:  policy.ID,
		EvaluatorVersion: assessmentEvaluatorVersion,
		ProcessingLimit:  settings.AssessmentProcessingLimit,
		ClaimedAt:        now,
		LeaseUntil:       now.Add(assessmentLeaseDuration),
	})
	if err != nil {
		return err
	}
	if len(work) == 0 {
		return nil
	}
	return s.submitAssessmentWork(ctx, *currentResume, policy, work)
}

func (s *Service) admitAutomaticAssessments(ctx context.Context) error {
	for {
		admitted, err := s.pool.AdmitAssessments(ctx, automaticAdmissionBatchSize)
		if err != nil {
			return err
		}
		if admitted < automaticAdmissionBatchSize {
			return nil
		}
	}
}

// Run owns the single v1 assessment scheduling loop and therefore never has
// more than one Pi submission in flight for this local instance.
func (s *Service) Run(ctx context.Context) {
	if s.submitter != nil {
		defer func() { _ = s.submitter.Close(context.Background()) }()
	}
	runCycle := func() {
		if err := s.runSchedulingCycle(ctx, s.now()); err != nil {
			_ = s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
				Flow: runlog.FlowAssessment, Stage: "scheduling_cycle", Err: err,
			})
		}
	}
	runCycle()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCycle()
		}
	}
}

func (s *Service) submitAssessmentWork(
	ctx context.Context,
	currentResume onlineresume.Version,
	policy Policy,
	work []jobpool.AssessmentWork,
) error {
	attempts := assessmentAttempts(work, runlog.OperationSubmitAssessment)
	trace, err := startAssessmentTrace(ctx, s.logs, attempts)
	if err != nil {
		return fmt.Errorf("start assessment submission trace: %w", err)
	}
	request := AssessmentRequest{
		TraceID: trace.ID(), ResumeVersion: currentResume.Version,
		Resume: currentResume.Content, Policy: policy, Jobs: assessmentInputs(work),
	}
	if err := s.submitter.Submit(ctx, request); err != nil {
		return s.failAssessmentSubmission(ctx, trace, attempts, work, err)
	}
	for _, attempt := range attempts {
		if err := s.logs.FinishItem(ctx, trace, attempt, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
			return fmt.Errorf("finish assessment submission trace: %w", err)
		}
	}
	return nil
}

func (s *Service) failAssessmentSubmission(
	ctx context.Context,
	trace runlog.Trace,
	attempts []runlog.Attempt,
	work []jobpool.AssessmentWork,
	submitErr error,
) error {
	finishContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), assessmentFinishTimeout)
	defer cancel()
	errorCategory, shouldRetry := submissionFailurePolicy(submitErr)
	var terminalErr error
	for _, attempt := range attempts {
		terminalErr = errors.Join(terminalErr, s.logs.FinishItem(finishContext, trace, attempt, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: errorCategory, Err: submitErr,
		}))
	}
	if terminalErr != nil {
		return errors.Join(fmt.Errorf("record failed assessment submission: %w", submitErr), terminalErr)
	}

	failedAt := s.now()
	var retryAt *time.Time
	if shouldRetry {
		value := failedAt.Add(assessmentRetryDelay)
		retryAt = &value
	}
	outcomes := make([]jobpool.AssessmentOutcome, 0, len(work))
	for _, item := range work {
		reason := "Pi 鉴定请求失败，请手工重试"
		if shouldRetry {
			reason = "Pi 鉴定请求暂时失败，等待自动重试"
		}
		outcomes = append(outcomes, jobpool.AssessmentOutcome{
			JobID: item.JobID, AttemptNo: item.AttemptNo, Status: jobpool.AssessmentStatusFailed,
			Reason:   reason,
			Evidence: json.RawMessage(`{"code":"assessment_submit_failed"}`),
			RetryAt:  retryAt, CompletedAt: failedAt,
		})
	}
	if _, err := s.pool.FinishAssessments(finishContext, outcomes); err != nil {
		cause := fmt.Errorf("persist failed assessment submission: %w", err)
		logErr := s.logs.RecordTechnicalError(finishContext, runlog.TechnicalError{
			Flow: runlog.FlowAssessment, Stage: "persist_submission_failure", TraceID: trace.ID(), Err: cause,
		})
		return errors.Join(cause, logErr)
	}
	return nil
}

func submissionFailurePolicy(err error) (runlog.ErrorCategory, bool) {
	var submissionError *SubmissionError
	if !errors.As(err, &submissionError) {
		return runlog.ErrorCategoryUnknown, false
	}
	switch submissionError.Category {
	case SubmissionErrorTransient:
		return runlog.ErrorCategoryTransient, true
	case SubmissionErrorInvalidProtocol:
		return runlog.ErrorCategoryInvalidProtocol, false
	default:
		return runlog.ErrorCategoryUnknown, false
	}
}

func startAssessmentTrace(
	ctx context.Context,
	logs *runlog.Log,
	attempts []runlog.Attempt,
) (runlog.Trace, error) {
	if len(attempts) == 1 {
		return logs.Start(ctx, attempts[0])
	}
	return logs.StartBatch(ctx, attempts)
}

func assessmentAttempts(work []jobpool.AssessmentWork, operation runlog.Operation) []runlog.Attempt {
	attempts := make([]runlog.Attempt, 0, len(work))
	for _, item := range work {
		attempts = append(attempts, runlog.Attempt{
			Flow: runlog.FlowAssessment, Operation: operation,
			PlatformJobID: item.PlatformJobID, AttemptNo: item.AttemptNo,
		})
	}
	return attempts
}

func assessmentInputs(work []jobpool.AssessmentWork) []AssessmentJobInput {
	inputs := make([]AssessmentJobInput, 0, len(work))
	for _, item := range work {
		inputs = append(inputs, AssessmentJobInput{
			JobID: item.JobID, AttemptNo: item.AttemptNo, PlatformJobID: item.PlatformJobID,
			CanonicalURL: item.CanonicalURL, JobTitle: item.JobTitle, CompanyName: item.CompanyName,
			City: item.City, Salary: item.Salary, Responsibilities: item.Responsibilities,
			Requirements: item.Requirements, JDHash: item.JDHash,
		})
	}
	return inputs
}
