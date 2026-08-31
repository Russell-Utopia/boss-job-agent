package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

const (
	discoveryWorkerLease = 10 * time.Minute
	discoveryRetryDelay  = time.Minute
	maxAutomaticAttempts = 3
)

var errStaleWorker = errors.New("discovery worker is no longer current")

type workerAttempt struct {
	runID              int64
	attemptNo          int64
	owner              string
	failureCount       int64
	currentSearchRange SearchRange
	nextPage           int
}

func (s *Service) Run(ctx context.Context) {
	runCycle := func() {
		if !s.logs.Health().Healthy {
			return
		}
		_ = s.runSchedulingCycle(ctx, s.now())
	}
	runDiscoveryLoop(ctx, s.wake, runCycle)
}

func runDiscoveryLoop(ctx context.Context, wake <-chan struct{}, runCycle func()) {
	runCycle()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			runCycle()
		case <-ticker.C:
			runCycle()
		}
	}
}

func (s *Service) runSchedulingCycle(ctx context.Context, now time.Time) error {
	row, ready, err := s.schedulableRun(ctx, now)
	if err != nil || !ready {
		return err
	}
	worker := workerFromRow(row)
	ranges, err := searchRangesFromSavedResume(row.ResumeJson)
	if err != nil {
		return s.failWorker(ctx, worker, "", err, now)
	}
	rangeIndex, err := findSearchRange(ranges, row.CurrentRole.String, row.CurrentCity.String)
	if err != nil {
		return s.failWorker(ctx, worker, "", err, now)
	}
	worker.currentSearchRange = ranges[rangeIndex]
	err = s.completeRanges(ctx, worker, ranges, rangeIndex, now)
	if errors.Is(err, errStaleWorker) {
		return nil
	}
	return err
}

func (s *Service) schedulableRun(
	ctx context.Context,
	now time.Time,
) (sqlitedb.GetLatestDiscoveryRunRow, bool, error) {
	row, err := s.queries.GetLatestDiscoveryRun(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		cause := fmt.Errorf("query discovery work: %w", err)
		return row, false, s.recordTechnicalError(ctx, "query_work", 0, 0, cause)
	}
	if Status(row.Status) == StatusFailed {
		return s.claimedRetryRow(ctx, row, now)
	}
	if Status(row.Status) != StatusRunning {
		return row, false, nil
	}
	if row.WorkerOwner.String == s.workerOwner {
		return row, true, nil
	}
	if !row.WorkerLeaseUntil.Valid || row.WorkerLeaseUntil.Int64 > now.UnixMilli() {
		return row, false, nil
	}
	return row, false, s.expireWorker(ctx, row, now)
}

func (s *Service) claimedRetryRow(
	ctx context.Context,
	row sqlitedb.GetLatestDiscoveryRunRow,
	now time.Time,
) (sqlitedb.GetLatestDiscoveryRunRow, bool, error) {
	claimed, err := s.claimDueRetry(ctx, row, now)
	if err != nil || !claimed {
		return row, false, err
	}
	claimedRunID := row.ID
	claimedAttemptNo := row.AttemptNo + 1
	row, err = s.queries.GetLatestDiscoveryRun(ctx)
	if err == nil {
		return row, true, nil
	}
	cause := fmt.Errorf("query claimed discovery work: %w", err)
	err = s.recordTechnicalError(ctx, "query_claimed_work", claimedRunID, claimedAttemptNo, cause)
	return row, false, err
}

func workerFromRow(row sqlitedb.GetLatestDiscoveryRunRow) workerAttempt {
	return workerAttempt{
		runID:        row.ID,
		attemptNo:    row.AttemptNo,
		owner:        row.WorkerOwner.String,
		failureCount: row.ConsecutiveFailureCount,
		nextPage:     int(row.NextPage.Int64),
	}
}

func (s *Service) claimDueRetry(
	ctx context.Context,
	row sqlitedb.GetLatestDiscoveryRunRow,
	now time.Time,
) (bool, error) {
	if !row.RetryAt.Valid || row.RetryAt.Int64 > now.UnixMilli() {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.queries.ClaimDueDiscoveryRetry(ctx, sqlitedb.ClaimDueDiscoveryRetryParams{
		WorkerOwner:      nullString(s.workerOwner),
		WorkerLeaseUntil: nullInt64(now.Add(discoveryWorkerLease).UnixMilli()),
		UpdatedAt:        now.UnixMilli(),
		RunID:            row.ID,
		Now:              nullInt64(now.UnixMilli()),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		cause := fmt.Errorf("claim retry for discovery run %d: %w", row.ID, err)
		return false, s.recordTechnicalError(ctx, "claim_retry", row.ID, row.AttemptNo, cause)
	}
	return true, nil
}

func (s *Service) expireWorker(
	ctx context.Context,
	row sqlitedb.GetLatestDiscoveryRunRow,
	now time.Time,
) error {
	s.mu.Lock()
	_, err := s.queries.ExpireDiscoveryWorker(ctx, sqlitedb.ExpireDiscoveryWorkerParams{
		UpdatedAt:          now.UnixMilli(),
		RunID:              row.ID,
		CurrentWorkerOwner: nullString(s.workerOwner),
		Now:                nullInt64(now.UnixMilli()),
	})
	s.mu.Unlock()
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		cause := fmt.Errorf("expire discovery worker for run %d: %w", row.ID, err)
		return s.recordTechnicalError(ctx, "expire_worker", row.ID, row.AttemptNo, cause)
	}
	cause := fmt.Errorf("discovery worker %q lease expired", row.WorkerOwner.String)
	return s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
		Flow:           runlog.FlowDiscovery,
		Stage:          "worker_lease_expired",
		DiscoveryRunID: row.ID,
		AttemptNo:      row.AttemptNo,
		Err:            cause,
	})
}

func (s *Service) completeRanges(
	ctx context.Context,
	worker workerAttempt,
	ranges []SearchRange,
	rangeIndex int,
	now time.Time,
) error {
	for ; rangeIndex < len(ranges); rangeIndex++ {
		worker.currentSearchRange = ranges[rangeIndex]
		if err := s.completeSearchRange(ctx, &worker, ranges, rangeIndex, now); err != nil {
			return err
		}
		if rangeIndex == len(ranges)-1 {
			return nil
		}
		worker.nextPage = 1
	}
	return nil
}

func (s *Service) completeSearchRange(
	ctx context.Context,
	worker *workerAttempt,
	ranges []SearchRange,
	rangeIndex int,
	now time.Time,
) error {
	for {
		page, traceID, err := s.fetchReliablePage(ctx, *worker, worker.currentSearchRange, worker.nextPage)
		if err != nil {
			return s.handleWorkerError(ctx, *worker, traceID, err, now)
		}
		lastRange := rangeIndex == len(ranges)-1
		err = s.commitPage(ctx, *worker, page, nextRange(ranges, rangeIndex), lastRange, now)
		if err != nil {
			return s.handleWorkerError(ctx, *worker, traceID, err, now)
		}
		worker.failureCount = 0
		if !page.HasMore {
			return nil
		}
		worker.nextPage++
	}
}

func (s *Service) handleWorkerError(
	ctx context.Context,
	worker workerAttempt,
	traceID string,
	err error,
	now time.Time,
) error {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errStaleWorker) {
		return err
	}
	return s.failWorker(ctx, worker, traceID, err, now)
}

func nextRange(ranges []SearchRange, current int) *SearchRange {
	if current+1 >= len(ranges) {
		return nil
	}
	return &ranges[current+1]
}

func (s *Service) fetchReliablePage(
	ctx context.Context,
	worker workerAttempt,
	searchRange SearchRange,
	pageNo int,
) (DiscoveryPage, string, error) {
	attempt := runlog.Attempt{
		Flow:           runlog.FlowDiscovery,
		Operation:      runlog.OperationFetchPage,
		DiscoveryRunID: worker.runID,
		AttemptNo:      worker.attemptNo,
		SearchRole:     searchRange.Role,
		SearchCity:     searchRange.City,
		PageNo:         pageNo,
	}
	trace, err := s.logs.Start(ctx, attempt)
	if err != nil {
		return DiscoveryPage{}, "", fmt.Errorf("start discovery page trace: %w", err)
	}
	page, fetchErr := s.discovery.FetchPage(ctx, searchRange, pageNo)
	if fetchErr != nil {
		category := fetchErrorCategory(fetchErr)
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: category, Err: fetchErr,
		})
		return DiscoveryPage{}, trace.ID(), errors.Join(fmt.Errorf("fetch discovery page %d: %w", pageNo, fetchErr), finishErr)
	}
	if validationErr := ValidatePage(page); validationErr != nil {
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: runlog.ErrorCategoryInvalidResponse, Err: validationErr,
		})
		return DiscoveryPage{}, trace.ID(), errors.Join(validationErr, finishErr)
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		return DiscoveryPage{}, trace.ID(), fmt.Errorf("finish discovery page trace: %w", err)
	}
	return page, trace.ID(), nil
}

func (s *Service) commitPage(
	ctx context.Context,
	worker workerAttempt,
	page DiscoveryPage,
	next *SearchRange,
	lastRange bool,
	now time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.queries.IsCurrentDiscoveryWorker(ctx, sqlitedb.IsCurrentDiscoveryWorkerParams{
		RunID:       worker.runID,
		AttemptNo:   worker.attemptNo,
		WorkerOwner: nullString(worker.owner),
		CurrentRole: nullString(worker.currentSearchRange.Role),
		CurrentCity: nullString(worker.currentSearchRange.City),
		NextPage:    nullInt64(int64(worker.nextPage)),
	})
	if err != nil {
		return fmt.Errorf("check discovery worker %d: %w", worker.runID, err)
	}
	if !current {
		return errStaleWorker
	}

	for _, observation := range page.Observations {
		if _, err := s.pool.Observe(ctx, worker.runID, toPoolObservation(observation, now)); err != nil {
			return err
		}
	}

	if page.HasMore {
		return s.advancePage(ctx, worker, now)
	}
	if lastRange {
		return s.completeRun(ctx, worker, now)
	}
	return s.switchRange(ctx, worker, *next, now)
}

func (s *Service) advancePage(ctx context.Context, worker workerAttempt, at time.Time) error {
	now := at.UnixMilli()
	_, err := s.queries.AdvanceDiscoveryPage(ctx, sqlitedb.AdvanceDiscoveryPageParams{
		NextPage:         nullInt64(int64(worker.nextPage + 1)),
		ProgressAt:       nullInt64(now),
		WorkerLeaseUntil: nullInt64(at.Add(discoveryWorkerLease).UnixMilli()),
		UpdatedAt:        now,
		RunID:            worker.runID,
		AttemptNo:        worker.attemptNo,
		WorkerOwner:      nullString(worker.owner),
		CurrentRole:      nullString(worker.currentSearchRange.Role),
		CurrentCity:      nullString(worker.currentSearchRange.City),
		CurrentPage:      nullInt64(int64(worker.nextPage)),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return errStaleWorker
	}
	if err != nil {
		return fmt.Errorf("advance discovery run %d from page %d: %w", worker.runID, worker.nextPage, err)
	}
	return nil
}

func (s *Service) switchRange(ctx context.Context, worker workerAttempt, next SearchRange, at time.Time) error {
	now := at.UnixMilli()
	_, err := s.queries.SwitchDiscoveryRange(ctx, sqlitedb.SwitchDiscoveryRangeParams{
		NextRole:         nullString(next.Role),
		NextCity:         nullString(next.City),
		ProgressAt:       nullInt64(now),
		WorkerLeaseUntil: nullInt64(at.Add(discoveryWorkerLease).UnixMilli()),
		UpdatedAt:        now,
		RunID:            worker.runID,
		AttemptNo:        worker.attemptNo,
		WorkerOwner:      nullString(worker.owner),
		CurrentRole:      nullString(worker.currentSearchRange.Role),
		CurrentCity:      nullString(worker.currentSearchRange.City),
		CurrentPage:      nullInt64(int64(worker.nextPage)),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return errStaleWorker
	}
	if err != nil {
		return fmt.Errorf("switch discovery run %d to %s in %s: %w", worker.runID, next.Role, next.City, err)
	}
	return nil
}

func (s *Service) completeRun(ctx context.Context, worker workerAttempt, at time.Time) error {
	now := at.UnixMilli()
	_, err := s.queries.CompleteDiscoveryRun(ctx, sqlitedb.CompleteDiscoveryRunParams{
		ProgressAt:  nullInt64(now),
		FinishedAt:  nullInt64(now),
		UpdatedAt:   now,
		RunID:       worker.runID,
		AttemptNo:   worker.attemptNo,
		WorkerOwner: nullString(worker.owner),
		CurrentRole: nullString(worker.currentSearchRange.Role),
		CurrentCity: nullString(worker.currentSearchRange.City),
		CurrentPage: nullInt64(int64(worker.nextPage)),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return errStaleWorker
	}
	if err != nil {
		return fmt.Errorf("complete discovery run %d: %w", worker.runID, err)
	}
	return nil
}

func (s *Service) failWorker(
	ctx context.Context,
	worker workerAttempt,
	traceID string,
	cause error,
	at time.Time,
) error {
	s.mu.Lock()
	retryAt := sql.NullInt64{}
	if fetchErrorCategory(cause) == runlog.ErrorCategoryTransient && worker.failureCount+1 < maxAutomaticAttempts {
		retryAt = nullInt64(at.Add(discoveryRetryDelay).UnixMilli())
	}
	_, err := s.queries.FailDiscoveryRun(ctx, sqlitedb.FailDiscoveryRunParams{
		RetryAt:     retryAt,
		UpdatedAt:   at.UnixMilli(),
		RunID:       worker.runID,
		AttemptNo:   worker.attemptNo,
		WorkerOwner: nullString(worker.owner),
	})
	s.mu.Unlock()
	if errors.Is(err, sql.ErrNoRows) {
		return errStaleWorker
	}
	if err != nil {
		stateErr := fmt.Errorf("mark discovery run %d failed: %w", worker.runID, err)
		logErr := s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
			Flow:           runlog.FlowDiscovery,
			Stage:          "mark_worker_failed",
			TraceID:        traceID,
			DiscoveryRunID: worker.runID,
			AttemptNo:      worker.attemptNo,
			Err:            stateErr,
		})
		return errors.Join(cause, stateErr, logErr)
	}
	logErr := s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
		Flow:           runlog.FlowDiscovery,
		Stage:          "worker_attempt_failed",
		TraceID:        traceID,
		DiscoveryRunID: worker.runID,
		AttemptNo:      worker.attemptNo,
		Err:            cause,
	})
	return errors.Join(cause, logErr)
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

func nullInt64(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: true}
}

// ValidatePage enforces the complete-page contract implemented by production
// and controlled JobDiscovery adapters.
