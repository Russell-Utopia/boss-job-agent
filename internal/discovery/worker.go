package discovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	pageCheckpoint     *pageCheckpoint
}

type pageCheckpoint struct {
	jobIDs         []string
	hasMore        bool
	nextJobOrdinal int
	encodedJobIDs  string
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
	checkpoint, err := pageCheckpointFromRow(row)
	if err != nil {
		return s.failWorker(ctx, worker, "", err, now)
	}
	worker.pageCheckpoint = checkpoint
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

func pageCheckpointFromRow(row sqlitedb.GetLatestDiscoveryRunRow) (*pageCheckpoint, error) {
	validFields := 0
	if row.CurrentPageJobIdsJson.Valid {
		validFields++
	}
	if row.CurrentPageHasMore.Valid {
		validFields++
	}
	if row.NextJobOrdinal.Valid {
		validFields++
	}
	if validFields == 0 {
		return nil, nil
	}
	if validFields != 3 {
		return nil, invalidResponseError("saved discovery page checkpoint is incomplete")
	}
	var jobIDs []string
	if err := json.Unmarshal([]byte(row.CurrentPageJobIdsJson.String), &jobIDs); err != nil {
		return nil, invalidResponseError("decode saved discovery page checkpoint: %v", err)
	}
	page := JobPage{PlatformJobIDs: jobIDs, HasMore: row.CurrentPageHasMore.Int64 == 1}
	if err := validateJobPage(page); err != nil {
		return nil, invalidResponseError("saved discovery page checkpoint: %v", err)
	}
	ordinal := int(row.NextJobOrdinal.Int64)
	if ordinal < 0 || ordinal > len(jobIDs) {
		return nil, invalidResponseError("saved discovery job ordinal %d is outside page size %d", ordinal, len(jobIDs))
	}
	return &pageCheckpoint{
		jobIDs:         jobIDs,
		hasMore:        page.HasMore,
		nextJobOrdinal: ordinal,
		encodedJobIDs:  row.CurrentPageJobIdsJson.String,
	}, nil
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
		page, traceID, err := s.listReliablePage(ctx, *worker)
		now = s.freshWorkerTime(now)
		if err != nil {
			return s.handleWorkerError(ctx, *worker, traceID, err, now)
		}
		if worker.pageCheckpoint == nil {
			checkpoint, err := s.freezePage(ctx, *worker, page, now)
			if err != nil {
				return s.handleWorkerError(ctx, *worker, traceID, err, now)
			}
			worker.pageCheckpoint = checkpoint
			worker.failureCount = 0
		}
		for worker.pageCheckpoint.nextJobOrdinal < len(worker.pageCheckpoint.jobIDs) {
			jobOrdinal := worker.pageCheckpoint.nextJobOrdinal
			platformJobID := worker.pageCheckpoint.jobIDs[jobOrdinal]
			observation, jobTraceID, err := s.readReliableJob(ctx, *worker, platformJobID, jobOrdinal)
			now = s.freshWorkerTime(now)
			if err != nil {
				return s.handleWorkerError(ctx, *worker, jobTraceID, err, now, platformJobID)
			}
			if err := s.commitJob(ctx, *worker, observation, jobOrdinal, now); err != nil {
				return s.handleWorkerError(
					ctx, *worker, jobTraceID, err, now, platformJobID, observation.PlatformJobID,
				)
			}
			worker.pageCheckpoint.nextJobOrdinal++
			worker.failureCount = 0
		}
		lastRange := rangeIndex == len(ranges)-1
		now = s.freshWorkerTime(now)
		err = s.finishPage(ctx, *worker, nextRange(ranges, rangeIndex), lastRange, now)
		if err != nil {
			return s.handleWorkerError(ctx, *worker, traceID, err, now)
		}
		worker.failureCount = 0
		worker.pageCheckpoint = nil
		if !page.HasMore {
			return nil
		}
		worker.nextPage++
	}
}

func (s *Service) freshWorkerTime(previous time.Time) time.Time {
	current := s.now()
	if current.After(previous) {
		return current
	}
	return previous
}

func (s *Service) handleWorkerError(
	ctx context.Context,
	worker workerAttempt,
	traceID string,
	err error,
	now time.Time,
	errorRedactions ...string,
) error {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errStaleWorker) {
		return err
	}
	return s.failWorker(ctx, worker, traceID, err, now, errorRedactions...)
}

func nextRange(ranges []SearchRange, current int) *SearchRange {
	if current+1 >= len(ranges) {
		return nil
	}
	return &ranges[current+1]
}

func (s *Service) listReliablePage(
	ctx context.Context,
	worker workerAttempt,
) (JobPage, string, error) {
	attempt := runlog.Attempt{
		Flow:           runlog.FlowDiscovery,
		Operation:      runlog.OperationListPage,
		DiscoveryRunID: worker.runID,
		AttemptNo:      worker.attemptNo,
		PageNo:         worker.nextPage,
	}
	trace, err := s.logs.Start(ctx, attempt)
	if err != nil {
		return JobPage{}, "", fmt.Errorf("start discovery page list trace: %w", err)
	}
	page, fetchErr := s.discovery.ListPage(ctx, worker.currentSearchRange, worker.nextPage)
	if fetchErr != nil {
		category := fetchErrorCategory(fetchErr)
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome:         runlog.OutcomeFailed,
			ErrorCategory:   category,
			ExternalFailure: runlogFailureEvidence(fetchErr),
			Err:             fetchErr,
		})
		return JobPage{}, trace.ID(), errors.Join(
			fmt.Errorf("list discovery page %d: %w", worker.nextPage, fetchErr),
			finishErr,
		)
	}
	if validationErr := validateJobPage(page); validationErr != nil {
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: runlog.ErrorCategoryInvalidResponse, Err: validationErr,
		})
		return JobPage{}, trace.ID(), errors.Join(validationErr, finishErr)
	}
	if worker.pageCheckpoint != nil &&
		(!slices.Equal(page.PlatformJobIDs, worker.pageCheckpoint.jobIDs) ||
			page.HasMore != worker.pageCheckpoint.hasMore) {
		validationErr := invalidResponseError(
			"discovery page %d changed after its checkpoint was frozen",
			worker.nextPage,
		)
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: runlog.ErrorCategoryInvalidResponse, Err: validationErr,
		})
		return JobPage{}, trace.ID(), errors.Join(validationErr, finishErr)
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		return JobPage{}, trace.ID(), fmt.Errorf("finish discovery page list trace: %w", err)
	}
	return page, trace.ID(), nil
}

func validateJobPage(page JobPage) error {
	seen := make(map[string]struct{}, len(page.PlatformJobIDs))
	for index, platformJobID := range page.PlatformJobIDs {
		if strings.TrimSpace(platformJobID) == "" {
			return invalidResponseError("discovery page job %d has no stable ID", index+1)
		}
		if _, duplicate := seen[platformJobID]; duplicate {
			return invalidResponseError("discovery page repeats stable ID at job %d", index+1)
		}
		seen[platformJobID] = struct{}{}
	}
	return nil
}

func invalidResponseError(format string, values ...any) error {
	return &FetchError{
		Category: FetchErrorInvalidResponse,
		Cause:    fmt.Errorf(format, values...),
	}
}

func (s *Service) freezePage(
	ctx context.Context,
	worker workerAttempt,
	page JobPage,
	now time.Time,
) (*pageCheckpoint, error) {
	encoded, err := json.Marshal(page.PlatformJobIDs)
	if err != nil {
		return nil, invalidResponseError("encode discovery page checkpoint: %v", err)
	}
	checkpoint := &pageCheckpoint{
		jobIDs:        append([]string(nil), page.PlatformJobIDs...),
		hasMore:       page.HasMore,
		encodedJobIDs: string(encoded),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.queries.FreezeDiscoveryPage(ctx, sqlitedb.FreezeDiscoveryPageParams{
		JobIdsJson:       nullString(checkpoint.encodedJobIDs),
		HasMore:          nullInt64(boolInt64(checkpoint.hasMore)),
		ProgressAt:       nullInt64(now.UnixMilli()),
		WorkerLeaseUntil: nullInt64(now.Add(discoveryWorkerLease).UnixMilli()),
		UpdatedAt:        now.UnixMilli(),
		RunID:            worker.runID,
		AttemptNo:        worker.attemptNo,
		WorkerOwner:      nullString(worker.owner),
		CurrentRole:      nullString(worker.currentSearchRange.Role),
		CurrentCity:      nullString(worker.currentSearchRange.City),
		NextPage:         nullInt64(int64(worker.nextPage)),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errStaleWorker
	}
	if err != nil {
		return nil, fmt.Errorf("freeze discovery run %d page %d: %w", worker.runID, worker.nextPage, err)
	}
	return checkpoint, nil
}

func (s *Service) readReliableJob(
	ctx context.Context,
	worker workerAttempt,
	platformJobID string,
	jobOrdinal int,
) (JobObservation, string, error) {
	attempt := runlog.Attempt{
		Flow:             runlog.FlowDiscovery,
		Operation:        runlog.OperationReadJob,
		DiscoveryRunID:   worker.runID,
		AttemptNo:        worker.attemptNo,
		PageNo:           worker.nextPage,
		JobOrdinal:       jobOrdinal + 1,
		JobIDFingerprint: stableIDFingerprint(platformJobID),
	}
	trace, err := s.logs.Start(ctx, attempt)
	if err != nil {
		return JobObservation{}, "", fmt.Errorf("start discovery job trace: %w", err)
	}
	observation, readErr := s.discovery.ReadJob(ctx, platformJobID)
	if readErr != nil {
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome:         runlog.OutcomeFailed,
			ErrorCategory:   fetchErrorCategory(readErr),
			ExternalFailure: runlogFailureEvidence(readErr),
			Err:             readErr,
			ErrorRedactions: []string{platformJobID},
		})
		return JobObservation{}, trace.ID(), errors.Join(
			fmt.Errorf("read discovery job %d on page %d: %w", jobOrdinal+1, worker.nextPage, readErr),
			finishErr,
		)
	}
	validationErr := validateObservation(observation)
	if validationErr == nil && observation.PlatformJobID != platformJobID {
		validationErr = errors.New("read discovery job returned a different stable ID")
	}
	if validationErr != nil {
		failure := invalidResponseError("discovery job %d is unreliable: %v", jobOrdinal+1, validationErr)
		finishErr := s.logs.Finish(ctx, trace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, ErrorCategory: runlog.ErrorCategoryInvalidResponse, Err: failure,
			ErrorRedactions: []string{platformJobID, observation.PlatformJobID},
		})
		return JobObservation{}, trace.ID(), errors.Join(failure, finishErr)
	}
	if err := s.logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		return JobObservation{}, trace.ID(), fmt.Errorf("finish discovery job trace: %w", err)
	}
	return observation, trace.ID(), nil
}

func stableIDFingerprint(platformJobID string) string {
	digest := sha256.Sum256([]byte(platformJobID))
	return hex.EncodeToString(digest[:])
}

func (s *Service) commitJob(
	ctx context.Context,
	worker workerAttempt,
	observation JobObservation,
	jobOrdinal int,
	at time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint := worker.pageCheckpoint
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin current discovery job observation: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	queries := s.queries.WithTx(transaction)
	_, err = queries.LockCurrentDiscoveryJob(ctx, sqlitedb.LockCurrentDiscoveryJobParams{
		RunID:         worker.runID,
		AttemptNo:     worker.attemptNo,
		WorkerOwner:   nullString(worker.owner),
		CurrentRole:   nullString(worker.currentSearchRange.Role),
		CurrentCity:   nullString(worker.currentSearchRange.City),
		NextPage:      nullInt64(int64(worker.nextPage)),
		JobIdsJson:    nullString(checkpoint.encodedJobIDs),
		HasMore:       nullInt64(boolInt64(checkpoint.hasMore)),
		JobOrdinal:    nullInt64(int64(jobOrdinal)),
		PlatformJobID: nullString(observation.PlatformJobID),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return errStaleWorker
	}
	if err != nil {
		return fmt.Errorf("lock current discovery job: %w", err)
	}
	if _, err := s.pool.ObserveInTransaction(
		ctx,
		transaction,
		worker.runID,
		toPoolObservation(observation, at),
	); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit current discovery job observation: %w", err)
	}
	_, err = s.queries.AdvanceDiscoveryJob(ctx, sqlitedb.AdvanceDiscoveryJobParams{
		NextJobOrdinal:    nullInt64(int64(jobOrdinal + 1)),
		ProgressAt:        nullInt64(at.UnixMilli()),
		WorkerLeaseUntil:  nullInt64(at.Add(discoveryWorkerLease).UnixMilli()),
		UpdatedAt:         at.UnixMilli(),
		RunID:             worker.runID,
		AttemptNo:         worker.attemptNo,
		WorkerOwner:       nullString(worker.owner),
		CurrentRole:       nullString(worker.currentSearchRange.Role),
		CurrentCity:       nullString(worker.currentSearchRange.City),
		CurrentPage:       nullInt64(int64(worker.nextPage)),
		JobIdsJson:        nullString(checkpoint.encodedJobIDs),
		HasMore:           nullInt64(boolInt64(checkpoint.hasMore)),
		CurrentJobOrdinal: nullInt64(int64(jobOrdinal)),
		PlatformJobID:     nullString(observation.PlatformJobID),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return errStaleWorker
	}
	if err != nil {
		return fmt.Errorf("advance discovery job checkpoint: %w", err)
	}
	return nil
}

func (s *Service) finishPage(
	ctx context.Context,
	worker workerAttempt,
	next *SearchRange,
	lastRange bool,
	at time.Time,
) error {
	if worker.pageCheckpoint.hasMore {
		return s.advancePage(ctx, worker, at)
	}
	if lastRange {
		return s.completeRun(ctx, worker, at)
	}
	return s.switchRange(ctx, worker, *next, at)
}

func (s *Service) advancePage(ctx context.Context, worker workerAttempt, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	errorRedactions ...string,
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
			Flow:            runlog.FlowDiscovery,
			Stage:           "mark_worker_failed",
			TraceID:         traceID,
			DiscoveryRunID:  worker.runID,
			AttemptNo:       worker.attemptNo,
			Err:             stateErr,
			ErrorRedactions: errorRedactions,
		})
		return errors.Join(cause, stateErr, logErr)
	}
	logErr := s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
		Flow:            runlog.FlowDiscovery,
		Stage:           "worker_attempt_failed",
		TraceID:         traceID,
		DiscoveryRunID:  worker.runID,
		AttemptNo:       worker.attemptNo,
		Err:             cause,
		ErrorRedactions: errorRedactions,
	})
	return errors.Join(cause, logErr)
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

func nullInt64(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: true}
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
