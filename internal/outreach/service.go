package outreach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

const (
	outreachBatchSize     = 100
	outreachWorkerLease   = 10 * time.Minute
	outreachRetryDelay    = time.Minute
	outreachFinishTimeout = 10 * time.Second
)

// PlatformJobRef is the minimum trusted identity an external BOSS action
// needs. The URL comes from the persisted JobPool observation.
type PlatformJobRef struct {
	PlatformJobID string
	CanonicalURL  string
}

// ContactStatus is reliable only when Check returns without an error. Open
// and AlreadyContacted are independent because an open job can already have
// an existing conversation.
type ContactStatus struct {
	Open             bool
	AlreadyContacted bool
	Evidence         json.RawMessage
}

type FirstContactRequest struct {
	PlatformJobID string
	CanonicalURL  string
	GreetingText  string
}

type OutreachEffect string

const (
	OutreachEffectConfirmedSent     OutreachEffect = "confirmed_sent"
	OutreachEffectConfirmedNoEffect OutreachEffect = "confirmed_no_effect"
	OutreachEffectPossiblyEffective OutreachEffect = "possibly_effective"
)

type FirstContactResult struct {
	Effect   OutreachEffect
	Evidence json.RawMessage
}

type ErrorCategory string

const (
	ErrorTransient             ErrorCategory = "transient"
	ErrorAuthenticationExpired ErrorCategory = "authentication_expired"
	ErrorVerificationRequired  ErrorCategory = "verification_required"
	ErrorPlatformLimited       ErrorCategory = "platform_limited"
	ErrorInvalidResponse       ErrorCategory = "invalid_response"
	ErrorInvalidProtocol       ErrorCategory = "invalid_protocol"
	ErrorUnknown               ErrorCategory = "unknown"
)

// ActionError keeps BOSS-specific failure classification inside the outreach
// module. The scheduler maps it to the durable runlog category and retry rule.
type ActionError struct {
	Category ErrorCategory
	Err      error
}

func (e *ActionError) Error() string {
	if e == nil || e.Err == nil {
		return "outreach action failed"
	}
	return e.Err.Error()
}

func (e *ActionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// SendFirstContact and CheckContactStatus are the two external seams owned
// by PostService. A production adapter implements both with one authenticated
// BOSS browser session.
type SendFirstContact interface {
	Send(context.Context, FirstContactRequest) (FirstContactResult, error)
}

type CheckContactStatus interface {
	Check(context.Context, PlatformJobRef) (ContactStatus, error)
}

type Adapter interface {
	SendFirstContact
	CheckContactStatus
}

// Service owns the scheduling and recovery policy for real outreach. It does
// not own a business table; all platform-job transitions go through JobPool.
type Service struct {
	pool     *jobpool.Pool
	settings *automationsettings.Settings
	adapter  Adapter
	logs     *runlog.Log
	now      func() time.Time
}

func New(
	pool *jobpool.Pool,
	settings *automationsettings.Settings,
	adapter Adapter,
	logs *runlog.Log,
) *Service {
	return newService(pool, settings, adapter, logs, time.Now)
}

func newService(
	pool *jobpool.Pool,
	settings *automationsettings.Settings,
	adapter Adapter,
	logs *runlog.Log,
	now func() time.Time,
) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, settings: settings, adapter: adapter, logs: logs, now: now}
}

// Run performs an immediate scheduling cycle and then scans once per minute.
// This service consumes only work already authorized by Settings and persisted
// by JobPool; it never changes the automatic-outreach setting itself.
func (s *Service) Run(ctx context.Context) {
	runCycle := func() {
		if s.logs != nil && !s.logs.Health().Healthy {
			return
		}
		if err := s.runSchedulingCycle(ctx, s.now()); err != nil && s.logs != nil {
			_ = s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
				Flow: runlog.FlowOutreach, Stage: "scheduling_cycle", Err: err,
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

func (s *Service) runSchedulingCycle(ctx context.Context, now time.Time) error {
	if err := validateSchedulingCycle(s, now); err != nil {
		return err
	}
	settings, err := s.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("read automation settings for outreach: %w", err)
	}
	work, err := s.pool.ClaimOutreachWithinWindow(ctx, jobpool.OutreachClaim{
		Worker: "outreach-worker-1", Limit: outreachBatchSize,
		ClaimedAt: now, LeaseUntil: now.Add(outreachWorkerLease),
	}, settings.AllowsOutreachAt(now))
	if err != nil {
		return fmt.Errorf("claim outreach work: %w", err)
	}
	return s.processWorkBatch(ctx, work)
}

func validateSchedulingCycle(s *Service, now time.Time) error {
	if s.pool == nil || s.settings == nil || s.adapter == nil || s.logs == nil {
		return errors.New("outreach service is not fully assembled")
	}
	if now.IsZero() {
		return errors.New("outreach scheduling requires current time")
	}
	return nil
}

func (s *Service) processWorkBatch(ctx context.Context, work []jobpool.OutreachWork) error {
	var cycleErr error
	for _, item := range work {
		cycleErr = errors.Join(cycleErr, s.processWork(ctx, item))
	}
	return cycleErr
}

func (s *Service) processWork(ctx context.Context, work jobpool.OutreachWork) error {
	ref := PlatformJobRef{PlatformJobID: work.PlatformJobID, CanonicalURL: work.CanonicalURL}
	checkAttempt := runlog.Attempt{
		Flow: runlog.FlowOutreach, Operation: runlog.OperationCheckContactStatus,
		PlatformJobID: work.PlatformJobID, AttemptNo: work.AttemptNo,
	}
	trace, err := s.logs.Start(ctx, checkAttempt)
	if err != nil {
		return fmt.Errorf("start BOSS contact check for job %d: %w", work.JobID, err)
	}
	status, checkErr := s.adapter.Check(ctx, ref)
	if checkErr != nil {
		return s.finishCheckError(ctx, work, trace, checkErr)
	}
	if err := validateContactStatus(status); err != nil {
		return s.finishCheckError(ctx, work, trace, &ActionError{Category: ErrorInvalidProtocol, Err: err})
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		return fmt.Errorf("finish BOSS contact check for job %d: %w", work.JobID, err)
	}
	if status.AlreadyContacted {
		return s.finishBusiness(ctx, work, jobpool.OutreachStatusContacted,
			jobpool.ContactSourceBossExisting, status.Evidence, nil)
	}
	if !status.Open {
		return s.finishBusiness(ctx, work, jobpool.OutreachStatusFailed, "", status.Evidence, nil)
	}
	if work.Mode == jobpool.OutreachModeReconcile {
		return s.finishBusiness(ctx, work, jobpool.OutreachStatusFailed, "", status.Evidence, nil)
	}

	sendAttempt := runlog.Attempt{
		Flow: runlog.FlowOutreach, Operation: runlog.OperationSendFirstContact,
		PlatformJobID: work.PlatformJobID, AttemptNo: work.AttemptNo,
	}
	sendTrace, err := s.logs.StartLinked(ctx, trace.ID(), sendAttempt)
	if err != nil {
		return fmt.Errorf("start BOSS first contact for job %d: %w", work.JobID, err)
	}
	result, sendErr := s.adapter.Send(ctx, FirstContactRequest{
		PlatformJobID: work.PlatformJobID, CanonicalURL: work.CanonicalURL, GreetingText: work.GreetingText,
	})
	return s.finishSend(ctx, work, sendTrace, result, sendErr)
}

func validateContactStatus(status ContactStatus) error {
	if !json.Valid(status.Evidence) {
		return errors.New("BOSS contact check returned invalid evidence")
	}
	return nil
}

func (s *Service) finishCheckError(
	ctx context.Context,
	work jobpool.OutreachWork,
	trace runlog.Trace,
	callErr error,
) error {
	actionErr := asActionError(callErr)
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{
		Outcome: runlog.OutcomeFailed, ErrorCategory: runlogErrorCategory(actionErr.Category), Err: actionErr,
	}); err != nil {
		return fmt.Errorf("finish BOSS contact check for job %d: %w", work.JobID, err)
	}
	return s.finishBusiness(ctx, work, jobpool.OutreachStatusFailed, "",
		json.RawMessage(`{"code":"outreach_check_failed"}`), retryAtFor(actionErr.Category, s.now()))
}

func (s *Service) finishSend(
	ctx context.Context,
	work jobpool.OutreachWork,
	trace runlog.Trace,
	result FirstContactResult,
	sendErr error,
) error {
	if !validEffect(result.Effect) || !json.Valid(result.Evidence) {
		return s.finishInvalidSend(ctx, work, trace, result)
	}

	switch result.Effect {
	case OutreachEffectConfirmedSent:
		return s.finishConfirmedSent(ctx, work, trace, result)
	case OutreachEffectConfirmedNoEffect:
		return s.finishNoEffect(ctx, work, trace, result, sendErr)
	case OutreachEffectPossiblyEffective:
		return s.finishPossiblyEffective(ctx, work, trace, result, sendErr)
	default:
		return errors.New("unreachable outreach effect")
	}
}

func (s *Service) finishInvalidSend(ctx context.Context, work jobpool.OutreachWork, trace runlog.Trace, result FirstContactResult) error {
	protocolErr := &ActionError{Category: ErrorInvalidProtocol, Err: errors.New("BOSS first contact returned an invalid effect or evidence")}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{
		Outcome: runlog.OutcomeFailed, ErrorCategory: runlogErrorCategory(protocolErr.Category), Err: protocolErr,
		OutreachEffect: runlog.OutreachEffectPossiblyEffective,
	}); err != nil {
		return fmt.Errorf("finish invalid BOSS first contact for job %d: %w", work.JobID, err)
	}
	return s.finishBusiness(ctx, work, jobpool.OutreachStatusPossiblyContacted, "", evidenceOrFallback(result.Evidence, `{"code":"outreach_send_unreliable"}`), nil)
}

func (s *Service) finishConfirmedSent(ctx context.Context, work jobpool.OutreachWork, trace runlog.Trace, result FirstContactResult) error {
	// The effect is authoritative even if the adapter also returned a
	// diagnostic error: external success must never be retried.
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{
		Outcome: runlog.OutcomeSucceeded, OutreachEffect: runlog.OutreachEffectConfirmedSent,
	}); err != nil {
		return fmt.Errorf("finish confirmed BOSS first contact for job %d: %w", work.JobID, err)
	}
	return s.finishBusiness(ctx, work, jobpool.OutreachStatusContacted,
		jobpool.ContactSourceAgent, result.Evidence, nil)
}

func (s *Service) finishNoEffect(ctx context.Context, work jobpool.OutreachWork, trace runlog.Trace, result FirstContactResult, sendErr error) error {
	if sendErr == nil {
		if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeSucceeded, OutreachEffect: runlog.OutreachEffectConfirmedNoEffect,
		}); err != nil {
			return fmt.Errorf("finish no-effect BOSS first contact for job %d: %w", work.JobID, err)
		}
		return s.finishBusiness(ctx, work, jobpool.OutreachStatusFailed, "", result.Evidence, nil)
	}
	actionErr := asActionError(sendErr)
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{
		Outcome: runlog.OutcomeFailed, ErrorCategory: runlogErrorCategory(actionErr.Category), Err: actionErr,
		OutreachEffect: runlog.OutreachEffectConfirmedNoEffect,
	}); err != nil {
		return fmt.Errorf("finish failed BOSS first contact for job %d: %w", work.JobID, err)
	}
	return s.finishBusiness(ctx, work, jobpool.OutreachStatusFailed, "", result.Evidence,
		retryAtFor(actionErr.Category, s.now()))
}

func (s *Service) finishPossiblyEffective(ctx context.Context, work jobpool.OutreachWork, trace runlog.Trace, result FirstContactResult, sendErr error) error {
	actionErr := asActionError(sendErr)
	if sendErr == nil {
		actionErr = &ActionError{Category: ErrorUnknown, Err: errors.New("BOSS first contact effect could not be confirmed")}
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{
		Outcome: runlog.OutcomeFailed, ErrorCategory: runlogErrorCategory(actionErr.Category), Err: actionErr,
		OutreachEffect: runlog.OutreachEffectPossiblyEffective,
	}); err != nil {
		return fmt.Errorf("finish uncertain BOSS first contact for job %d: %w", work.JobID, err)
	}
	return s.finishBusiness(ctx, work, jobpool.OutreachStatusPossiblyContacted, "", result.Evidence, nil)
}

func (s *Service) finishBusiness(
	ctx context.Context,
	work jobpool.OutreachWork,
	status jobpool.OutreachStatus,
	source jobpool.ContactSource,
	evidence json.RawMessage,
	retryAt *time.Time,
) error {
	completedAt := s.now()
	finishContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), outreachFinishTimeout)
	defer cancel()
	if _, err := s.pool.FinishOutreach(finishContext, []jobpool.OutreachOutcome{{
		JobID: work.JobID, AttemptNo: work.AttemptNo, Status: status, ContactSource: source,
		Evidence: evidence, RetryAt: retryAt, CompletedAt: completedAt,
	}}); err != nil {
		return fmt.Errorf("persist outreach result for job %d: %w", work.JobID, err)
	}
	return nil
}

func asActionError(err error) *ActionError {
	var actionErr *ActionError
	if errors.As(err, &actionErr) && actionErr.Category != "" && actionErr.Err != nil {
		return actionErr
	}
	return &ActionError{Category: ErrorUnknown, Err: err}
}

func runlogErrorCategory(category ErrorCategory) runlog.ErrorCategory {
	switch category {
	case ErrorTransient:
		return runlog.ErrorCategoryTransient
	case ErrorAuthenticationExpired:
		return runlog.ErrorCategoryAuthenticationExpired
	case ErrorVerificationRequired:
		return runlog.ErrorCategoryVerificationRequired
	case ErrorPlatformLimited:
		return runlog.ErrorCategoryPlatformLimited
	case ErrorInvalidResponse:
		return runlog.ErrorCategoryInvalidResponse
	case ErrorInvalidProtocol:
		return runlog.ErrorCategoryInvalidProtocol
	default:
		return runlog.ErrorCategoryUnknown
	}
}

func retryAtFor(category ErrorCategory, completedAt time.Time) *time.Time {
	if category != ErrorTransient || completedAt.IsZero() {
		return nil
	}
	retryAt := completedAt.Add(outreachRetryDelay)
	return &retryAt
}

func validEffect(effect OutreachEffect) bool {
	return effect == OutreachEffectConfirmedSent ||
		effect == OutreachEffectConfirmedNoEffect ||
		effect == OutreachEffectPossiblyEffective
}

func evidenceOrFallback(evidence json.RawMessage, fallback string) json.RawMessage {
	if json.Valid(evidence) {
		return evidence
	}
	return json.RawMessage(fallback)
}
