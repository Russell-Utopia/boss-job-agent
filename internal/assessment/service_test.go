package assessment

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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
	return New(db, nil, nil, nil, nil, time.Now), db
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
	service       *Service
	submitter     *controlledAssessmentSubmitter
	job           jobpool.JobView
	resumeContent onlineresume.ResumeContent
	resumeVersion int
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
	resumeVersions := onlineresume.New(
		db,
		&controlledResumeReader{content: resumeContent},
		logs,
		func() time.Time { return time.UnixMilli(1_000) },
	)
	refreshed, err := resumeVersions.RefreshFromBoss(t.Context())
	if err != nil {
		t.Fatalf("refresh online resume: %v", err)
	}
	pool := jobpool.New(db)
	job := prepareQueuedAssessmentJob(t, pool)
	before, err := pool.GetJobDetail(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("get pending assessment: %v", err)
	}
	if before.AssessmentInputs != (jobpool.AssessmentInputVersions{}) {
		t.Fatalf("pending assessment inputs = %#v, want none", before.AssessmentInputs)
	}

	submitter := &controlledAssessmentSubmitter{requests: make(chan AssessmentRequest, 1)}
	service := New(db, resumeVersions, pool, submitter, logs, func() time.Time { return now })
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
		service: service, submitter: submitter, job: job,
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
		EvaluatorVersion: assessmentEvaluatorVersion, Limit: len(jobs),
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
	service := New(db, nil, pool, nil, logs, func() time.Time { return time.UnixMilli(3_000) })
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
