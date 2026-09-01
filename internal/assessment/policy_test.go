package assessment

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type controlledPolicyAdvisor struct {
	generate  func(PolicyGenerationRequest) (PolicyDraft, error)
	validate  func(PolicyValidationRequest) (PolicyValidationResult, error)
	generates int
	validates int
}

func (a *controlledPolicyAdvisor) Generate(_ context.Context, request PolicyGenerationRequest) (PolicyDraft, error) {
	a.generates++
	if a.generate == nil {
		return PolicyDraft{Text: "新的规则"}, nil
	}
	return a.generate(request)
}

func (a *controlledPolicyAdvisor) Validate(_ context.Context, request PolicyValidationRequest) (PolicyValidationResult, error) {
	a.validates++
	if a.validate == nil {
		return PolicyValidationResult{}, nil
	}
	return a.validate(request)
}

func openPolicyService(t *testing.T, advisor PolicyAdvisor) (*Service, *jobpool.Pool, *sql.DB, *onlineresume.Versions) {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(t.TempDir() + "/assessment.jsonl")
	t.Cleanup(func() { _ = logs.Close() })
	resumeReader := &controlledResumeReader{content: onlineresume.ResumeContent{
		JobIntentions:   []onlineresume.JobIntention{{Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职"}},
		WorkExperiences: []string{"负责 Go 服务"}, ProjectExperiences: []string{"招聘助手"},
		Educations: []string{"计算机本科"}, Skills: []string{"Go", "SQLite"},
	}}
	resumes := onlineresume.New(db, resumeReader, logs, func() time.Time { return time.UnixMilli(1000) })
	if _, err := resumes.RefreshFromBoss(t.Context()); err != nil {
		t.Fatalf("save online resume: %v", err)
	}
	pool := jobpool.New(db)
	service := New(db, resumes, pool, nil, nil, advisor, logs, func() time.Time {
		return time.UnixMilli(2000)
	})
	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure default policy: %v", err)
	}
	return service, pool, db, resumes
}

func policySampleJob(t *testing.T, pool *jobpool.Pool, id string) jobpool.JobView {
	t.Helper()
	job, err := pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: id, CanonicalURL: "https://www.zhipin.com/job_detail/" + id + ".html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1500),
	})
	if err != nil {
		t.Fatalf("observe policy sample: %v", err)
	}
	return job
}

func reviewPolicySample(t *testing.T, pool *jobpool.Pool, job jobpool.JobView, verdict jobpool.HumanVerdict) {
	t.Helper()
	if err := pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: job.ID, ExpectedJDHash: job.JDHash, Verdict: verdict, Note: "监督标注",
	}}); err != nil {
		t.Fatalf("review policy sample: %v", err)
	}
}

func TestGeneratePolicyDraftFreezesCurrentInputsAndSelectedCompleteSamples(t *testing.T) {
	t.Parallel()

	var received PolicyGenerationRequest
	advisor := &controlledPolicyAdvisor{
		generate: func(request PolicyGenerationRequest) (PolicyDraft, error) {
			received = request
			return PolicyDraft{Text: "明确匹配时通过\n信息不足时人工确认"}, nil
		},
	}
	service, pool, _, resumes := openPolicyService(t, advisor)
	first := policySampleJob(t, pool, "policy-generate-1")
	second := policySampleJob(t, pool, "policy-generate-2")
	reviewPolicySample(t, pool, first, jobpool.HumanVerdictSuitable)
	reviewPolicySample(t, pool, second, jobpool.HumanVerdictUnsuitable)

	draft, err := service.GeneratePolicyDraft(t.Context(), []int64{second.ID})
	if err != nil {
		t.Fatalf("generate policy draft: %v", err)
	}
	current, err := resumes.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current resume: %v", err)
	}
	assertPolicyDraftMetadata(t, draft, advisor.generates, current.Version)
	assertPolicyGenerationRequest(t, received, current.Content, current.Version, second.ID)
}

func assertPolicyDraftMetadata(t *testing.T, draft PolicyDraft, generateCalls, resumeVersion int) {
	t.Helper()
	if generateCalls != 1 || draft.Text == "" || draft.ResumeVersion != resumeVersion || draft.PolicyVersion != 1 || draft.GenerationSampleCount != 1 || !draft.GeneratedAt.Equal(time.UnixMilli(2000)) {
		t.Errorf("draft metadata = %#v, advisor calls = %d", draft, generateCalls)
	}
}

func assertPolicyGenerationRequest(t *testing.T, request PolicyGenerationRequest, resume onlineresume.ResumeContent, resumeVersion int, jobID int64) {
	t.Helper()
	if !reflect.DeepEqual(request.Resume, resume) {
		t.Errorf("generation resume = %#v, want %#v", request.Resume, resume)
	}
	if request.ResumeVersion != resumeVersion || request.Policy.Version != 1 || len(request.Samples) != 1 {
		t.Errorf("generation request metadata = %#v", request)
	}
	if request.Samples[0].JobID != jobID || request.Samples[0].Verdict != jobpool.HumanVerdictUnsuitable {
		t.Errorf("generation sample = %#v", request.Samples)
	}
}

func TestValidatePolicyDraftComparesAllSamplesAndDoesNotTouchPlatformJobs(t *testing.T) {
	t.Parallel()

	var received PolicyValidationRequest
	advisor := &controlledPolicyAdvisor{
		generate: func(PolicyGenerationRequest) (PolicyDraft, error) {
			return PolicyDraft{Text: "候选规则"}, nil
		},
		validate: func(request PolicyValidationRequest) (PolicyValidationResult, error) {
			received = request
			return PolicyValidationResult{Results: []PolicyValidationComparison{
				{JobID: 1, CurrentStatus: jobpool.AssessmentStatusNeedsUserConfirmation, CandidateStatus: jobpool.AssessmentStatusUnsuitable},
				{JobID: 2, CurrentStatus: jobpool.AssessmentStatusSuitable, CandidateStatus: jobpool.AssessmentStatusSuitable},
			}}, nil
		},
	}
	service, pool, db, _ := openPolicyService(t, advisor)
	first := policySampleJob(t, pool, "policy-validate-1")
	second := policySampleJob(t, pool, "policy-validate-2")
	reviewPolicySample(t, pool, first, jobpool.HumanVerdictUnsuitable)
	reviewPolicySample(t, pool, second, jobpool.HumanVerdictSuitable)
	draft, err := service.GeneratePolicyDraft(t.Context(), []int64{first.ID})
	if err != nil {
		t.Fatalf("generate policy draft: %v", err)
	}
	draft.ValidationEnabled = true
	report, err := service.ValidatePolicyDraft(t.Context(), draft)
	if err != nil {
		t.Fatalf("validate policy draft: %v", err)
	}
	assertPolicyValidationReport(t, report, advisor.validates)
	assertPolicyValidationRequest(t, received, draft, first.ID, second.ID)
	assertUnchangedPolicyJob(t, db, first.ID)
}

func assertPolicyValidationReport(t *testing.T, report PolicyValidationReport, validateCalls int) {
	t.Helper()
	if validateCalls != 1 {
		t.Errorf("advisor calls = %d, want 1", validateCalls)
	}
	if report.Status != PolicyValidationPassed {
		t.Errorf("validation status = %q", report.Status)
	}
	if len(report.FullResults) != 2 || len(report.UngeneratedResults) != 1 {
		t.Errorf("validation result counts = %d/%d", len(report.FullResults), len(report.UngeneratedResults))
	}
}

func assertPolicyValidationRequest(t *testing.T, request PolicyValidationRequest, draft PolicyDraft, firstID, secondID int64) {
	t.Helper()
	if len(request.Samples) != 2 || request.Samples[0].JobID != firstID || request.Samples[1].JobID != secondID {
		t.Errorf("validation samples = %#v", request.Samples)
	}
	if len(request.GenerationSampleIDs) != 1 || request.GenerationSampleIDs[0] != firstID {
		t.Errorf("generation sample IDs = %#v", request.GenerationSampleIDs)
	}
	if request.ResumeVersion != draft.ResumeVersion || request.Policy.Version != draft.PolicyVersion {
		t.Errorf("validation request metadata = %#v", request)
	}
}

func assertUnchangedPolicyJob(t *testing.T, db *sql.DB, jobID int64) {
	t.Helper()
	var status string
	if err := db.QueryRowContext(t.Context(), `SELECT assessment_status FROM platform_jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
		t.Fatalf("read unchanged platform job: %v", err)
	}
	if status != string(jobpool.AssessmentStatusNotQueued) {
		t.Errorf("platform job assessment status = %q, want unchanged not_queued", status)
	}
}

func TestCreatePolicyVersionAddsImmutableActiveVersion(t *testing.T) {
	t.Parallel()

	service, _, db, _ := openPolicyService(t, nil)
	versionID, err := service.CreatePolicyVersion(t.Context(), []string{"新的完整规则", "信息不足时人工确认"}, "用户采用策略候选稿")
	if err != nil {
		t.Fatalf("create policy version: %v", err)
	}
	policy, err := service.GetActivePolicy(t.Context())
	if err != nil {
		t.Fatalf("get adopted policy: %v", err)
	}
	if policy.ID != versionID || policy.Version != 2 || !reflect.DeepEqual(policy.Rules, []string{"新的完整规则", "信息不足时人工确认"}) {
		t.Errorf("adopted policy = %#v, want new active v2", policy)
	}
	var oldRules string
	if err := db.QueryRowContext(t.Context(), `SELECT rules_json FROM assessment_policy_versions WHERE version_no = 1`).Scan(&oldRules); err != nil {
		t.Fatalf("read immutable old policy: %v", err)
	}
	if oldRules != defaultPolicyJSON {
		var oldDocument struct {
			Rules []string `json:"rules"`
		}
		if err := json.Unmarshal([]byte(oldRules), &oldDocument); err != nil || len(oldDocument.Rules) != 4 {
			t.Errorf("old policy changed = %q", oldRules)
		}
	}
}
