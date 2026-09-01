package assessment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

type ConfirmationBatch struct {
	Results          []AssessmentConfirmation `json:"results"`
	TraceID          string                   `json:"-"`
	ExpectedAttempts []ConfirmationAttempt    `json:"-"`
}

type ConfirmationAttempt struct {
	JobID     int64
	AttemptNo int64
}

type AssessmentConfirmation struct {
	JobID         int64                    `json:"jobId"`
	AttemptNo     int64                    `json:"attemptNo"`
	Status        jobpool.AssessmentStatus `json:"status"`
	Reason        string                   `json:"reason"`
	Evidence      json.RawMessage          `json:"evidence"`
	protocolError error
}

// UnmarshalJSON keeps item-level protocol failures inside their item so one
// malformed result cannot hide valid peers from Confirm.
func (b *ConfirmationBatch) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Results []json.RawMessage `json:"results"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	b.Results = make([]AssessmentConfirmation, 0, len(envelope.Results))
	for _, raw := range envelope.Results {
		b.Results = append(b.Results, decodeAssessmentConfirmation(raw))
	}
	return nil
}

func decodeAssessmentConfirmation(raw json.RawMessage) AssessmentConfirmation {
	type wireConfirmation AssessmentConfirmation
	var wire wireConfirmation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&wire)
	if err == nil {
		err = requireJSONEOF(decoder)
	}
	confirmation := AssessmentConfirmation(wire)
	if err == nil {
		return confirmation
	}
	confirmation.JobID, confirmation.AttemptNo = confirmationIdentity(raw)
	confirmation.protocolError = err
	return confirmation
}

func confirmationIdentity(raw json.RawMessage) (int64, int64) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return 0, 0
	}
	var jobID int64
	var attemptNo int64
	_ = json.Unmarshal(fields["jobId"], &jobID)
	_ = json.Unmarshal(fields["attemptNo"], &attemptNo)
	return jobID, attemptNo
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

type ConfirmationItemStatus string

const (
	ConfirmationAccepted ConfirmationItemStatus = "accepted"
	ConfirmationInvalid  ConfirmationItemStatus = "invalid"
	ConfirmationStale    ConfirmationItemStatus = "stale"
)

type ConfirmationReceipt struct {
	Results []ConfirmationItemReceipt `json:"results"`
}

type ConfirmationItemReceipt struct {
	JobID     int64                  `json:"jobId"`
	AttemptNo int64                  `json:"attemptNo"`
	Status    ConfirmationItemStatus `json:"status"`
	Code      string                 `json:"code,omitempty"`
	Reason    string                 `json:"reason,omitempty"`
}

// Confirm is the only business entry point for Pi assessment conclusions.
func (s *Service) Confirm(ctx context.Context, batch ConfirmationBatch) (ConfirmationReceipt, error) {
	if len(batch.Results) == 0 {
		return ConfirmationReceipt{}, fmt.Errorf("confirm assessments: at least one result is required")
	}
	receipt := ConfirmationReceipt{Results: make([]ConfirmationItemReceipt, 0, len(batch.Results))}
	expected := make(map[ConfirmationAttempt]struct{}, len(batch.ExpectedAttempts))
	for _, attempt := range batch.ExpectedAttempts {
		expected[attempt] = struct{}{}
	}
	for _, confirmation := range batch.Results {
		item, err := s.confirmOne(ctx, batch.TraceID, expected, confirmation)
		if err != nil {
			return ConfirmationReceipt{}, err
		}
		receipt.Results = append(receipt.Results, item)
	}
	return receipt, nil
}

func (s *Service) confirmOne(
	ctx context.Context,
	traceID string,
	expected map[ConfirmationAttempt]struct{},
	confirmation AssessmentConfirmation,
) (ConfirmationItemReceipt, error) {
	receipt := ConfirmationItemReceipt{
		JobID: confirmation.JobID, AttemptNo: confirmation.AttemptNo,
	}
	validationErr := errors.Join(
		validateConfirmationScope(expected, confirmation),
		validateConfirmation(confirmation),
	)
	job, jobErr := s.pool.GetJob(ctx, confirmation.JobID)
	if jobErr != nil {
		receipt.Status = ConfirmationInvalid
		receipt.Code = "platform_job_not_found"
		receipt.Reason = "岗位不存在或已发生变化"
		return receipt, nil
	}
	if validationErr != nil {
		if err := s.recordInvalidConfirmation(ctx, traceID, job, confirmation, validationErr); err != nil {
			return ConfirmationItemReceipt{}, err
		}
		receipt.Status = ConfirmationInvalid
		receipt.Code = "invalid_confirmation"
		receipt.Reason = "鉴定结果格式无效，未写入岗位"
		return receipt, nil
	}

	attempt := runlog.Attempt{
		Flow: runlog.FlowAssessment, Operation: runlog.OperationConfirmAssessmentResults,
		PlatformJobID: job.PlatformJobID, AttemptNo: confirmation.AttemptNo,
	}
	trace, err := startConfirmationTrace(ctx, s.logs, traceID, attempt)
	if err != nil {
		return ConfirmationItemReceipt{}, fmt.Errorf("start assessment confirmation trace: %w", err)
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		return ConfirmationItemReceipt{}, fmt.Errorf("finish assessment confirmation trace: %w", err)
	}
	result, err := s.pool.FinishAssessments(ctx, []jobpool.AssessmentOutcome{{
		JobID: confirmation.JobID, AttemptNo: confirmation.AttemptNo,
		Status: confirmation.Status, Reason: confirmation.Reason, Evidence: confirmation.Evidence,
		CompletedAt: s.now(),
	}})
	if err != nil {
		cause := fmt.Errorf("persist confirmed assessment for job %d: %w", confirmation.JobID, err)
		logErr := s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
			Flow: runlog.FlowAssessment, Stage: "persist_confirmation", TraceID: trace.ID(),
			PlatformJobID: job.PlatformJobID, AttemptNo: confirmation.AttemptNo, Err: cause,
		})
		return ConfirmationItemReceipt{}, errors.Join(cause, logErr)
	}
	if result.Succeeded == 1 {
		receipt.Status = ConfirmationAccepted
		return receipt, nil
	}
	receipt.Status = ConfirmationStale
	receipt.Code = "stale_assessment_attempt"
	receipt.Reason = "AI 鉴定结果已过期，未写入岗位"
	return receipt, nil
}

func validateConfirmationScope(
	expected map[ConfirmationAttempt]struct{},
	confirmation AssessmentConfirmation,
) error {
	if len(expected) == 0 {
		return nil
	}
	key := ConfirmationAttempt{JobID: confirmation.JobID, AttemptNo: confirmation.AttemptNo}
	if _, ok := expected[key]; !ok {
		return fmt.Errorf("assessment confirmation does not belong to this request")
	}
	return nil
}

func validateConfirmation(confirmation AssessmentConfirmation) error {
	if confirmation.protocolError != nil {
		return fmt.Errorf("decode assessment confirmation: %w", confirmation.protocolError)
	}
	if confirmation.JobID <= 0 || confirmation.AttemptNo <= 0 {
		return fmt.Errorf("job ID and attempt number must be positive")
	}
	switch confirmation.Status {
	case jobpool.AssessmentStatusSuitable,
		jobpool.AssessmentStatusUnsuitable,
		jobpool.AssessmentStatusNeedsUserConfirmation:
	default:
		return fmt.Errorf("unsupported assessment suggestion %q", confirmation.Status)
	}
	if strings.TrimSpace(confirmation.Reason) == "" {
		return fmt.Errorf("assessment reason is required")
	}
	if !structuredEvidence(confirmation.Evidence) {
		return fmt.Errorf("structured JSON evidence is required")
	}
	return nil
}

func structuredEvidence(evidence json.RawMessage) bool {
	trimmed := bytes.TrimSpace(evidence)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return false
	}
	return trimmed[0] == '{' || trimmed[0] == '['
}

func (s *Service) recordInvalidConfirmation(
	ctx context.Context,
	traceID string,
	job jobpool.JobView,
	confirmation AssessmentConfirmation,
	cause error,
) error {
	if confirmation.AttemptNo <= 0 {
		return nil
	}
	attempt := runlog.Attempt{
		Flow: runlog.FlowAssessment, Operation: runlog.OperationConfirmAssessmentResults,
		PlatformJobID: job.PlatformJobID, AttemptNo: confirmation.AttemptNo,
	}
	trace, err := startConfirmationTrace(ctx, s.logs, traceID, attempt)
	if err != nil {
		return fmt.Errorf("start invalid assessment confirmation trace: %w", err)
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{
		Outcome: runlog.OutcomeFailed, ErrorCategory: runlog.ErrorCategoryInvalidProtocol, Err: cause,
	}); err != nil {
		return fmt.Errorf("finish invalid assessment confirmation trace: %w", err)
	}
	return nil
}

func startConfirmationTrace(
	ctx context.Context,
	logs *runlog.Log,
	traceID string,
	attempt runlog.Attempt,
) (runlog.Trace, error) {
	if traceID != "" {
		return logs.StartLinked(ctx, traceID, attempt)
	}
	return logs.Start(ctx, attempt)
}
