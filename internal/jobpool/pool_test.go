package jobpool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestObserveKeepsOneGlobalJobPerStablePlatformID(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pool := New(db)

	first := observedJob("boss-job-1")
	if _, err := pool.Observe(t.Context(), 1, first); err != nil {
		t.Fatalf("observe first platform job: %v", err)
	}
	first.ObservedAt = first.ObservedAt.Add(time.Minute)
	if _, err := pool.Observe(t.Context(), 2, first); err != nil {
		t.Fatalf("observe same platform job again: %v", err)
	}
	if _, err := pool.Observe(t.Context(), 2, observedJob("boss-job-2")); err != nil {
		t.Fatalf("observe similar job with new platform ID: %v", err)
	}

	jobs, err := pool.ListJobs(t.Context())
	if err != nil {
		t.Fatalf("list global jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("global jobs = %d, want 2", len(jobs))
	}
	if jobs[0].PlatformJobID != "boss-job-1" || jobs[1].PlatformJobID != "boss-job-2" {
		t.Errorf("platform job IDs = %q, %q", jobs[0].PlatformJobID, jobs[1].PlatformJobID)
	}
}

func TestObserveInTransactionCommitsOnlyWithItsCaller(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	transaction, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin observation transaction: %v", err)
	}
	if _, err := pool.ObserveInTransaction(
		t.Context(),
		transaction,
		1,
		observedJob("boss-job-transactional"),
	); err != nil {
		_ = transaction.Rollback()
		t.Fatalf("observe in caller transaction: %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("roll back observation transaction: %v", err)
	}
	jobs, err := pool.ListJobs(t.Context())
	if err != nil {
		t.Fatalf("list jobs after rollback: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs after rollback = %#v, want none", jobs)
	}
}

func TestAssessmentQueueClaimFinishRejectsLateResults(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	assertNewJobActions(t, job)
	resumeID, policyID := seedAssessmentInputs(t, db)

	queued := mustQueueAssessments(t, pool, job.ID)
	assertBatchResult(t, queued, 1, 0, "")
	claim := AssessmentClaim{
		Worker:           "assessment-worker-1",
		ResumeVersionID:  resumeID,
		PolicyVersionID:  policyID,
		EvaluatorVersion: 7,
		ProcessingLimit:  1,
		ClaimedAt:        time.UnixMilli(2000),
		LeaseUntil:       time.UnixMilli(3000),
	}
	work := mustClaimAssessments(t, pool, claim)
	claimed := requireAssessmentWork(t, work, job.ID, 1)
	assertAssessmentInputs(t, claimed, job.JDHash, resumeID, policyID)

	claim.Worker = "assessment-worker-2"
	claim.ClaimedAt = time.UnixMilli(2500)
	claim.LeaseUntil = time.UnixMilli(3500)
	duplicate := mustClaimAssessments(t, pool, claim)
	assertNoAssessmentWork(t, duplicate)

	finished := mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID:       job.ID,
		AttemptNo:   work[0].AttemptNo,
		Status:      AssessmentStatusSuitable,
		Reason:      "Go 与 SQLite 经验匹配",
		Evidence:    json.RawMessage(`{"matches":["Go","SQLite"]}`),
		CompletedAt: time.UnixMilli(2600),
	})
	assertBatchResult(t, finished, 1, 0, "")

	late := mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID:       job.ID,
		AttemptNo:   work[0].AttemptNo,
		Status:      AssessmentStatusFailed,
		Reason:      "迟到的旧结果",
		Evidence:    json.RawMessage(`{"late":true}`),
		CompletedAt: time.UnixMilli(2700),
	})
	assertBatchResult(t, late, 0, 1, "stale_assessment_attempt")

	current := mustGetJob(t, pool, job.ID)
	assertAssessedJob(t, current, AssessmentStatusSuitable, 1)
	requeued := mustQueueAssessments(t, pool, job.ID)
	assertBatchResult(t, requeued, 0, 1, "assessment_already_completed")
}

func TestConcurrentAssessmentClaimsRespectTheGlobalProcessingLimitWithoutDuplicates(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	resumeID, policyID := seedAssessmentInputs(t, db)
	jobIDs := seedAssessmentJobs(t, pool, 8)
	assertBatchResult(t, mustQueueAssessments(t, pool, jobIDs...), len(jobIDs), 0, "")

	const (
		processingLimit = 3
		claimers        = 5
	)
	results := runConcurrentAssessmentClaims(t, pool, resumeID, policyID, processingLimit, claimers)
	assertUniqueClaimedJobCount(t, results, processingLimit)
	assertProcessingAssessmentCount(t, db, processingLimit)
}

func seedAssessmentJobs(t *testing.T, pool *Pool, count int) []int64 {
	t.Helper()
	jobIDs := make([]int64, count)
	for index := range jobIDs {
		job := mustObserve(t, pool, int64(index+1), observedJob(fmt.Sprintf("boss-job-limit-%d", index+1)))
		jobIDs[index] = job.ID
	}
	return jobIDs
}

type assessmentClaimResult struct {
	work []AssessmentWork
	err  error
}

func runConcurrentAssessmentClaims(
	t *testing.T,
	pool *Pool,
	resumeID int64,
	policyID int64,
	processingLimit int,
	claimers int,
) [][]AssessmentWork {
	t.Helper()
	start := make(chan struct{})
	resultChannel := make(chan assessmentClaimResult, claimers)
	var group sync.WaitGroup
	for index := range claimers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			work, err := pool.ClaimAssessments(t.Context(), AssessmentClaim{
				Worker:          fmt.Sprintf("assessment-worker-%d", index+1),
				ResumeVersionID: resumeID, PolicyVersionID: policyID, EvaluatorVersion: 1,
				ProcessingLimit: processingLimit, ClaimedAt: time.UnixMilli(2_000), LeaseUntil: time.UnixMilli(3_000),
			})
			resultChannel <- assessmentClaimResult{work: work, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(resultChannel)

	results := make([][]AssessmentWork, 0, claimers)
	for result := range resultChannel {
		if result.err != nil {
			t.Fatalf("concurrent assessment claim: %v", result.err)
		}
		results = append(results, result.work)
	}
	return results
}

func assertUniqueClaimedJobCount(t *testing.T, results [][]AssessmentWork, want int) {
	t.Helper()
	claimedJobIDs := make(map[int64]struct{})
	for _, work := range results {
		for _, item := range work {
			if _, duplicate := claimedJobIDs[item.JobID]; duplicate {
				t.Errorf("platform job %d was claimed more than once", item.JobID)
			}
			claimedJobIDs[item.JobID] = struct{}{}
		}
	}
	if len(claimedJobIDs) != want {
		t.Errorf("concurrent claims returned %d jobs, want global limit %d", len(claimedJobIDs), want)
	}
}

func assertProcessingAssessmentCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var processing int
	if err := db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM platform_jobs WHERE assessment_status = 'processing'
	`).Scan(&processing); err != nil {
		t.Fatalf("count processing assessments: %v", err)
	}
	if processing != want {
		t.Errorf("processing assessments = %d, want global limit %d", processing, want)
	}
}

func TestExpiredAssessmentLeaseCountsFailuresStopsAtTheLimitAndRejectsTheOldWorker(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	resumeID, policyID := seedAssessmentInputs(t, db)
	assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 1, 0, "")
	first := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker-1", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(2000), LeaseUntil: time.UnixMilli(3000),
	})
	requireAssessmentWork(t, first, job.ID, 1)
	second := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker-2", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(4000), LeaseUntil: time.UnixMilli(5000),
	})
	requireAssessmentWork(t, second, job.ID, 2)
	third := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker-3", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(6000), LeaseUntil: time.UnixMilli(7000),
	})
	requireAssessmentWork(t, third, job.ID, 3)
	stopped := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker-4", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(8000), LeaseUntil: time.UnixMilli(9000),
	})
	assertNoAssessmentWork(t, stopped)
	assertAssessmentFailureLimit(t, db, job.ID, 3)

	late := mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID: job.ID, AttemptNo: first[0].AttemptNo, Status: AssessmentStatusSuitable,
		Reason: "旧 Worker 结果", Evidence: json.RawMessage(`{"late":true}`), CompletedAt: time.UnixMilli(4100),
	})
	assertBatchResult(t, late, 0, 1, "stale_assessment_attempt")

	changed := observedJob("boss-job-1")
	changed.FullJD = "负责 Go 服务与新队列状态机开发"
	changed.ObservedAt = time.UnixMilli(8500)
	updated := mustObserve(t, pool, 2, changed)
	if updated.AssessmentStatus != AssessmentStatusPending || updated.AssessmentJDHash != "" {
		t.Fatalf("new JD after exhausted failures = %#v, want fresh pending cycle", updated)
	}
	restarted := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker-5", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(9000), LeaseUntil: time.UnixMilli(10000),
	})
	work := requireAssessmentWork(t, restarted, job.ID, 4)
	if work.JDHash != updated.JDHash {
		t.Errorf("restarted JD hash = %q, want latest %q", work.JDHash, updated.JDHash)
	}
}

func assertAssessmentFailureLimit(t *testing.T, db *sql.DB, jobID int64, wantFailures int) {
	t.Helper()
	var status string
	var reason sql.NullString
	var failures int
	var retryAt sql.NullInt64
	err := db.QueryRowContext(t.Context(), `
		SELECT assessment_status, assessment_reason, assessment_consecutive_failure_count, assessment_retry_at
		FROM platform_jobs
		WHERE id = ?
	`, jobID).Scan(&status, &reason, &failures, &retryAt)
	if err != nil {
		t.Fatalf("read assessment failure limit: %v", err)
	}
	if status != string(AssessmentStatusFailed) || failures != wantFailures || retryAt.Valid {
		t.Errorf("assessment failure state = (%q, %d, %#v), want failed, %d, no retry", status, failures, retryAt, wantFailures)
	}
	if !reason.Valid || reason.String != "自动重试已达上限，请手工重试" {
		t.Errorf("assessment failure reason = %#v, want exhausted retry guidance", reason)
	}
}

func seedAssessmentInputs(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) (int64, int64) {
	t.Helper()
	resumeResult, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (1, '{"jobIntentions":[]}', 'resume-v1', 1, 1000)
	`)
	if err != nil {
		t.Fatalf("seed online resume version: %v", err)
	}
	resumeID, err := resumeResult.LastInsertId()
	if err != nil {
		t.Fatalf("read online resume version ID: %v", err)
	}
	policyResult, err := db.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, created_at
		) VALUES (1, '{"rules":["match"]}', 1, 1000)
	`)
	if err != nil {
		t.Fatalf("seed assessment policy version: %v", err)
	}
	policyID, err := policyResult.LastInsertId()
	if err != nil {
		t.Fatalf("read assessment policy version ID: %v", err)
	}
	return resumeID, policyID
}

func TestObserveNormalizesJudgmentContentAndKeepsPlatformStatusOutOfHash(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pool := New(db)

	first := observedJob("boss-job-1")
	first.FullJD = "  负责 Go 服务开发\r\n  维护 SQLite  "
	created, err := pool.Observe(t.Context(), 1, first)
	if err != nil {
		t.Fatalf("observe open platform job: %v", err)
	}

	closed := first
	closed.FullJD = "负责   Go 服务开发\n\n维护 SQLite"
	closed.PlatformStatus = PlatformStatusClosed
	closed.PlatformClosedReason = "岗位已停止招聘"
	closed.ObservedAt = first.ObservedAt.Add(time.Minute)
	updated, err := pool.Observe(t.Context(), 2, closed)
	if err != nil {
		t.Fatalf("observe closed platform job: %v", err)
	}

	if updated.JDHash != created.JDHash {
		t.Errorf("JD hash changed after formatting and platform status changes: %q -> %q", created.JDHash, updated.JDHash)
	}
	if updated.PlatformStatus != PlatformStatusClosed {
		t.Errorf("platform status = %q, want closed", updated.PlatformStatus)
	}
	if updated.PlatformClosedReason != "岗位已停止招聘" {
		t.Errorf("closed reason = %q, want reliable user-visible reason", updated.PlatformClosedReason)
	}
	if !updated.LastSeenAt.Equal(closed.ObservedAt) {
		t.Errorf("last seen at = %v, want %v", updated.LastSeenAt, closed.ObservedAt)
	}
}

func TestUnreliableOrOlderObservationDoesNotOverwriteReliableJobFacts(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	open := observedJob("boss-job-1")
	open.ObservedAt = time.UnixMilli(2000)
	job := mustObserve(t, pool, 1, open)

	unreliable := open
	unreliable.PlatformStatus = ""
	unreliable.FullJD = "未确认的新职责"
	unreliable.ObservedAt = time.UnixMilli(3000)
	if _, err := pool.Observe(t.Context(), 2, unreliable); err == nil {
		t.Fatal("unreliable observation succeeded, want rejection")
	}
	unchanged := mustGetJob(t, pool, job.ID)
	if unchanged.JDHash != job.JDHash || unchanged.PlatformStatus != PlatformStatusOpen || !unchanged.LastSeenAt.Equal(open.ObservedAt) {
		t.Errorf("job after unreliable read = %#v, want last reliable facts", unchanged)
	}

	closed := open
	closed.PlatformStatus = PlatformStatusClosed
	closed.PlatformClosedReason = "岗位已停止招聘"
	closed.ObservedAt = time.UnixMilli(4000)
	mustObserve(t, pool, 3, closed)
	stale := open
	stale.ObservedAt = time.UnixMilli(3000)
	current := mustObserve(t, pool, 2, stale)
	if current.PlatformStatus != PlatformStatusClosed || current.PlatformClosedReason != "岗位已停止招聘" {
		t.Errorf("older observation overwrote closure: %#v", current)
	}
	if !current.LastSeenAt.Equal(closed.ObservedAt) {
		t.Errorf("last seen after older observation = %v, want %v", current.LastSeenAt, closed.ObservedAt)
	}
}

func TestPlatformClosureStopsClaimsAndReopeningRestoresUnchangedWork(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	observation := observedJob("boss-job-1")
	job := mustObserve(t, pool, 1, observation)
	resumeID, policyID := seedAssessmentInputs(t, db)
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable,
	})
	assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 1, 0, "")
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID), 1, 0, "")

	closed := observation
	closed.PlatformStatus = PlatformStatusClosed
	closed.PlatformClosedReason = "岗位已停止招聘"
	closed.ObservedAt = time.UnixMilli(2000)
	closedView := mustObserve(t, pool, 2, closed)
	assertClosedJobState(t, closedView)

	claim := AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(2100), LeaseUntil: time.UnixMilli(3100),
	}
	work := mustClaimAssessments(t, pool, claim)
	assertNoAssessmentWork(t, work)

	reopened := observation
	reopened.ObservedAt = time.UnixMilli(3000)
	reopenedView := mustObserve(t, pool, 3, reopened)
	assertReopenedJobState(t, reopenedView, job.JDHash)
	claim.ClaimedAt = time.UnixMilli(3100)
	claim.LeaseUntil = time.UnixMilli(4100)
	work = mustClaimAssessments(t, pool, claim)
	requireAssessmentWork(t, work, job.ID, 1)
}

func TestJDChangeInvalidatesUncontactedAssessmentAndMakesHumanReviewStale(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	observation := observedJob("boss-job-1")
	job := mustObserve(t, pool, 1, observation)
	resumeID, policyID := seedAssessmentInputs(t, db)
	assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 1, 0, "")
	work := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(1500), LeaseUntil: time.UnixMilli(2500),
	})
	claimed := requireAssessmentWork(t, work, job.ID, 1)
	assertBatchResult(t, mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID: job.ID, AttemptNo: claimed.AttemptNo, Status: AssessmentStatusSuitable,
		Reason: "经验匹配", Evidence: json.RawMessage(`{"matched":true}`), CompletedAt: time.UnixMilli(2000),
	}), 1, 0, "")
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable,
	})
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID), 1, 0, "")

	changed := observation
	changed.FullJD = "负责 Go 服务开发和分布式系统架构"
	changed.ObservedAt = time.UnixMilli(3000)
	updated := mustObserve(t, pool, 2, changed)
	assertInvalidatedJDChange(t, job, updated)
	detail, err := pool.GetJobDetail(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("get stale human review detail: %v", err)
	}
	if detail.HumanReviewStatus != HumanReviewStatusStale || detail.CurrentJudgment.Available ||
		detail.CurrentJudgment.Code != "human_review_stale" || detail.SupervisionLabel != "" {
		t.Errorf("stale review detail = %#v, want blocked current judgment and no supervision label", detail)
	}
}

func TestJDChangeKeepsPendingAssessmentQueuedForTheLatestJD(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	resumeID, policyID := seedAssessmentInputs(t, db)
	assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 1, 0, "")

	changed := observedJob("boss-job-1")
	changed.FullJD = "负责 Go 服务和队列状态机开发"
	changed.ObservedAt = time.UnixMilli(2000)
	updated := mustObserve(t, pool, 2, changed)
	if updated.AssessmentStatus != AssessmentStatusPending || updated.AssessmentJDHash != "" {
		t.Fatalf("changed queued assessment = %#v, want pending without selected inputs", updated)
	}

	work := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(3000), LeaseUntil: time.UnixMilli(4000),
	})
	claimed := requireAssessmentWork(t, work, job.ID, 1)
	if claimed.JDHash != updated.JDHash || claimed.JDHash == job.JDHash {
		t.Errorf("claimed JD hash = %q, want latest %q instead of old %q", claimed.JDHash, updated.JDHash, job.JDHash)
	}
}

func TestJDChangeRestartsFailedAssessmentWithTheLatestInputs(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	resumeID, policyID := seedAssessmentInputs(t, db)
	assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 1, 0, "")
	work := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker-1", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(1500), LeaseUntil: time.UnixMilli(2500),
	})
	claimed := requireAssessmentWork(t, work, job.ID, 1)
	retryAt := time.UnixMilli(3000)
	assertBatchResult(t, mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID: job.ID, AttemptNo: claimed.AttemptNo, Status: AssessmentStatusFailed,
		Reason: "Pi 暂时不可用", Evidence: json.RawMessage(`{"code":"pi_unavailable"}`),
		RetryAt: &retryAt, CompletedAt: time.UnixMilli(2000),
	}), 1, 0, "")

	changed := observedJob("boss-job-1")
	changed.FullJD = "熟悉 Go、SQLite 与队列状态机"
	changed.ObservedAt = time.UnixMilli(2500)
	updated := mustObserve(t, pool, 2, changed)
	if updated.AssessmentStatus != AssessmentStatusPending || updated.AssessmentJDHash != "" || updated.AssessmentReason != "" {
		t.Fatalf("changed failed assessment = %#v, want pending without old inputs or failure", updated)
	}

	retried := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker-2", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: retryAt, LeaseUntil: time.UnixMilli(4000),
	})
	newAttempt := requireAssessmentWork(t, retried, job.ID, 2)
	if newAttempt.AttemptNo != 2 || newAttempt.JDHash != updated.JDHash {
		t.Errorf("retried assessment = %#v, want attempt 2 with latest JD %q", newAttempt, updated.JDHash)
	}
	assertAssessmentFailureCount(t, db, job.ID, 0)
}

func assertAssessmentFailureCount(t *testing.T, db *sql.DB, jobID int64, want int) {
	t.Helper()
	var failures int
	if err := db.QueryRowContext(t.Context(), `
		SELECT assessment_consecutive_failure_count FROM platform_jobs WHERE id = ?
	`, jobID).Scan(&failures); err != nil {
		t.Fatalf("read assessment failure count: %v", err)
	}
	if failures != want {
		t.Errorf("assessment failure count = %d, want %d", failures, want)
	}
}

func TestContactedJobPreservesConclusionsAndRejectsLateOrRepeatedWork(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	observation := observedJob("boss-job-1")
	job := mustObserve(t, pool, 1, observation)
	resumeID, policyID := seedAssessmentInputs(t, db)
	if result := mustQueueAssessments(t, pool, job.ID); result.Succeeded != 1 {
		t.Fatalf("queue assessment: result=%#v", result)
	}
	assessmentWork := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(1500), LeaseUntil: time.UnixMilli(2500),
	})
	assessmentAttempt := requireAssessmentWork(t, assessmentWork, job.ID, 1)
	assertBatchResult(t, mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID: job.ID, AttemptNo: assessmentAttempt.AttemptNo, Status: AssessmentStatusSuitable,
		Reason: "经验匹配", Evidence: json.RawMessage(`{"matched":true}`), CompletedAt: time.UnixMilli(2000),
	}), 1, 0, "")
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable,
	})
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID), 1, 0, "")

	claim := OutreachClaim{
		Worker: "outreach-worker-1", Limit: 1,
		ClaimedAt: time.UnixMilli(2200), LeaseUntil: time.UnixMilli(3200),
	}
	outreachWork := mustClaimOutreach(t, pool, claim)
	outreachAttempt := requireOutreachWork(t, outreachWork, job.ID, 1)
	assertContactWork(t, outreachAttempt, "您好")
	claim.Worker = "outreach-worker-2"
	claim.ClaimedAt = time.UnixMilli(2500)
	claim.LeaseUntil = time.UnixMilli(3500)
	duplicate := mustClaimOutreach(t, pool, claim)
	assertNoOutreachWork(t, duplicate)

	finished := mustFinishOutreach(t, pool, OutreachOutcome{
		JobID: job.ID, AttemptNo: outreachAttempt.AttemptNo,
		Status: OutreachStatusContacted, ContactSource: ContactSourceAgent,
		Evidence: json.RawMessage(`{"contacted":true}`), CompletedAt: time.UnixMilli(2600),
	})
	assertBatchResult(t, finished, 1, 0, "")
	late := mustFinishOutreach(t, pool, OutreachOutcome{
		JobID: job.ID, AttemptNo: outreachAttempt.AttemptNo,
		Status: OutreachStatusFailed, Evidence: json.RawMessage(`{"late":true}`), CompletedAt: time.UnixMilli(2700),
	})
	assertBatchResult(t, late, 0, 1, "stale_outreach_attempt")

	changed := observation
	changed.FullJD = "熟悉 Go、SQLite 和分布式系统"
	changed.ObservedAt = time.UnixMilli(3000)
	updated := mustObserve(t, pool, 2, changed)
	assertContactedJDChange(t, job, updated)
	assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 0, 1, "outreach_already_contacted")
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID), 0, 1, "outreach_already_contacted")
}

func TestExpiredOutreachLeaseRequiresReconciliationInsteadOfResending(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable,
	})
	if result := mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID); result.Succeeded != 1 {
		t.Fatalf("queue outreach: result=%#v", result)
	}
	first := mustClaimOutreach(t, pool, OutreachClaim{
		Worker: "outreach-worker-1", Limit: 1,
		ClaimedAt: time.UnixMilli(2000), LeaseUntil: time.UnixMilli(3000),
	})
	if len(first) != 1 {
		t.Fatalf("claim first outreach attempt: work=%#v", first)
	}
	second := mustClaimOutreach(t, pool, OutreachClaim{
		Worker: "outreach-worker-2", Limit: 1,
		ClaimedAt: time.UnixMilli(4000), LeaseUntil: time.UnixMilli(5000),
	})
	if len(second) != 1 || second[0].JobID != job.ID || second[0].AttemptNo != 2 {
		t.Fatalf("reclaimed outreach = %#v, want job %d attempt 2", second, job.ID)
	}
	if second[0].Mode != OutreachModeReconcile {
		t.Fatalf("expired outreach mode = %q, want reconciliation", second[0].Mode)
	}
	late := mustFinishOutreach(t, pool, OutreachOutcome{
		JobID: job.ID, AttemptNo: first[0].AttemptNo,
		Status: OutreachStatusContacted, ContactSource: ContactSourceAgent,
		Evidence: json.RawMessage(`{"late":true}`), CompletedAt: time.UnixMilli(4100),
	})
	if late.Succeeded != 0 || len(late.Skipped) != 1 {
		t.Fatalf("expired outreach finish = %#v, want skipped", late)
	}
}

func TestOutreachAutomaticRetryStopsAfterThreeAttempts(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable,
	})
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID), 1, 0, "")

	for attempt := int64(1); attempt <= unattendedAttemptLimit; attempt++ {
		claimedAt := time.UnixMilli(attempt * 2000)
		work := mustClaimOutreach(t, pool, OutreachClaim{
			Worker: fmt.Sprintf("outreach-worker-%d", attempt), Limit: 1,
			ClaimedAt: claimedAt, LeaseUntil: claimedAt.Add(time.Second),
		})
		claimed := requireOutreachWork(t, work, job.ID, attempt)
		retryAt := claimedAt.Add(2 * time.Second)
		assertBatchResult(t, mustFinishOutreach(t, pool, OutreachOutcome{
			JobID: job.ID, AttemptNo: claimed.AttemptNo, Status: OutreachStatusFailed,
			Evidence: json.RawMessage(`{"code":"boss_temporarily_unavailable"}`),
			RetryAt:  &retryAt, CompletedAt: claimedAt.Add(time.Second),
		}), 1, 0, "")
	}

	stopped := mustClaimOutreach(t, pool, OutreachClaim{
		Worker: "outreach-worker-4", Limit: 1,
		ClaimedAt: time.UnixMilli(8000), LeaseUntil: time.UnixMilli(9000),
	})
	assertNoOutreachWork(t, stopped)
	assertOutreachFailureLimit(t, db, job.ID, int(unattendedAttemptLimit))

	assertBatchResult(t, mustRetryOutreach(t, pool, job.ID), 1, 0, "")
	restarted := mustClaimOutreach(t, pool, OutreachClaim{
		Worker: "outreach-worker-5", Limit: 1,
		ClaimedAt: time.UnixMilli(9000), LeaseUntil: time.UnixMilli(10000),
	})
	requireOutreachWork(t, restarted, job.ID, 4)
}

func assertOutreachFailureLimit(t *testing.T, db *sql.DB, jobID int64, wantFailures int) {
	t.Helper()
	var status string
	var failures int
	var retryAt sql.NullInt64
	err := db.QueryRowContext(t.Context(), `
		SELECT outreach_status, outreach_consecutive_failure_count, outreach_retry_at
		FROM platform_jobs
		WHERE id = ?
	`, jobID).Scan(&status, &failures, &retryAt)
	if err != nil {
		t.Fatalf("read outreach failure limit: %v", err)
	}
	if status != string(OutreachStatusFailed) || failures != wantFailures || retryAt.Valid {
		t.Errorf("outreach failure state = (%q, %d, %#v), want failed, %d, no retry", status, failures, retryAt, wantFailures)
	}
}

func TestAutomaticAdmissionUsesCurrentEligibilityAndLimit(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	jobs := make([]JobView, 0, 3)
	for index := 1; index <= 3; index++ {
		job := mustObserve(t, pool, 1, observedJob(fmt.Sprintf("boss-job-%d", index)))
		jobs = append(jobs, job)
		mustReview(t, pool, ReviewDecision{
			JobID: job.ID, Verdict: HumanVerdictSuitable,
		})
	}
	closed := observedJob("boss-job-3")
	closed.PlatformStatus = PlatformStatusClosed
	closed.PlatformClosedReason = "岗位已停止招聘"
	closed.ObservedAt = time.UnixMilli(2000)
	mustObserve(t, pool, 2, closed)

	admittedAssessments := mustAdmitAssessments(t, pool, 10)
	if admittedAssessments != 2 {
		t.Fatalf("admitted assessments = %d, want 2 open jobs", admittedAssessments)
	}
	authorization := OutreachAuthorization{GreetingText: "您好", TimeDescription: "全天可打招呼"}
	admittedOutreach := mustAdmitOutreach(t, pool, authorization, 1)
	if admittedOutreach != 1 {
		t.Fatalf("first outreach admission = %d, want 1", admittedOutreach)
	}
	admittedOutreach = mustAdmitOutreach(t, pool, authorization, 10)
	if admittedOutreach != 1 {
		t.Fatalf("remaining outreach admission = %d, want 1", admittedOutreach)
	}

	first := mustGetJob(t, pool, jobs[0].ID)
	if first.AssessmentStatus != AssessmentStatusPending || first.OutreachStatus != OutreachStatusPending {
		t.Errorf("first admitted job = %#v, want both pending", first)
	}
	if first.OutreachGreetingText != "您好" {
		t.Errorf("automatic outreach greeting = %q, want frozen greeting", first.OutreachGreetingText)
	}
	third := mustGetJob(t, pool, jobs[2].ID)
	if third.AssessmentStatus != AssessmentStatusNotQueued || third.OutreachStatus != OutreachStatusNotQueued {
		t.Errorf("closed job admission state = %#v, want not queued", third)
	}
}

func TestManualRetriesAcceptFailuresButNeverPossiblyContactedWork(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	assessmentJob := mustObserve(t, pool, 1, observedJob("boss-job-assessment"))
	resumeID, policyID := seedAssessmentInputs(t, db)
	assertBatchResult(t, mustQueueAssessments(t, pool, assessmentJob.ID), 1, 0, "")
	assessmentWork := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(2000), LeaseUntil: time.UnixMilli(3000),
	})
	assessmentAttempt := requireAssessmentWork(t, assessmentWork, assessmentJob.ID, 1)
	assertBatchResult(t, mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID: assessmentJob.ID, AttemptNo: assessmentAttempt.AttemptNo, Status: AssessmentStatusFailed,
		Reason: "Pi 返回无效", Evidence: json.RawMessage(`{"invalid":true}`), CompletedAt: time.UnixMilli(2500),
	}), 1, 0, "")
	assertBatchResult(t, mustRetryAssessments(t, pool, assessmentJob.ID), 1, 0, "")
	retryView := mustGetJob(t, pool, assessmentJob.ID)
	assertRetriedAssessment(t, retryView)

	outreachJob := mustObserve(t, pool, 1, observedJob("boss-job-outreach"))
	mustReview(t, pool, ReviewDecision{
		JobID: outreachJob.ID, Verdict: HumanVerdictSuitable,
	})
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, outreachJob.ID), 1, 0, "")
	outreachWork := mustClaimOutreach(t, pool, OutreachClaim{
		Worker: "outreach-worker", Limit: 1,
		ClaimedAt: time.UnixMilli(3000), LeaseUntil: time.UnixMilli(4000),
	})
	outreachAttempt := requireOutreachWork(t, outreachWork, outreachJob.ID, 1)
	assertBatchResult(t, mustFinishOutreach(t, pool, OutreachOutcome{
		JobID: outreachJob.ID, AttemptNo: outreachAttempt.AttemptNo, Status: OutreachStatusFailed,
		Evidence: json.RawMessage(`{"sent":false}`), CompletedAt: time.UnixMilli(3500),
	}), 1, 0, "")
	assertBatchResult(t, mustRetryOutreach(t, pool, outreachJob.ID), 1, 0, "")
	retryWork := mustClaimOutreach(t, pool, OutreachClaim{
		Worker: "outreach-worker", Limit: 1,
		ClaimedAt: time.UnixMilli(4000), LeaseUntil: time.UnixMilli(5000),
	})
	retryAttempt := requireOutreachWork(t, retryWork, outreachJob.ID, 2)
	assertBatchResult(t, mustFinishOutreach(t, pool, OutreachOutcome{
		JobID: outreachJob.ID, AttemptNo: retryAttempt.AttemptNo, Status: OutreachStatusPossiblyContacted,
		Evidence: json.RawMessage(`{"uncertain":true}`), CompletedAt: time.UnixMilli(4500),
	}), 1, 0, "")
	result := mustRetryOutreach(t, pool, outreachJob.ID)
	assertBatchResult(t, result, 0, 1, "outreach_reconciliation_required")
}

func TestInvalidBusinessCombinationsAreRejectedWithoutMutation(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	if err := pool.Review(t.Context(), []ReviewDecision{{
		JobID: job.ID, Verdict: "maybe",
	}}); err == nil {
		t.Fatal("invalid human verdict succeeded")
	}
	if _, err := pool.ClaimAssessments(t.Context(), AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: 1, PolicyVersionID: 1,
		EvaluatorVersion: 1, ProcessingLimit: 1,
		ClaimedAt: time.UnixMilli(3000), LeaseUntil: time.UnixMilli(3000),
	}); err == nil {
		t.Fatal("non-future assessment lease succeeded")
	}
	if _, err := pool.FinishAssessments(t.Context(), []AssessmentOutcome{{
		JobID: job.ID, AttemptNo: 1, Status: AssessmentStatusSuitable,
		Reason: "无效证据", Evidence: json.RawMessage(`not-json`), CompletedAt: time.UnixMilli(3000),
	}}); err == nil {
		t.Fatal("assessment with invalid JSON evidence succeeded")
	}
	if _, err := pool.FinishOutreach(t.Context(), []OutreachOutcome{{
		JobID: job.ID, AttemptNo: 1, Status: OutreachStatusContacted,
		Evidence: json.RawMessage(`{"contacted":true}`), CompletedAt: time.UnixMilli(3000),
	}}); err == nil {
		t.Fatal("contacted outreach without source succeeded")
	}
	current := mustGetJob(t, pool, job.ID)
	if current.HumanVerdict != "" || current.AssessmentStatus != AssessmentStatusNotQueued || current.OutreachStatus != OutreachStatusNotQueued {
		t.Errorf("job changed after invalid commands: %#v", current)
	}
}

func observedJob(platformJobID string) Observation {
	return Observation{
		PlatformJobID:  platformJobID,
		CanonicalURL:   "https://www.zhipin.com/job_detail/" + platformJobID + ".html",
		JobTitle:       "Go 后端工程师",
		CompanyName:    "示例科技",
		City:           "福州",
		Salary:         "20-30K",
		FullJD:         "负责 Go 服务开发\n熟悉 Go 与 SQLite",
		PlatformStatus: PlatformStatusOpen,
		ObservedAt:     time.UnixMilli(1000),
	}
}

func TestObserveKeepsJobWhenReliableSalaryIsUnavailable(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	job := observedJob("boss-job-without-salary")
	job.Salary = ""
	got, err := pool.Observe(t.Context(), 1, job)
	if err != nil {
		t.Fatalf("observe platform job without reliable salary: %v", err)
	}
	if got.Salary != "" {
		t.Errorf("observed salary = %q, want unavailable empty value", got.Salary)
	}

	jobs, err := pool.ListJobs(t.Context())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Salary != "" {
		t.Errorf("listed jobs = %#v, want one job with unavailable salary", jobs)
	}
}

func TestReliableSalaryAppearingChangesJudgmentHash(t *testing.T) {
	t.Parallel()

	withoutSalary := observedJob("boss-job-salary-change")
	withoutSalary.Salary = ""
	_, withoutHash, err := encodeJudgmentContent(withoutSalary)
	if err != nil {
		t.Fatalf("encode job without reliable salary: %v", err)
	}
	_, withHash, err := encodeJudgmentContent(observedJob("boss-job-salary-change"))
	if err != nil {
		t.Fatalf("encode job with reliable salary: %v", err)
	}
	if withoutHash == withHash {
		t.Fatal("judgment hash did not change when reliable salary appeared")
	}
}

func openTestPool(t *testing.T) (*Pool, *sql.DB) {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), db
}

func mustObserve(t *testing.T, pool *Pool, runID int64, observation Observation) JobView {
	t.Helper()
	job, err := pool.Observe(t.Context(), runID, observation)
	if err != nil {
		t.Fatalf("observe platform job: %v", err)
	}
	return job
}

func mustGetJob(t *testing.T, pool *Pool, jobID int64) JobView {
	t.Helper()
	job, err := pool.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatalf("get platform job: %v", err)
	}
	return job
}

func mustGetJobDetail(t *testing.T, pool *Pool, jobID int64) JobDetailView {
	t.Helper()
	detail, err := pool.GetJobDetail(t.Context(), jobID)
	if err != nil {
		t.Fatalf("get platform job detail: %v", err)
	}
	return detail
}

func mustReview(t *testing.T, pool *Pool, decision ReviewDecision) {
	t.Helper()
	if decision.ExpectedJDHash == "" {
		decision.ExpectedJDHash = mustGetJob(t, pool, decision.JobID).JDHash
	}
	if err := pool.Review(t.Context(), []ReviewDecision{decision}); err != nil {
		t.Fatalf("review platform job: %v", err)
	}
}

func mustQueueAssessments(t *testing.T, pool *Pool, jobIDs ...int64) BatchActionResult {
	t.Helper()
	result, err := pool.QueueAssessments(t.Context(), jobIDs)
	if err != nil {
		t.Fatalf("queue assessments: %v", err)
	}
	return result
}

func mustQueueOutreach(t *testing.T, pool *Pool, authorization OutreachAuthorization, jobIDs ...int64) BatchActionResult {
	t.Helper()
	result, err := pool.QueueAuthorizedOutreach(t.Context(), jobIDs, authorization)
	if err != nil {
		t.Fatalf("queue outreach: %v", err)
	}
	return result
}

func mustClaimAssessments(t *testing.T, pool *Pool, claim AssessmentClaim) []AssessmentWork {
	t.Helper()
	work, err := pool.ClaimAssessments(t.Context(), claim)
	if err != nil {
		t.Fatalf("claim assessments: %v", err)
	}
	return work
}

func mustFinishAssessments(t *testing.T, pool *Pool, outcomes ...AssessmentOutcome) BatchActionResult {
	t.Helper()
	result, err := pool.FinishAssessments(t.Context(), outcomes)
	if err != nil {
		t.Fatalf("finish assessments: %v", err)
	}
	return result
}

func mustClaimOutreach(t *testing.T, pool *Pool, claim OutreachClaim) []OutreachWork {
	t.Helper()
	work, err := pool.ClaimOutreach(t.Context(), claim)
	if err != nil {
		t.Fatalf("claim outreach: %v", err)
	}
	return work
}

func mustFinishOutreach(t *testing.T, pool *Pool, outcomes ...OutreachOutcome) BatchActionResult {
	t.Helper()
	result, err := pool.FinishOutreach(t.Context(), outcomes)
	if err != nil {
		t.Fatalf("finish outreach: %v", err)
	}
	return result
}

func mustAdmitAssessments(t *testing.T, pool *Pool, limit int) int {
	t.Helper()
	count, err := pool.AdmitAssessments(t.Context(), limit)
	if err != nil {
		t.Fatalf("admit assessments: %v", err)
	}
	return count
}

func mustAdmitOutreach(t *testing.T, pool *Pool, authorization OutreachAuthorization, limit int) int {
	t.Helper()
	count, err := pool.AdmitOutreach(t.Context(), authorization, limit)
	if err != nil {
		t.Fatalf("admit outreach: %v", err)
	}
	return count
}

func mustRetryAssessments(t *testing.T, pool *Pool, jobIDs ...int64) BatchActionResult {
	t.Helper()
	result, err := pool.RetryAssessmentFailures(t.Context(), jobIDs)
	if err != nil {
		t.Fatalf("retry assessment failures: %v", err)
	}
	return result
}

func mustRetryOutreach(t *testing.T, pool *Pool, jobIDs ...int64) BatchActionResult {
	t.Helper()
	result, err := pool.RetryOutreachFailures(t.Context(), jobIDs)
	if err != nil {
		t.Fatalf("retry outreach failures: %v", err)
	}
	return result
}

func assertBatchResult(t *testing.T, result BatchActionResult, succeeded, skipped int, code string) {
	t.Helper()
	if result.Succeeded != succeeded {
		t.Fatalf("succeeded actions = %d, want %d; result=%#v", result.Succeeded, succeeded, result)
	}
	if len(result.Skipped) != skipped {
		t.Fatalf("skipped actions = %d, want %d; result=%#v", len(result.Skipped), skipped, result)
	}
	if code != "" && result.Skipped[0].Code != code {
		t.Fatalf("skipped code = %q, want %q; result=%#v", result.Skipped[0].Code, code, result)
	}
}

func requireAssessmentWork(t *testing.T, work []AssessmentWork, jobID, attemptNo int64) AssessmentWork {
	t.Helper()
	if len(work) != 1 {
		t.Fatalf("assessment work count = %d, want 1; work=%#v", len(work), work)
	}
	if work[0].JobID != jobID {
		t.Fatalf("assessment job ID = %d, want %d", work[0].JobID, jobID)
	}
	if work[0].AttemptNo != attemptNo {
		t.Fatalf("assessment attempt = %d, want %d", work[0].AttemptNo, attemptNo)
	}
	return work[0]
}

func requireOutreachWork(t *testing.T, work []OutreachWork, jobID, attemptNo int64) OutreachWork {
	t.Helper()
	if len(work) != 1 {
		t.Fatalf("outreach work count = %d, want 1; work=%#v", len(work), work)
	}
	if work[0].JobID != jobID {
		t.Fatalf("outreach job ID = %d, want %d", work[0].JobID, jobID)
	}
	if work[0].AttemptNo != attemptNo {
		t.Fatalf("outreach attempt = %d, want %d", work[0].AttemptNo, attemptNo)
	}
	return work[0]
}

func assertNoAssessmentWork(t *testing.T, work []AssessmentWork) {
	t.Helper()
	if len(work) != 0 {
		t.Fatalf("assessment work = %#v, want none", work)
	}
}

func assertNoOutreachWork(t *testing.T, work []OutreachWork) {
	t.Helper()
	if len(work) != 0 {
		t.Fatalf("outreach work = %#v, want none", work)
	}
}

func assertNewJobActions(t *testing.T, job JobView) {
	t.Helper()
	if !job.AssessmentAction.Allowed {
		t.Errorf("new job assessment action = %#v, want allowed", job.AssessmentAction)
	}
	if !job.ReviewAction.Allowed {
		t.Errorf("new job review action = %#v, want allowed", job.ReviewAction)
	}
	if job.OutreachAction.Allowed {
		t.Errorf("new job outreach action = %#v, want disabled", job.OutreachAction)
	}
	if job.OutreachAction.Code != "suitable_judgment_required" {
		t.Errorf("new job outreach code = %q, want suitable_judgment_required", job.OutreachAction.Code)
	}
}

func assertAssessmentInputs(t *testing.T, work AssessmentWork, jdHash string, resumeID, policyID int64) {
	t.Helper()
	if work.JDHash != jdHash {
		t.Errorf("assessment JD hash = %q, want %q", work.JDHash, jdHash)
	}
	if work.ResumeVersionID != resumeID {
		t.Errorf("assessment resume version = %d, want %d", work.ResumeVersionID, resumeID)
	}
	if work.PolicyVersionID != policyID {
		t.Errorf("assessment policy version = %d, want %d", work.PolicyVersionID, policyID)
	}
}

func assertAssessedJob(t *testing.T, job JobView, status AssessmentStatus, attemptNo int64) {
	t.Helper()
	if job.AssessmentStatus != status {
		t.Fatalf("assessment status = %q, want %q", job.AssessmentStatus, status)
	}
	if job.AssessmentAttemptNo != attemptNo {
		t.Fatalf("assessment attempt = %d, want %d", job.AssessmentAttemptNo, attemptNo)
	}
}

func assertClosedJobState(t *testing.T, job JobView) {
	t.Helper()
	if job.AssessmentStatus != AssessmentStatusPending {
		t.Errorf("closed assessment status = %q, want pending", job.AssessmentStatus)
	}
	if job.OutreachStatus != OutreachStatusNotQueued {
		t.Errorf("closed outreach status = %q, want not_queued", job.OutreachStatus)
	}
	if job.OutreachGreetingText != "" {
		t.Errorf("closed outreach greeting = %q, want empty", job.OutreachGreetingText)
	}
}

func assertReopenedJobState(t *testing.T, job JobView, jdHash string) {
	t.Helper()
	if job.JDHash != jdHash {
		t.Fatalf("reopened JD hash = %q, want %q", job.JDHash, jdHash)
	}
	if job.AssessmentStatus != AssessmentStatusPending {
		t.Fatalf("reopened assessment status = %q, want pending", job.AssessmentStatus)
	}
}

func assertInvalidatedJDChange(t *testing.T, original, updated JobView) {
	t.Helper()
	if updated.JDHash == original.JDHash {
		t.Fatal("changed judgment content kept the old JD hash")
	}
	assertClearedAssessment(t, updated)
	assertStaleHumanReview(t, original, updated)
	assertWithdrawnOutreach(t, updated)
}

func assertClearedAssessment(t *testing.T, job JobView) {
	t.Helper()
	if job.AssessmentStatus != AssessmentStatusNotQueued {
		t.Errorf("changed assessment status = %q, want not_queued", job.AssessmentStatus)
	}
	if job.AssessmentJDHash != "" || job.AssessmentReason != "" {
		t.Errorf("changed assessment inputs remain: %#v", job)
	}
	if job.AssessmentAttemptNo != 1 {
		t.Errorf("assessment attempt = %d, want lifecycle attempt 1", job.AssessmentAttemptNo)
	}
}

func assertStaleHumanReview(t *testing.T, original, updated JobView) {
	t.Helper()
	if updated.HumanVerdict != HumanVerdictSuitable {
		t.Errorf("human verdict = %q, want suitable preserved", updated.HumanVerdict)
	}
	if updated.HumanReviewedJDHash != original.JDHash {
		t.Errorf("human reviewed JD = %q, want %q", updated.HumanReviewedJDHash, original.JDHash)
	}
	if updated.HumanReviewedJDHash == updated.JDHash {
		t.Error("old human review unexpectedly applies to changed JD")
	}
}

func assertWithdrawnOutreach(t *testing.T, job JobView) {
	t.Helper()
	if job.OutreachStatus != OutreachStatusNotQueued {
		t.Errorf("changed outreach status = %q, want not_queued", job.OutreachStatus)
	}
	if job.OutreachGreetingText != "" {
		t.Errorf("changed outreach greeting = %q, want empty", job.OutreachGreetingText)
	}
}

func assertContactWork(t *testing.T, work OutreachWork, greeting string) {
	t.Helper()
	if work.Mode != OutreachModeContact {
		t.Errorf("outreach mode = %q, want contact", work.Mode)
	}
	if work.GreetingText != greeting {
		t.Errorf("outreach greeting = %q, want %q", work.GreetingText, greeting)
	}
}

func assertContactedJDChange(t *testing.T, original, updated JobView) {
	t.Helper()
	if updated.JDHash == original.JDHash {
		t.Fatal("changed contacted job kept old JD hash")
	}
	if updated.OutreachStatus != OutreachStatusContacted {
		t.Fatalf("contacted status = %q, want contacted", updated.OutreachStatus)
	}
	assertContactedAssessment(t, original, updated)
	assertContactedHumanReview(t, original, updated)
}

func assertContactedAssessment(t *testing.T, original, updated JobView) {
	t.Helper()
	if updated.AssessmentStatus != AssessmentStatusSuitable {
		t.Errorf("contacted assessment status = %q, want suitable", updated.AssessmentStatus)
	}
	if updated.AssessmentJDHash != original.JDHash {
		t.Errorf("contacted assessment JD = %q, want %q", updated.AssessmentJDHash, original.JDHash)
	}
}

func assertContactedHumanReview(t *testing.T, original, updated JobView) {
	t.Helper()
	if updated.HumanVerdict != HumanVerdictSuitable {
		t.Errorf("contacted human verdict = %q, want suitable", updated.HumanVerdict)
	}
	if updated.HumanReviewedJDHash != original.JDHash {
		t.Errorf("contacted review JD = %q, want %q", updated.HumanReviewedJDHash, original.JDHash)
	}
}

func assertRetriedAssessment(t *testing.T, job JobView) {
	t.Helper()
	if job.AssessmentStatus != AssessmentStatusPending {
		t.Errorf("retried assessment status = %q, want pending", job.AssessmentStatus)
	}
	if job.AssessmentJDHash != "" {
		t.Errorf("retried assessment JD = %q, want cleared", job.AssessmentJDHash)
	}
}

func assertOutreachAllowed(t *testing.T, job JobView) {
	t.Helper()
	if !job.OutreachAction.Allowed {
		t.Errorf("outreach action = %#v, want allowed", job.OutreachAction)
	}
}

func assertQueuedReviewedOutreach(t *testing.T, job JobView, greeting string) {
	t.Helper()
	if job.HumanVerdict != HumanVerdictSuitable {
		t.Fatalf("queued human verdict = %q, want suitable", job.HumanVerdict)
	}
	if job.OutreachStatus != OutreachStatusPending {
		t.Fatalf("queued outreach status = %q, want pending", job.OutreachStatus)
	}
	if job.OutreachGreetingText != greeting {
		t.Errorf("queued greeting = %q, want %q", job.OutreachGreetingText, greeting)
	}
}

func TestQueueAuthorizedOutreachRejectsWhenNoJobIsAvailable(t *testing.T) {
	t.Parallel()

	pool := &Pool{}
	_, err := pool.QueueAuthorizedOutreach(t.Context(), nil, OutreachAuthorization{})
	var rejection *Rejection
	ok := errors.As(err, &rejection)
	if !ok {
		t.Fatalf("queue authorized outreach error = %v, want business rejection", err)
	}
	if rejection.Code != "outreach_unavailable" {
		t.Errorf("rejection code = %q, want outreach_unavailable", rejection.Code)
	}
	if rejection.Reason != "当前没有可真实打招呼的岗位" {
		t.Errorf("rejection reason = %q, want current availability reason", rejection.Reason)
	}
}

func TestReviewAtomicallyControlsPendingOutreachEligibility(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))

	result := mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText:    "您好，想和您聊聊这个岗位",
		TimeDescription: "全天可打招呼",
	}, job.ID)
	assertBatchResult(t, result, 0, 1, "suitable_judgment_required")

	mustReview(t, pool, ReviewDecision{
		JobID:   job.ID,
		Verdict: HumanVerdictSuitable,
		Note:    "经验匹配",
	})
	reviewed := mustGetJob(t, pool, job.ID)
	assertOutreachAllowed(t, reviewed)
	result = mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText:    "您好，想和您聊聊这个岗位",
		TimeDescription: "全天可打招呼",
	}, job.ID)
	assertBatchResult(t, result, 1, 0, "")

	queued := mustGetJob(t, pool, job.ID)
	assertQueuedReviewedOutreach(t, queued, "您好，想和您聊聊这个岗位")

	mustReview(t, pool, ReviewDecision{
		JobID:   job.ID,
		Verdict: HumanVerdictUnsuitable,
	})
	withdrawn := mustGetJob(t, pool, job.ID)
	assertWithdrawnOutreach(t, withdrawn)
}

func TestReviewDoesNotRollbackOutreachAlreadyClaimedByAWorker(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-claimed-outreach"))
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable,
	})
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID), 1, 0, "")
	work := mustClaimOutreach(t, pool, OutreachClaim{
		Worker: "outreach-worker", Limit: 1,
		ClaimedAt: time.UnixMilli(2000), LeaseUntil: time.UnixMilli(3000),
	})
	requireOutreachWork(t, work, job.ID, 1)

	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictUnsuitable,
	})
	current := mustGetJob(t, pool, job.ID)
	if current.HumanVerdict != HumanVerdictUnsuitable || current.OutreachStatus != OutreachStatusProcessing ||
		current.OutreachLeaseOwner != "outreach-worker" {
		t.Errorf("claimed outreach after unsuitable review = %#v, want external-effect state preserved", current)
	}
}

func TestJobDetailUsesTheLatestHumanReviewAsCurrentJudgmentAndSupervision(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-1"))
	resumeID, policyID := seedAssessmentInputs(t, db)
	assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 1, 0, "")
	work := mustClaimAssessments(t, pool, AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 7, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(1500), LeaseUntil: time.UnixMilli(2500),
	})
	assertBatchResult(t, mustFinishAssessments(t, pool, AssessmentOutcome{
		JobID: job.ID, AttemptNo: work[0].AttemptNo, Status: AssessmentStatusUnsuitable,
		Reason: "缺少高并发经验", Evidence: json.RawMessage(`{"missing":["高并发"]}`),
		CompletedAt: time.UnixMilli(2000),
	}), 1, 0, "")

	reviewedAt := time.UnixMilli(3000)
	pool.now = func() time.Time { return reviewedAt }
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable, Note: "项目经历可以覆盖",
	})
	detail := mustGetJobDetail(t, pool, job.ID)
	assertAssessmentInputVersions(t, detail.AssessmentInputs, 1, 1, 7)
	assertCurrentHumanJudgment(t, detail, HumanVerdictSuitable)

	reviewedAt = time.UnixMilli(4000)
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictUnsuitable, Note: "重新核对后不匹配",
	})
	detail = mustGetJobDetail(t, pool, job.ID)
	assertLatestHumanReview(t, detail, HumanVerdictUnsuitable, "重新核对后不匹配", reviewedAt)
}

func assertAssessmentInputVersions(
	t *testing.T,
	inputs AssessmentInputVersions,
	resumeVersion, policyVersion, evaluatorVersion int64,
) {
	t.Helper()
	if inputs.ResumeVersion != resumeVersion || inputs.PolicyVersion != policyVersion ||
		inputs.EvaluatorVersion != evaluatorVersion {
		t.Errorf(
			"assessment inputs = %#v, want resume v%d, policy v%d, evaluator v%d",
			inputs, resumeVersion, policyVersion, evaluatorVersion,
		)
	}
}

func assertCurrentHumanJudgment(t *testing.T, detail JobDetailView, verdict HumanVerdict) {
	t.Helper()
	if !detail.CurrentJudgment.Available || detail.CurrentJudgment.Source != JudgmentSourceHuman ||
		detail.CurrentJudgment.Verdict != JudgmentVerdict(verdict) {
		t.Errorf("current judgment = %#v, want current %s human judgment", detail.CurrentJudgment, verdict)
	}
	if detail.SupervisionLabel != verdict {
		t.Errorf("supervision label = %q, want %q", detail.SupervisionLabel, verdict)
	}
}

func assertLatestHumanReview(
	t *testing.T,
	detail JobDetailView,
	verdict HumanVerdict,
	note string,
	reviewedAt time.Time,
) {
	t.Helper()
	if detail.HumanVerdict != verdict || detail.HumanReviewNote != note ||
		detail.HumanReviewedAt == nil || !detail.HumanReviewedAt.Equal(reviewedAt) {
		t.Errorf("latest human review = %#v, want %q with the latest note and time", detail.JobView, verdict)
	}
	if detail.SupervisionLabel != verdict {
		t.Errorf("latest supervision label = %q, want %q", detail.SupervisionLabel, verdict)
	}
}

func TestReviewAllowsEveryAIStateWithoutStartingAnotherAssessment(t *testing.T) {
	t.Parallel()

	pool, db := openTestPool(t)
	resumeID, policyID := seedAssessmentInputs(t, db)
	tests := []struct {
		name   string
		status AssessmentStatus
	}{
		{name: "without AI conclusion", status: AssessmentStatusNotQueued},
		{name: "AI suitable", status: AssessmentStatusSuitable},
		{name: "AI unsuitable", status: AssessmentStatusUnsuitable},
		{name: "AI needs confirmation", status: AssessmentStatusNeedsUserConfirmation},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := mustObserve(t, pool, int64(index+1), observedJob(fmt.Sprintf("boss-job-review-%d", index)))
			if test.status != AssessmentStatusNotQueued {
				assertBatchResult(t, mustQueueAssessments(t, pool, job.ID), 1, 0, "")
				work := mustClaimAssessments(t, pool, AssessmentClaim{
					Worker: fmt.Sprintf("assessment-worker-%d", index), ResumeVersionID: resumeID,
					PolicyVersionID: policyID, EvaluatorVersion: 1, ProcessingLimit: 1,
					ClaimedAt:  time.UnixMilli(int64(2000 + index*1000)),
					LeaseUntil: time.UnixMilli(int64(2500 + index*1000)),
				})
				assertBatchResult(t, mustFinishAssessments(t, pool, AssessmentOutcome{
					JobID: job.ID, AttemptNo: work[0].AttemptNo, Status: test.status,
					Reason: "AI 原结论", Evidence: json.RawMessage(`{"source":"ai"}`),
					CompletedAt: time.UnixMilli(int64(2200 + index*1000)),
				}), 1, 0, "")
			}

			before := mustGetJob(t, pool, job.ID)
			mustReview(t, pool, ReviewDecision{
				JobID: job.ID, Verdict: HumanVerdictSuitable,
			})
			after := mustGetJob(t, pool, job.ID)
			if after.HumanVerdict != HumanVerdictSuitable {
				t.Errorf("human verdict = %q, want suitable", after.HumanVerdict)
			}
			if after.AssessmentStatus != before.AssessmentStatus || after.AssessmentAttemptNo != before.AssessmentAttemptNo ||
				after.AssessmentReason != before.AssessmentReason {
				t.Errorf("assessment changed during review: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestReviewAndOutreachClaimLeaveOnlyAnAtomicEligibilityOutcome(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	job := mustObserve(t, pool, 1, observedJob("boss-job-concurrent-review"))
	mustReview(t, pool, ReviewDecision{
		JobID: job.ID, Verdict: HumanVerdictSuitable,
	})
	assertBatchResult(t, mustQueueOutreach(t, pool, OutreachAuthorization{
		GreetingText: "您好", TimeDescription: "全天可打招呼",
	}, job.ID), 1, 0, "")

	start := make(chan struct{})
	reviewError := make(chan error, 1)
	claimResult := make(chan []OutreachWork, 1)
	claimError := make(chan error, 1)
	go func() {
		<-start
		reviewError <- pool.Review(t.Context(), []ReviewDecision{{
			JobID: job.ID, ExpectedJDHash: job.JDHash,
			Verdict: HumanVerdictUnsuitable,
		}})
	}()
	go func() {
		<-start
		work, err := pool.ClaimOutreach(t.Context(), OutreachClaim{
			Worker: "outreach-worker", Limit: 1,
			ClaimedAt: time.UnixMilli(2000), LeaseUntil: time.UnixMilli(3000),
		})
		claimResult <- work
		claimError <- err
	}()
	close(start)

	if err := <-reviewError; err != nil {
		t.Fatalf("concurrent review: %v", err)
	}
	work := <-claimResult
	if err := <-claimError; err != nil {
		t.Fatalf("concurrent outreach claim: %v", err)
	}
	current := mustGetJob(t, pool, job.ID)
	if current.HumanVerdict != HumanVerdictUnsuitable {
		t.Fatalf("human verdict = %q, want unsuitable", current.HumanVerdict)
	}
	assertConcurrentReviewOutcome(t, current, work)
}

func assertConcurrentReviewOutcome(t *testing.T, current JobView, work []OutreachWork) {
	t.Helper()
	switch len(work) {
	case 0:
		if current.OutreachStatus != OutreachStatusNotQueued || current.OutreachLeaseOwner != "" {
			t.Errorf("unclaimed unsuitable outreach = %#v, want atomically withdrawn", current)
		}
	case 1:
		if current.OutreachStatus != OutreachStatusProcessing || current.OutreachLeaseOwner != "outreach-worker" {
			t.Errorf("claimed outreach after unsuitable review = %#v, want in-flight work preserved", current)
		}
	default:
		t.Fatalf("claimed work count = %d, want 0 or 1", len(work))
	}
}

func TestReviewRejectsWhenTheJDSinceShownHasChanged(t *testing.T) {
	t.Parallel()

	pool, _ := openTestPool(t)
	original := mustObserve(t, pool, 1, observedJob("boss-job-stale-review-submit"))
	changed := observedJob("boss-job-stale-review-submit")
	changed.FullJD = "熟悉 Go、SQLite 与分布式事务"
	changed.ObservedAt = time.UnixMilli(2000)
	mustObserve(t, pool, 2, changed)

	err := pool.Review(t.Context(), []ReviewDecision{{
		JobID: original.ID, ExpectedJDHash: original.JDHash,
		Verdict: HumanVerdictSuitable,
	}})
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("stale review error = %v, want business rejection", err)
	}
	if rejection.Code != "platform_job_changed" {
		t.Errorf("stale review rejection = %#v, want platform_job_changed", rejection)
	}
	current := mustGetJob(t, pool, original.ID)
	if current.HumanVerdict != "" || current.HumanReviewedJDHash != "" {
		t.Errorf("stale review changed current job: %#v", current)
	}
}
