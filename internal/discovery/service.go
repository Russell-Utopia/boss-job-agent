package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

var workerSequence atomic.Uint64

// Service owns job discovery runs.
type Service struct {
	mu             sync.Mutex
	resumeVersions *onlineresume.Versions
	pool           *jobpool.Pool
	discovery      JobDiscovery
	logs           *runlog.Log
	now            func() time.Time
	queries        *sqlitedb.Queries
	workerOwner    string
	wake           chan struct{}
}

func New(
	db *sql.DB,
	resumeVersions *onlineresume.Versions,
	pool *jobpool.Pool,
	discovery JobDiscovery,
	logs *runlog.Log,
	now func() time.Time,
) *Service {
	return &Service{
		resumeVersions: resumeVersions,
		pool:           pool,
		discovery:      discovery,
		logs:           logs,
		now:            now,
		queries:        sqlitedb.New(db),
		workerOwner:    fmt.Sprintf("discovery-%d-%d", os.Getpid(), workerSequence.Add(1)),
		wake:           make(chan struct{}, 1),
	}
}

func (s *Service) Start(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	availability, err := s.StartAvailability(ctx)
	if err != nil {
		return 0, err
	}
	if !availability.Allowed {
		return 0, &Rejection{Code: availability.Code, Reason: availability.Reason}
	}
	current, err := s.resumeVersions.GetCurrent(ctx)
	if err != nil {
		return 0, err
	}
	firstRange := searchRangeFromIntention(current.Content.JobIntentions[0])
	runID, err := s.createRun(ctx, current.ID, firstRange)
	if err != nil {
		return 0, err
	}
	s.signal()
	return runID, nil
}

func (s *Service) createRun(
	ctx context.Context,
	resumeVersionID int64,
	searchRange SearchRange,
) (int64, error) {
	now := s.now()
	runID, err := s.queries.CreateDiscoveryRun(ctx, sqlitedb.CreateDiscoveryRunParams{
		ResumeVersionID: sql.NullInt64{Int64: resumeVersionID, Valid: true},
		CurrentRole:     sql.NullString{String: searchRange.Role, Valid: true},
		CurrentCity:     sql.NullString{String: searchRange.City, Valid: true},
		WorkerOwner:     sql.NullString{String: s.workerOwner, Valid: true},
		WorkerLeaseUntil: sql.NullInt64{
			Int64: now.Add(discoveryWorkerLease).UnixMilli(), Valid: true,
		},
		CreatedAt:  now.UnixMilli(),
		PreparedAt: sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		UpdatedAt:  now.UnixMilli(),
	})
	if err != nil {
		cause := fmt.Errorf("create discovery run: %w", err)
		return 0, s.recordTechnicalError(ctx, "create_run", 0, 0, cause)
	}
	return runID, nil
}

func (s *Service) Pause(ctx context.Context, runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.queries.PauseDiscoveryRun(ctx, sqlitedb.PauseDiscoveryRunParams{
		UpdatedAt: s.now().UnixMilli(),
		RunID:     runID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return &Rejection{Code: "discovery_not_running", Reason: "只有运行中的岗位发现可以暂停"}
	}
	if err != nil {
		cause := fmt.Errorf("pause discovery run %d: %w", runID, err)
		return s.recordTechnicalError(ctx, "pause_run", runID, 0, cause)
	}
	return nil
}

func (s *Service) Continue(ctx context.Context, runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	_, err := s.queries.ContinueDiscoveryRun(ctx, sqlitedb.ContinueDiscoveryRunParams{
		WorkerOwner: sql.NullString{String: s.workerOwner, Valid: true},
		WorkerLeaseUntil: sql.NullInt64{
			Int64: now.Add(discoveryWorkerLease).UnixMilli(), Valid: true,
		},
		UpdatedAt: now.UnixMilli(),
		RunID:     runID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return &Rejection{Code: "discovery_not_resumable", Reason: "只有暂停或失败的岗位发现可以继续"}
	}
	if err != nil {
		cause := fmt.Errorf("continue discovery run %d: %w", runID, err)
		return s.recordTechnicalError(ctx, "continue_run", runID, 0, cause)
	}
	s.signal()
	return nil
}

func (s *Service) EndEarly(ctx context.Context, runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UnixMilli()
	_, err := s.queries.EndDiscoveryRunEarly(ctx, sqlitedb.EndDiscoveryRunEarlyParams{
		FinishedAt: sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:  now,
		RunID:      runID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return &Rejection{Code: "discovery_already_finished", Reason: "该岗位发现已经结束"}
	}
	if err != nil {
		cause := fmt.Errorf("end discovery run %d early: %w", runID, err)
		return s.recordTechnicalError(ctx, "end_run_early", runID, 0, cause)
	}
	return nil
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) recordTechnicalError(
	ctx context.Context,
	stage string,
	runID int64,
	attemptNo int64,
	cause error,
) error {
	logErr := s.logs.RecordTechnicalError(ctx, runlog.TechnicalError{
		Flow:           runlog.FlowDiscovery,
		Stage:          stage,
		DiscoveryRunID: runID,
		AttemptNo:      attemptNo,
		Err:            cause,
	})
	return errors.Join(cause, logErr)
}

// Run advances discovery independently of Web request lifetimes. It performs
// one immediate scan and then scans on either a command wake-up or each minute.
