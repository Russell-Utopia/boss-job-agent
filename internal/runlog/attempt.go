package runlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"time"
)

type Flow string

const (
	FlowOnlineResume Flow = "online_resume"
	FlowDiscovery    Flow = "discovery"
	FlowAssessment   Flow = "assessment"
	FlowOutreach     Flow = "outreach"
)

type Operation string

const (
	OperationReadOnlineResume         Operation = "read_online_resume"
	OperationListPage                 Operation = "list_page"
	OperationReadJob                  Operation = "read_job"
	OperationSubmitAssessment         Operation = "submit_assessment"
	OperationConfirmAssessmentResults Operation = "confirm_assessment_results"
	OperationCheckContactStatus       Operation = "check_contact_status"
	OperationSendFirstContact         Operation = "send_first_contact"
)

type ErrorCategory string

const (
	ErrorCategoryTransient             ErrorCategory = "transient"
	ErrorCategoryAuthenticationExpired ErrorCategory = "authentication_expired"
	ErrorCategoryVerificationRequired  ErrorCategory = "verification_required"
	ErrorCategoryPlatformLimited       ErrorCategory = "platform_limited"
	ErrorCategoryInvalidResponse       ErrorCategory = "invalid_response"
	ErrorCategoryInvalidProtocol       ErrorCategory = "invalid_protocol"
	ErrorCategoryUnknown               ErrorCategory = "unknown"
)

type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

type OutreachEffect string

const (
	OutreachEffectConfirmedSent     OutreachEffect = "confirmed_sent"
	OutreachEffectConfirmedNoEffect OutreachEffect = "confirmed_no_effect"
	OutreachEffectPossiblyEffective OutreachEffect = "possibly_effective"
)

// Attempt contains the stable business keys for exactly one external call or
// one item in a batched Pi call.
type Attempt struct {
	Flow             Flow
	Operation        Operation
	DiscoveryRunID   int64
	PlatformJobID    string
	AttemptNo        int64
	SearchRole       string
	SearchCity       string
	PageNo           int
	JobOrdinal       int
	JobIDFingerprint string
}

// Trace is returned only after the start record has persisted successfully.
// Callers may propagate ID to an external adapter and must pass the Trace back
// unchanged when finishing the attempt.
type Trace struct {
	id       string
	attempts []Attempt
}

func (t Trace) ID() string {
	return t.id
}

type AttemptResult struct {
	Outcome         Outcome
	ErrorCategory   ErrorCategory
	ExternalFailure *ExternalFailureEvidence
	Err             error
	ErrorRedactions []string
	OutreachEffect  OutreachEffect
}

// ExternalFailureEvidence is the non-sensitive location of the first failed
// upstream request within one business-level external attempt.
type ExternalFailureEvidence struct {
	RequestOrdinal int
	Stage          string
	DetailOrdinal  int
	UpstreamCode   string
}

var (
	externalFailureStagePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)
	upstreamCodePattern         = regexp.MustCompile(`^-?\d{1,10}$`)
	jobIDFingerprintPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Start is the fail-closed seam for a new BOSS or Pi attempt. It returns a
// Trace only after the durable start record succeeds.
func (l *Log) Start(ctx context.Context, attempt Attempt) (Trace, error) {
	return l.start(ctx, []Attempt{attempt})
}

// StartBatch persists one start record per platform job before a batched Pi
// request. Every record receives the same request-level trace ID while keeping
// the item's own stable business key.
func (l *Log) StartBatch(ctx context.Context, attempts []Attempt) (Trace, error) {
	if len(attempts) < 2 {
		return Trace{}, fmt.Errorf("batch requires at least two attempts")
	}
	return l.start(ctx, attempts)
}

// StartLinked starts a related external attempt under an existing trusted
// business trace ID, while retaining its own operation and terminal record.
func (l *Log) StartLinked(ctx context.Context, traceID string, attempt Attempt) (Trace, error) {
	if !validTechnicalTraceID(traceID) {
		return Trace{}, fmt.Errorf("linked trace ID is invalid")
	}
	return l.startWithTraceID(ctx, traceID, []Attempt{attempt})
}

func (l *Log) start(ctx context.Context, attempts []Attempt) (Trace, error) {
	traceID, err := newTraceID()
	if err != nil {
		return Trace{}, fmt.Errorf("generate trace ID: %w", err)
	}
	return l.startWithTraceID(ctx, traceID, attempts)
}

func (l *Log) startWithTraceID(ctx context.Context, traceID string, attempts []Attempt) (Trace, error) {
	if err := validateAttempts(attempts); err != nil {
		return Trace{}, err
	}
	trace := Trace{id: traceID, attempts: append([]Attempt(nil), attempts...)}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.health.Healthy {
		return Trace{}, fmt.Errorf("%w: %s", ErrUnavailable, l.health.Message)
	}
	for index, attempt := range attempts {
		record := slog.NewRecord(l.now().UTC(), slog.LevelInfo, "external attempt started", 0)
		record.AddAttrs(attemptAttrs(trace.id, attempt, "external_attempt_started", index, len(attempts))...)
		if err := l.handleLocked(ctx, record); err != nil {
			return Trace{}, err
		}
	}
	return trace, nil
}

// Finish records the terminal result for a non-batched external call.
// Callers must persist this terminal evidence before committing terminal
// business state. A write failure keeps the claimed item recoverable and closes
// the gate for new attempts.
func (l *Log) Finish(ctx context.Context, trace Trace, result AttemptResult) error {
	if len(trace.attempts) != 1 {
		return fmt.Errorf("Finish requires a single-item trace; use FinishItem for a batch")
	}
	return l.FinishItem(ctx, trace, trace.attempts[0], result)
}

// FinishItem records one self-contained terminal result from a batched Pi
// request. A terminal record that cannot be written is retained for replay
// before a later recheck may reopen the new-attempt gate.
func (l *Log) FinishItem(ctx context.Context, trace Trace, attempt Attempt, result AttemptResult) error {
	itemIndex, err := validateTerminal(trace, attempt, result)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	record := terminalRecord(l.now().UTC(), trace, attempt, itemIndex, result)
	if !l.health.Healthy {
		l.queueTerminalRecordLocked(record)
		return fmt.Errorf("%w: %s", ErrUnavailable, l.health.Message)
	}
	if err := l.handleLocked(ctx, record); err != nil {
		l.queueTerminalRecordLocked(record)
		return err
	}
	return nil
}

func validateTerminal(trace Trace, attempt Attempt, result AttemptResult) (int, error) {
	if trace.id == "" || len(trace.attempts) == 0 {
		return -1, fmt.Errorf("trace is empty")
	}
	itemIndex := traceItemIndex(trace, attempt)
	if itemIndex < 0 {
		return -1, fmt.Errorf("attempt does not belong to trace")
	}
	if err := validateResult(attempt, result); err != nil {
		return -1, err
	}
	return itemIndex, nil
}

func terminalRecord(at time.Time, trace Trace, attempt Attempt, itemIndex int, result AttemptResult) slog.Record {
	level := slog.LevelInfo
	message := "external attempt succeeded"
	if result.Outcome == OutcomeFailed {
		level = slog.LevelError
		message = "external attempt failed"
	}
	record := slog.NewRecord(at, level, message, 0)
	attrs := attemptAttrs(trace.id, attempt, "external_attempt_finished", itemIndex, len(trace.attempts))
	attrs = append(attrs, slog.String("outcome", string(result.Outcome)))
	if result.ErrorCategory != "" {
		attrs = append(attrs, slog.String("error_category", string(result.ErrorCategory)))
	}
	if result.OutreachEffect != "" {
		attrs = append(attrs, slog.String("outreach_effect", string(result.OutreachEffect)))
	}
	if result.ExternalFailure != nil {
		attrs = append(attrs,
			slog.Int("request_ordinal", result.ExternalFailure.RequestOrdinal),
			slog.String("stage", result.ExternalFailure.Stage),
		)
		if result.ExternalFailure.DetailOrdinal > 0 {
			attrs = append(attrs, slog.Int("detail_ordinal", result.ExternalFailure.DetailOrdinal))
		}
		if result.ExternalFailure.UpstreamCode != "" {
			attrs = append(attrs, slog.String("upstream_code", result.ExternalFailure.UpstreamCode))
		}
	}
	if result.Err != nil {
		chain, truncated := snapshotErrorTree(result.Err, result.ErrorRedactions...)
		attrs = append(attrs, slog.Any("error_chain", chain))
		if truncated {
			attrs = append(attrs, slog.Bool("error_chain_truncated", true))
		}
	}
	record.AddAttrs(attrs...)
	return record
}

func traceItemIndex(trace Trace, attempt Attempt) int {
	for index, candidate := range trace.attempts {
		if candidate == attempt {
			return index
		}
	}
	return -1
}

func attemptAttrs(traceID string, attempt Attempt, event string, itemIndex, batchSize int) []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("schema_version", 1),
		slog.String("event", event),
		slog.String("trace_id", traceID),
		slog.String("flow", string(attempt.Flow)),
		slog.String("operation", string(attempt.Operation)),
		slog.Int64("attempt_no", attempt.AttemptNo),
	}
	if batchSize > 1 {
		attrs = append(attrs,
			slog.Int("batch_size", batchSize),
			slog.Int("batch_item_index", itemIndex),
		)
	}
	if attempt.DiscoveryRunID != 0 {
		attrs = append(attrs, slog.Int64("discovery_run_id", attempt.DiscoveryRunID))
	}
	if attempt.PlatformJobID != "" {
		attrs = append(attrs, slog.String("platform_job_id", attempt.PlatformJobID))
	}
	if attempt.SearchRole != "" {
		attrs = append(attrs, slog.String("search_role", attempt.SearchRole))
	}
	if attempt.SearchCity != "" {
		attrs = append(attrs, slog.String("search_city", attempt.SearchCity))
	}
	if attempt.PageNo != 0 {
		attrs = append(attrs, slog.Int("page_no", attempt.PageNo))
	}
	if attempt.JobOrdinal != 0 {
		attrs = append(attrs, slog.Int("job_ordinal", attempt.JobOrdinal))
	}
	if attempt.JobIDFingerprint != "" {
		attrs = append(attrs, slog.String("job_id_fingerprint", attempt.JobIDFingerprint))
	}
	return attrs
}

func validateAttempts(attempts []Attempt) error {
	if len(attempts) == 0 || len(attempts) > 5 {
		return fmt.Errorf("external request requires between one and five attempts")
	}
	for index, attempt := range attempts {
		if err := validateAttempt(attempt); err != nil {
			return fmt.Errorf("validate attempt %d: %w", index, err)
		}
	}
	return validateBatch(attempts)
}

func validateBatch(attempts []Attempt) error {
	if len(attempts) == 1 {
		return nil
	}
	if attempts[0].Flow != FlowAssessment {
		return fmt.Errorf("only assessment Pi calls may contain multiple platform jobs")
	}
	seen := make(map[Attempt]struct{}, len(attempts))
	for _, attempt := range attempts {
		if attempt.Flow != attempts[0].Flow || attempt.Operation != attempts[0].Operation {
			return fmt.Errorf("batched attempts require one flow and operation")
		}
		if _, duplicate := seen[attempt]; duplicate {
			return fmt.Errorf("batched attempts require distinct business keys")
		}
		seen[attempt] = struct{}{}
	}
	return nil
}

func validateAttempt(attempt Attempt) error {
	if attempt.Flow == "" || attempt.Operation == "" || attempt.AttemptNo <= 0 {
		return fmt.Errorf("external attempt requires flow, operation, and positive attempt number")
	}
	switch attempt.Flow {
	case FlowOnlineResume:
		return validateOnlineResumeAttempt(attempt)
	case FlowDiscovery:
		return validateDiscoveryAttempt(attempt)
	case FlowAssessment:
		return validateAssessmentAttempt(attempt)
	case FlowOutreach:
		return validateOutreachAttempt(attempt)
	default:
		return fmt.Errorf("unsupported external flow %q", attempt.Flow)
	}
}

func validateOnlineResumeAttempt(attempt Attempt) error {
	if attempt.Operation != OperationReadOnlineResume {
		return fmt.Errorf("unsupported online resume operation %q", attempt.Operation)
	}
	if attempt.AttemptNo != 1 {
		return fmt.Errorf("online resume read is a one-shot command and requires attempt number 1")
	}
	return nil
}

func validateDiscoveryAttempt(attempt Attempt) error {
	if attempt.DiscoveryRunID <= 0 {
		return fmt.Errorf("discovery attempt requires discovery run ID")
	}
	switch attempt.Operation {
	case OperationListPage:
		return validateListPageAttempt(attempt)
	case OperationReadJob:
		return validateReadJobAttempt(attempt)
	default:
		return fmt.Errorf("unsupported discovery operation %q", attempt.Operation)
	}
}

func validateListPageAttempt(attempt Attempt) error {
	if attempt.PageNo <= 0 || attempt.SearchRole != "" || attempt.SearchCity != "" ||
		attempt.PlatformJobID != "" || attempt.JobOrdinal != 0 || attempt.JobIDFingerprint != "" {
		return fmt.Errorf("list_page requires only a positive page number")
	}
	return nil
}

func validateReadJobAttempt(attempt Attempt) error {
	if attempt.PageNo <= 0 || attempt.SearchRole != "" || attempt.SearchCity != "" ||
		attempt.PlatformJobID != "" || attempt.JobOrdinal <= 0 ||
		!jobIDFingerprintPattern.MatchString(attempt.JobIDFingerprint) {
		return fmt.Errorf("read_job requires page, positive job ordinal, and stable ID fingerprint")
	}
	return nil
}

func validateAssessmentAttempt(attempt Attempt) error {
	if attempt.PlatformJobID == "" {
		return fmt.Errorf("assessment attempt requires platform job ID")
	}
	if attempt.Operation != OperationSubmitAssessment && attempt.Operation != OperationConfirmAssessmentResults {
		return fmt.Errorf("unsupported assessment operation %q", attempt.Operation)
	}
	return nil
}

func validateOutreachAttempt(attempt Attempt) error {
	if attempt.PlatformJobID == "" {
		return fmt.Errorf("outreach attempt requires platform job ID")
	}
	if attempt.Operation != OperationCheckContactStatus && attempt.Operation != OperationSendFirstContact {
		return fmt.Errorf("unsupported outreach operation %q", attempt.Operation)
	}
	return nil
}

func validateResult(attempt Attempt, result AttemptResult) error {
	if err := validateOutcome(result); err != nil {
		return err
	}
	if result.ErrorCategory != "" && !validErrorCategory(result.ErrorCategory) {
		return fmt.Errorf("unsupported error category %q", result.ErrorCategory)
	}
	if err := validateExternalFailure(attempt, result); err != nil {
		return err
	}
	return validateOutreachEffect(attempt.Operation, result.OutreachEffect)
}

func validateExternalFailure(attempt Attempt, result AttemptResult) error {
	if result.ExternalFailure == nil {
		return nil
	}
	if result.Outcome != OutcomeFailed ||
		(attempt.Operation != OperationListPage &&
			attempt.Operation != OperationReadJob) {
		return fmt.Errorf("external failure evidence is only valid for failed discovery reads")
	}
	evidence := result.ExternalFailure
	if evidence.RequestOrdinal < 0 || evidence.DetailOrdinal < 0 ||
		!externalFailureStagePattern.MatchString(evidence.Stage) {
		return fmt.Errorf("external failure evidence requires non-negative ordinals and a stable stage")
	}
	if evidence.UpstreamCode != "" && !upstreamCodePattern.MatchString(evidence.UpstreamCode) {
		return fmt.Errorf("external failure evidence upstream code must be a short integer")
	}
	return nil
}

func validateOutcome(result AttemptResult) error {
	switch result.Outcome {
	case OutcomeSucceeded:
		if result.Err != nil || result.ErrorCategory != "" {
			return fmt.Errorf("successful result cannot include an error")
		}
	case OutcomeFailed:
		if result.Err == nil || result.ErrorCategory == "" {
			return fmt.Errorf("failed result requires error category and error tree")
		}
	default:
		return fmt.Errorf("unsupported external attempt outcome %q", result.Outcome)
	}
	return nil
}

func validateOutreachEffect(operation Operation, effect OutreachEffect) error {
	if operation == OperationSendFirstContact && !validOutreachEffect(effect) {
		return fmt.Errorf("send_first_contact result requires a valid outreach effect")
	}
	if operation != OperationSendFirstContact && effect != "" {
		return fmt.Errorf("outreach effect is only valid for send_first_contact")
	}
	return nil
}

func validErrorCategory(category ErrorCategory) bool {
	switch category {
	case ErrorCategoryTransient,
		ErrorCategoryAuthenticationExpired,
		ErrorCategoryVerificationRequired,
		ErrorCategoryPlatformLimited,
		ErrorCategoryInvalidResponse,
		ErrorCategoryInvalidProtocol,
		ErrorCategoryUnknown:
		return true
	default:
		return false
	}
}

func validOutreachEffect(effect OutreachEffect) bool {
	switch effect {
	case OutreachEffectConfirmedSent, OutreachEffectConfirmedNoEffect, OutreachEffectPossiblyEffective:
		return true
	default:
		return false
	}
}

func newTraceID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	if bytes == [16]byte{} {
		return "", fmt.Errorf("generated all-zero trace ID")
	}
	return hex.EncodeToString(bytes[:]), nil
}
