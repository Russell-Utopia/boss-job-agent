package assessment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type controlledAssessmentSubmitter struct {
	requests chan AssessmentRequest
	inspect  func(AssessmentRequest) error
	err      error
}

func (s *controlledAssessmentSubmitter) Submit(_ context.Context, request AssessmentRequest) error {
	if s.inspect != nil {
		if err := s.inspect(request); err != nil {
			return err
		}
	}
	if s.requests != nil {
		s.requests <- request
	}
	return s.err
}

func (s *controlledAssessmentSubmitter) Close(context.Context) error { return nil }

type cancelingAssessmentSubmitter struct {
	started chan struct{}
}

func (s *cancelingAssessmentSubmitter) Submit(ctx context.Context, _ AssessmentRequest) error {
	close(s.started)
	<-ctx.Done()
	return &SubmissionError{Category: SubmissionErrorTransient, Err: ctx.Err()}
}

func (s *cancelingAssessmentSubmitter) Close(context.Context) error { return nil }

type controlledResumeReader struct {
	content onlineresume.ResumeContent
}

func (r *controlledResumeReader) Read(context.Context) (onlineresume.ResumeContent, error) {
	return r.content, nil
}

func openTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, nil, nil, nil, nil, nil, time.Now), db
}

func TestDefaultPolicyIsReadyForTheFirstAssessment(t *testing.T) {
	t.Parallel()

	service, _ := openTestService(t)
	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure default policy: %v", err)
	}

	policy, err := service.GetActivePolicy(t.Context())
	if err != nil {
		t.Fatalf("get default policy: %v", err)
	}
	if policy.Version != 1 {
		t.Errorf("default policy version = %d, want 1", policy.Version)
	}
	if policy.Name != "默认策略 v1" {
		t.Errorf("default policy name = %q, want 默认策略 v1", policy.Name)
	}
	if len(policy.Rules) != 4 {
		t.Errorf("default policy rule count = %d, want 4", len(policy.Rules))
	}
}

func TestDefaultPolicyInitializationPreservesTheActiveSavedPolicy(t *testing.T) {
	t.Parallel()

	service, db := openTestService(t)
	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure default policy: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE assessment_policy_versions SET is_active = 0`); err != nil {
		t.Fatalf("deactivate default policy fixture: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (2, '{"rules":["用户保存的策略"]}', 1, '用户采用', 2000)
	`); err != nil {
		t.Fatalf("save policy fixture: %v", err)
	}

	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("ensure default policy after save: %v", err)
	}
	policy, err := service.GetActivePolicy(t.Context())
	if err != nil {
		t.Fatalf("get saved policy: %v", err)
	}
	if policy.Version != 2 {
		t.Errorf("active policy version = %d, want 2", policy.Version)
	}
	if policy.Name != "策略 v2" {
		t.Errorf("active policy name = %q, want 策略 v2", policy.Name)
	}
	if len(policy.Rules) != 1 || policy.Rules[0] != "用户保存的策略" {
		t.Errorf("active policy rules = %#v, want saved rules", policy.Rules)
	}
}

type assessmentRunFixture struct {
	service        *Service
	submitter      *controlledAssessmentSubmitter
	settings       *automationsettings.Settings
	pool           *jobpool.Pool
	db             *sql.DB
	resumeVersions *onlineresume.Versions
	resumeReader   *controlledResumeReader
	job            jobpool.JobView
	resumeContent  onlineresume.ResumeContent
	resumeVersion  int
}

func prepareQueuedAssessmentJob(t *testing.T, pool *jobpool.Pool) jobpool.JobView {
	t.Helper()
	job, err := pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: "boss-job-7", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-7.html",
		JobTitle: "Go 平台工程师", CompanyName: "示例科技", City: "福州", Salary: "25-35K",
		Responsibilities: "负责 Go 平台服务", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1_500),
	})
	if err != nil {
		t.Fatalf("observe platform job: %v", err)
	}
	queued, err := pool.QueueAssessments(t.Context(), []int64{job.ID})
	if err != nil || queued.Succeeded != 1 {
		t.Fatalf("queue assessment: result=%#v err=%v", queued, err)
	}
	return job
}

func newAssessmentRunFixture(t *testing.T) assessmentRunFixture {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(filepath.Join(t.TempDir(), "assessment.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	now := time.UnixMilli(2_000)
	resumeContent := onlineresume.ResumeContent{
		JobIntentions: []onlineresume.JobIntention{{
			Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
		}},
		WorkExperiences:    []string{"后端工程师｜负责 Go 服务"},
		ProjectExperiences: []string{"招聘助手"},
		Educations:         []string{"计算机本科"},
		Skills:             []string{"Go", "SQLite"},
	}
	resumeReader := &controlledResumeReader{content: resumeContent}
	resumeVersions := onlineresume.New(
		db,
		resumeReader,
		logs,
		func() time.Time { return time.UnixMilli(1_000) },
	)
	refreshed, err := resumeVersions.RefreshFromBoss(t.Context())
	if err != nil {
		t.Fatalf("refresh online resume: %v", err)
	}
	pool := jobpool.New(db)
	settings := automationsettings.New(db, pool)
	if err := settings.EnsureSafeDefaults(t.Context(), time.UnixMilli(1_000)); err != nil {
		t.Fatalf("ensure safe automation settings: %v", err)
	}
	job := prepareQueuedAssessmentJob(t, pool)
	before, err := pool.GetJobDetail(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("get pending assessment: %v", err)
	}
	if before.AssessmentInputs != (jobpool.AssessmentInputVersions{}) {
		t.Fatalf("pending assessment inputs = %#v, want none", before.AssessmentInputs)
	}

	submitter := &controlledAssessmentSubmitter{requests: make(chan AssessmentRequest, 1)}
	service := New(db, resumeVersions, pool, settings, submitter, logs, func() time.Time { return now })
	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(1_000)); err != nil {
		t.Fatalf("ensure default policy: %v", err)
	}
	submitter.inspect = func(request AssessmentRequest) error {
		claimed, getErr := pool.GetJob(t.Context(), job.ID)
		if getErr != nil {
			return getErr
		}
		if claimed.AssessmentStatus != jobpool.AssessmentStatusProcessing {
			t.Fatalf("status during Adapter call = %q, want processing", claimed.AssessmentStatus)
		}
		return nil
	}
	return assessmentRunFixture{
		service: service, submitter: submitter, settings: settings, pool: pool, db: db,
		resumeVersions: resumeVersions, resumeReader: resumeReader, job: job,
		resumeContent: resumeContent, resumeVersion: refreshed.Current.Version,
	}
}

func runAssessmentOnce(t *testing.T, fixture assessmentRunFixture) AssessmentRequest {
	t.Helper()
	if err := fixture.service.runSchedulingCycle(t.Context(), time.UnixMilli(2_000)); err != nil {
		t.Fatalf("submit pending assessment: %v", err)
	}
	return <-fixture.submitter.requests
}

func assertCompleteAssessmentRequest(t *testing.T, fixture assessmentRunFixture, request AssessmentRequest) {
	t.Helper()
	if request.TraceID == "" {
		t.Fatal("assessment trace ID is empty")
	}
	if !reflect.DeepEqual(request.Resume, fixture.resumeContent) {
		t.Errorf("submitted resume = %#v, want %#v", request.Resume, fixture.resumeContent)
	}
	if request.ResumeVersion != fixture.resumeVersion {
		t.Errorf("submitted resume version = %d, want %d", request.ResumeVersion, fixture.resumeVersion)
	}
	if request.Policy.Version != 1 || len(request.Policy.Rules) != 4 {
		t.Errorf("submitted policy = %#v, want complete default v1", request.Policy)
	}
	if len(request.Jobs) != 1 {
		t.Fatalf("submitted jobs = %#v, want one", request.Jobs)
	}
	input := request.Jobs[0]
	expectedInput := AssessmentJobInput{
		JobID: fixture.job.ID, AttemptNo: 1, PlatformJobID: fixture.job.PlatformJobID,
		CanonicalURL: fixture.job.CanonicalURL, JobTitle: fixture.job.JobTitle,
		CompanyName: fixture.job.CompanyName, City: fixture.job.City, Salary: fixture.job.Salary,
		Responsibilities: fixture.job.Responsibilities, Requirements: fixture.job.Requirements,
		JDHash: fixture.job.JDHash,
	}
	if !reflect.DeepEqual(input, expectedInput) {
		t.Errorf("submitted job input = %#v, want %#v", input, expectedInput)
	}
}

func TestServiceSubmitsPendingAssessmentWithTheCompleteCurrentInputsOutsideTheClaimTransaction(t *testing.T) {
	t.Parallel()

	fixture := newAssessmentRunFixture(t)
	request := runAssessmentOnce(t, fixture)
	assertCompleteAssessmentRequest(t, fixture, request)
}

func TestFailedPiSubmissionFinishesClaimedJobsAndSchedulesAutomaticRetry(t *testing.T) {
	t.Parallel()

	fixture := newAssessmentRunFixture(t)
	fixture.submitter.requests = nil
	fixture.submitter.err = &SubmissionError{
		Category: SubmissionErrorTransient,
		Err:      errors.New("controlled Pi failure"),
	}

	if err := fixture.service.runSchedulingCycle(t.Context(), time.UnixMilli(2_000)); err != nil {
		t.Fatalf("handle failed Pi submission: %v", err)
	}
	job, err := fixture.pool.GetJob(t.Context(), fixture.job.ID)
	if err != nil {
		t.Fatalf("get failed assessment job: %v", err)
	}
	if job.AssessmentStatus != jobpool.AssessmentStatusFailed || job.AssessmentLeaseOwner != "" || job.AssessmentLeaseUntil != nil {
		t.Errorf("job after failed Pi submission = %#v, want failed with released lease", job)
	}
	var failureCount int
	var retryAt sql.NullInt64
	if err := fixture.db.QueryRowContext(t.Context(), `
		SELECT assessment_consecutive_failure_count, assessment_retry_at
		FROM platform_jobs WHERE id = ?
	`, fixture.job.ID).Scan(&failureCount, &retryAt); err != nil {
		t.Fatalf("read failed assessment retry state: %v", err)
	}
	if failureCount != 1 || !retryAt.Valid || retryAt.Int64 <= 2_000 {
		t.Errorf("failed assessment retry state = count %d, retry %#v; want one failure and future retry", failureCount, retryAt)
	}
}

func TestInvalidPiProtocolFailureRequiresManualRetry(t *testing.T) {
	t.Parallel()

	fixture := newAssessmentRunFixture(t)
	fixture.submitter.requests = nil
	fixture.submitter.err = &SubmissionError{
		Category: SubmissionErrorInvalidProtocol,
		Err:      errors.New("controlled invalid Pi protocol"),
	}

	if err := fixture.service.runSchedulingCycle(t.Context(), time.UnixMilli(2_000)); err != nil {
		t.Fatalf("handle invalid Pi protocol: %v", err)
	}
	var status string
	var retryAt sql.NullInt64
	if err := fixture.db.QueryRowContext(t.Context(), `
		SELECT assessment_status, assessment_retry_at
		FROM platform_jobs WHERE id = ?
	`, fixture.job.ID).Scan(&status, &retryAt); err != nil {
		t.Fatalf("read invalid protocol failure: %v", err)
	}
	if status != string(jobpool.AssessmentStatusFailed) || retryAt.Valid {
		t.Errorf("invalid protocol state = status %q, retry %#v; want failed without automatic retry", status, retryAt)
	}
}

func TestCanceledPiSubmissionStillFinishesTheClaimWithABoundedCleanupContext(t *testing.T) {
	t.Parallel()

	fixture := newAssessmentRunFixture(t)
	submitter := &cancelingAssessmentSubmitter{started: make(chan struct{})}
	fixture.service.submitter = submitter
	runContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	cycleResult := make(chan error, 1)
	go func() {
		cycleResult <- fixture.service.runSchedulingCycle(runContext, time.UnixMilli(2_000))
	}()
	<-submitter.started
	cancel()
	if err := <-cycleResult; err != nil {
		t.Fatalf("finish canceled Pi submission: %v", err)
	}

	job, err := fixture.pool.GetJob(t.Context(), fixture.job.ID)
	if err != nil {
		t.Fatalf("get assessment after canceled submission: %v", err)
	}
	if job.AssessmentStatus != jobpool.AssessmentStatusFailed || job.AssessmentLeaseOwner != "" {
		t.Errorf("job after canceled submission = %#v, want failed with released lease", job)
	}
}

func TestPendingAssessmentUsesTheResumeAndPolicyCurrentWhenItIsActuallyClaimed(t *testing.T) {
	t.Parallel()

	fixture := newAssessmentRunFixture(t)
	if err := fixture.settings.ConfigureAssessment(t.Context(), false, 1); err != nil {
		t.Fatalf("configure initial assessment limit: %v", err)
	}
	firstRequest := runAssessmentOnce(t, fixture)
	if firstRequest.ResumeVersion != 1 || firstRequest.Policy.Version != 1 {
		t.Fatalf("first assessment inputs = resume v%d, policy v%d; want v1 and v1", firstRequest.ResumeVersion, firstRequest.Policy.Version)
	}
	secondJob := preparePendingAssessmentWithNewCurrentInputs(t, fixture)

	secondRequest := runAssessmentOnce(t, fixture)
	if secondRequest.ResumeVersion != 2 || secondRequest.Policy.Version != 2 || len(secondRequest.Jobs) != 1 || secondRequest.Jobs[0].JobID != secondJob.ID {
		t.Errorf("second assessment request = %#v, want pending job with resume v2 and policy v2", secondRequest)
	}
	assertRecordedAssessmentInputVersions(t, fixture, secondJob)
}

func preparePendingAssessmentWithNewCurrentInputs(
	t *testing.T,
	fixture assessmentRunFixture,
) jobpool.JobView {
	t.Helper()
	secondJob, err := fixture.pool.Observe(t.Context(), 2, jobpool.Observation{
		PlatformJobID: "boss-pending-current-input", CanonicalURL: "https://www.zhipin.com/job_detail/boss-pending-current-input.html",
		JobTitle: "Go 后端工程师", CompanyName: "另一科技", City: "福州", Salary: "25-35K",
		Responsibilities: "负责新服务", Requirements: "熟悉 Go",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(2_100),
	})
	if err != nil {
		t.Fatalf("observe second assessment job: %v", err)
	}
	queued, err := fixture.pool.QueueAssessments(t.Context(), []int64{secondJob.ID})
	if err != nil || queued.Succeeded != 1 {
		t.Fatalf("queue second assessment: result=%#v err=%v", queued, err)
	}
	activateSecondAssessmentInputs(t, fixture)
	return secondJob
}

func activateSecondAssessmentInputs(t *testing.T, fixture assessmentRunFixture) {
	t.Helper()
	fixture.resumeReader.content.Skills = append(fixture.resumeReader.content.Skills, "Kafka")
	refreshed, err := fixture.resumeVersions.RefreshFromBoss(t.Context())
	if err != nil || refreshed.Current.Version != 2 {
		t.Fatalf("refresh resume before pending claim: result=%#v err=%v", refreshed, err)
	}
	if _, err := fixture.db.ExecContext(t.Context(), `UPDATE assessment_policy_versions SET is_active = 0`); err != nil {
		t.Fatalf("deactivate first policy: %v", err)
	}
	if _, err := fixture.db.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (2, '{"rules":["采用新策略"]}', 1, '用户采用', 2200)
	`); err != nil {
		t.Fatalf("save second policy: %v", err)
	}
	if err := fixture.settings.ConfigureAssessment(t.Context(), false, 2); err != nil {
		t.Fatalf("increase assessment processing limit: %v", err)
	}
}

func assertRecordedAssessmentInputVersions(
	t *testing.T,
	fixture assessmentRunFixture,
	secondJob jobpool.JobView,
) {
	t.Helper()
	firstDetail, err := fixture.pool.GetJobDetail(t.Context(), fixture.job.ID)
	if err != nil {
		t.Fatalf("get first processing assessment: %v", err)
	}
	secondDetail, err := fixture.pool.GetJobDetail(t.Context(), secondJob.ID)
	if err != nil {
		t.Fatalf("get second processing assessment: %v", err)
	}
	if firstDetail.AssessmentInputs.ResumeVersion != 1 || firstDetail.AssessmentInputs.PolicyVersion != 1 {
		t.Errorf("first processing inputs changed = %#v, want resume v1 and policy v1", firstDetail.AssessmentInputs)
	}
	if secondDetail.AssessmentInputs.ResumeVersion != 2 || secondDetail.AssessmentInputs.PolicyVersion != 2 {
		t.Errorf("second claimed inputs = %#v, want resume v2 and policy v2", secondDetail.AssessmentInputs)
	}
}

func TestSchedulingUsesAutomaticAdmissionAndTheCurrentGlobalProcessingLimit(t *testing.T) {
	t.Parallel()

	fixture := newAssessmentRunFixture(t)
	if err := fixture.settings.ConfigureAssessment(t.Context(), true, 7); err != nil {
		t.Fatalf("enable automatic assessment: %v", err)
	}
	automaticJobs := observeAutomaticAssessmentJobs(t, fixture, 7)

	request := runAssessmentOnce(t, fixture)
	if len(request.Jobs) != 7 {
		t.Fatalf("submitted assessment jobs = %d, want current processing limit 7 without a default cap of 5", len(request.Jobs))
	}
	jobIDs := assessmentJobIDs(fixture.job, automaticJobs)
	processing, pending := countAssessmentStates(t, fixture.pool, jobIDs)
	if processing != 7 || pending != 1 {
		t.Errorf("scheduled assessment states = processing %d, pending %d; want 7 and 1", processing, pending)
	}

	if err := fixture.settings.ConfigureAssessment(t.Context(), false, 1); err != nil {
		t.Fatalf("lower and disable automatic assessment: %v", err)
	}
	if err := fixture.service.runSchedulingCycle(t.Context(), time.UnixMilli(3_000)); err != nil {
		t.Fatalf("run after lowering assessment limit: %v", err)
	}
	assertNoAssessmentRequest(t, fixture.submitter)
	assertNoJobReturnedToNotQueued(t, fixture.pool, jobIDs)
}

func observeAutomaticAssessmentJobs(
	t *testing.T,
	fixture assessmentRunFixture,
	count int,
) []jobpool.JobView {
	t.Helper()
	jobs := make([]jobpool.JobView, count)
	for index := range jobs {
		job, err := fixture.pool.Observe(t.Context(), int64(index+2), jobpool.Observation{
			PlatformJobID: fmt.Sprintf("boss-automatic-%d", index+1),
			CanonicalURL:  fmt.Sprintf("https://www.zhipin.com/job_detail/boss-automatic-%d.html", index+1),
			JobTitle:      fmt.Sprintf("Go 自动鉴定工程师 %d", index+1), CompanyName: "示例科技",
			City: "福州", Salary: "20-30K", Responsibilities: "负责 Go 服务",
			Requirements: "熟悉 Go", PlatformStatus: jobpool.PlatformStatusOpen,
			ObservedAt: time.UnixMilli(int64(1_600 + index)),
		})
		if err != nil {
			t.Fatalf("observe automatic assessment job %d: %v", index+1, err)
		}
		jobs[index] = job
	}
	return jobs
}

func assessmentJobIDs(firstJob jobpool.JobView, remainingJobs []jobpool.JobView) []int64 {
	jobIDs := []int64{firstJob.ID}
	for _, job := range remainingJobs {
		jobIDs = append(jobIDs, job.ID)
	}
	return jobIDs
}

func countAssessmentStates(t *testing.T, pool *jobpool.Pool, jobIDs []int64) (int, int) {
	t.Helper()
	processing, pending := 0, 0
	for _, jobID := range jobIDs {
		job, err := pool.GetJob(t.Context(), jobID)
		if err != nil {
			t.Fatalf("get scheduled job %d: %v", jobID, err)
		}
		switch job.AssessmentStatus {
		case jobpool.AssessmentStatusProcessing:
			processing++
		case jobpool.AssessmentStatusPending:
			pending++
		}
	}
	return processing, pending
}

func assertNoAssessmentRequest(t *testing.T, submitter *controlledAssessmentSubmitter) {
	t.Helper()
	select {
	case unexpected := <-submitter.requests:
		t.Errorf("submitted more work after lowering below active count: %#v", unexpected)
	default:
	}
}

func assertNoJobReturnedToNotQueued(t *testing.T, pool *jobpool.Pool, jobIDs []int64) {
	t.Helper()
	for _, jobID := range jobIDs {
		job, err := pool.GetJob(t.Context(), jobID)
		if err != nil {
			t.Fatalf("get job after lowering limit %d: %v", jobID, err)
		}
		if job.AssessmentStatus == jobpool.AssessmentStatusNotQueued {
			t.Errorf("job %d returned to not_queued after disabling automatic assessment", jobID)
		}
	}
}

func prepareClaimedAssessmentJobs(
	t *testing.T,
	db *sql.DB,
	pool *jobpool.Pool,
	count int,
) []jobpool.JobView {
	t.Helper()
	resumeID, policyID := seedAssessmentInputVersions(t, db)
	jobs := make([]jobpool.JobView, count)
	for index := range jobs {
		job, err := pool.Observe(t.Context(), 1, jobpool.Observation{
			PlatformJobID: fmt.Sprintf("confirm-job-%d", index+1),
			CanonicalURL:  fmt.Sprintf("https://www.zhipin.com/job_detail/confirm-job-%d.html", index+1),
			JobTitle:      fmt.Sprintf("Go 工程师 %d", index+1), CompanyName: "示例科技",
			City: "福州", Salary: "20-30K", Responsibilities: "负责 Go 服务",
			Requirements: "熟悉 Go", PlatformStatus: jobpool.PlatformStatusOpen,
			ObservedAt: time.UnixMilli(int64(1_000 + index)),
		})
		if err != nil {
			t.Fatalf("observe job %d: %v", index+1, err)
		}
		jobs[index] = job
	}
	jobIDs := make([]int64, len(jobs))
	for index := range jobs {
		jobIDs[index] = jobs[index].ID
	}
	queued, err := pool.QueueAssessments(t.Context(), jobIDs)
	if err != nil || queued.Succeeded != len(jobs) {
		t.Fatalf("queue assessments: result=%#v err=%v", queued, err)
	}
	work, err := pool.ClaimAssessments(t.Context(), jobpool.AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: assessmentEvaluatorVersion, ProcessingLimit: len(jobs),
		ClaimedAt: time.UnixMilli(2_000), LeaseUntil: time.UnixMilli(12_000),
	})
	if err != nil || len(work) != len(jobs) {
		t.Fatalf("claim assessments: work=%#v err=%v", work, err)
	}
	return jobs
}

func assertConfirmationStatuses(
	t *testing.T,
	receipt ConfirmationReceipt,
	wantStatuses []ConfirmationItemStatus,
) {
	t.Helper()
	if len(receipt.Results) != len(wantStatuses) {
		t.Fatalf("confirmation receipt = %#v", receipt)
	}
	for index, want := range wantStatuses {
		if receipt.Results[index].Status != want {
			t.Errorf("receipt %d = %#v, want status %q", index, receipt.Results[index], want)
		}
	}
}

func TestConfirmAcceptsThreeSuggestionsWithoutLettingInvalidOrStaleItemsHideThem(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(filepath.Join(t.TempDir(), "assessment-confirm.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	pool := jobpool.New(db)
	service := New(db, nil, pool, nil, nil, logs, func() time.Time { return time.UnixMilli(3_000) })
	jobs := prepareClaimedAssessmentJobs(t, db, pool, 5)

	receipt, err := service.Confirm(t.Context(), ConfirmationBatch{Results: []AssessmentConfirmation{
		{JobID: jobs[0].ID, AttemptNo: 1, Status: jobpool.AssessmentStatusSuitable,
			Reason: "经历与职责明确匹配", Evidence: json.RawMessage(`{"matches":["Go 服务"]}`)},
		{JobID: jobs[1].ID, AttemptNo: 1, Status: jobpool.AssessmentStatusUnsuitable,
			Reason: "岗位明确要求纯 Java", Evidence: json.RawMessage(`{"mismatches":["纯 Java"]}`)},
		{JobID: jobs[2].ID, AttemptNo: 1, Status: jobpool.AssessmentStatusNeedsUserConfirmation,
			Reason: "关键业务经验信息不足", Evidence: json.RawMessage(`{"uncertain":["业务经验"]}`)},
		{JobID: jobs[3].ID, AttemptNo: 1, Status: jobpool.AssessmentStatusSuitable,
			Reason: "", Evidence: json.RawMessage(`{"matches":["Go"]}`)},
		{JobID: jobs[4].ID, AttemptNo: 2, Status: jobpool.AssessmentStatusSuitable,
			Reason: "迟到结果", Evidence: json.RawMessage(`{"matches":["Go"]}`)},
	}})
	if err != nil {
		t.Fatalf("confirm mixed assessment batch: %v", err)
	}
	wantReceipts := []ConfirmationItemStatus{
		ConfirmationAccepted, ConfirmationAccepted, ConfirmationAccepted,
		ConfirmationInvalid, ConfirmationStale,
	}
	assertConfirmationStatuses(t, receipt, wantReceipts)
	assertAssessmentStatus(t, pool, jobs[0].ID, jobpool.AssessmentStatusSuitable)
	assertAssessmentStatus(t, pool, jobs[1].ID, jobpool.AssessmentStatusUnsuitable)
	assertAssessmentStatus(t, pool, jobs[2].ID, jobpool.AssessmentStatusNeedsUserConfirmation)
	assertAssessmentStatus(t, pool, jobs[3].ID, jobpool.AssessmentStatusProcessing)
	assertAssessmentStatus(t, pool, jobs[4].ID, jobpool.AssessmentStatusProcessing)

	corrected, err := service.Confirm(t.Context(), ConfirmationBatch{Results: []AssessmentConfirmation{
		{JobID: jobs[3].ID, AttemptNo: 1, Status: jobpool.AssessmentStatusSuitable,
			Reason: "Go 经历匹配", Evidence: json.RawMessage(`{"matches":["Go"]}`)},
		{JobID: jobs[4].ID, AttemptNo: 1, Status: jobpool.AssessmentStatusSuitable,
			Reason: "Go 经历匹配", Evidence: json.RawMessage(`{"matches":["Go"]}`)},
	}})
	if err != nil || len(corrected.Results) != 2 ||
		corrected.Results[0].Status != ConfirmationAccepted || corrected.Results[1].Status != ConfirmationAccepted {
		t.Fatalf("corrected confirmation = %#v err=%v", corrected, err)
	}
}

func seedAssessmentInputVersions(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	resumeResult, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (1, '{"jobIntentions":[],"workExperiences":[],"projectExperiences":[],"educations":[],"skills":[]}', 'resume-1', 1, 1000)
	`)
	if err != nil {
		t.Fatalf("seed online resume version: %v", err)
	}
	resumeID, err := resumeResult.LastInsertId()
	if err != nil {
		t.Fatalf("read online resume ID: %v", err)
	}
	policyResult, err := db.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (1, '{"rules":["只依据输入"]}', 1, 'test', 1000)
	`)
	if err != nil {
		t.Fatalf("seed assessment policy version: %v", err)
	}
	policyID, err := policyResult.LastInsertId()
	if err != nil {
		t.Fatalf("read assessment policy ID: %v", err)
	}
	return resumeID, policyID
}

func assertAssessmentStatus(t *testing.T, pool *jobpool.Pool, jobID int64, want jobpool.AssessmentStatus) {
	t.Helper()
	job, err := pool.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatalf("get platform job %d: %v", jobID, err)
	}
	if job.AssessmentStatus != want {
		t.Errorf("job %d assessment status = %q, want %q", jobID, job.AssessmentStatus, want)
	}
}
