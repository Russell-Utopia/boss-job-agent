package assessment

import (
	"context"
	"fmt"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

const (
	assessmentEvaluatorVersion int64 = 1
	assessmentBatchLimit             = 5
	assessmentLeaseDuration          = 10 * time.Minute
)

// AssessmentSubmitter is the Pi seam owned by the assessment module. Submit
// sends one complete request; AI conclusions return only through Service.Confirm.
type AssessmentSubmitter interface {
	Submit(context.Context, AssessmentRequest) error
	Close(context.Context) error
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
	if !s.logs.Health().Healthy {
		return nil
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
		Limit:            assessmentBatchLimit,
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
		return fmt.Errorf("submit assessment request: %w", err)
	}
	for _, attempt := range attempts {
		if err := s.logs.FinishItem(ctx, trace, attempt, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
			return fmt.Errorf("finish assessment submission trace: %w", err)
		}
	}
	return nil
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
