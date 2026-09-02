//go:build live

package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestPolicyLiveGeneratesAndValidatesOneCompleteRequest(t *testing.T) {
	if os.Getenv("PI_POLICY_LIVE") != "1" {
		t.Skip("set PI_POLICY_LIVE=1 to run the real Pi policy probe")
	}
	adapter := New(nil)
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	request := assessment.PolicyGenerationRequest{
		ResumeVersion: 1,
		Resume: onlineresume.ResumeContent{
			JobIntentions:      []onlineresume.JobIntention{{Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职"}},
			WorkExperiences:    []string{"三年 Go 后端开发经验"},
			ProjectExperiences: []string{"使用 Go 和 SQLite 构建本地服务"},
			Educations:         []string{"计算机本科"},
			Skills:             []string{"Go", "SQLite"},
		},
		Policy: assessment.Policy{Version: 1, Name: "live 策略", Rules: []string{"只依据完整输入"}},
		Samples: []jobpool.HumanReviewSample{
			{JobID: 1, PlatformJobID: "live-go", CanonicalURL: "https://example.invalid/live-go", JobTitle: "Go 后端工程师", CompanyName: "合成测试公司", City: "福州", Salary: "20-30K", FullJD: "使用 Go 开发服务\n熟悉 Go", JDHash: "live-go-jd", Verdict: jobpool.HumanVerdictSuitable},
			{JobID: 2, PlatformJobID: "live-java", CanonicalURL: "https://example.invalid/live-java", JobTitle: "Java 工程师", CompanyName: "合成测试公司", City: "福州", Salary: "20-30K", FullJD: "维护 Java 服务\n熟悉 Java", JDHash: "live-java-jd", Verdict: jobpool.HumanVerdictUnsuitable},
		},
	}
	assertCompletePolicyLiveRequest(t, request)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	draft, err := adapter.Generate(ctx, request)
	if err != nil {
		t.Fatalf("generate live Pi policy: %v", err)
	}
	if strings.TrimSpace(draft.Text) == "" {
		t.Fatal("live Pi policy generation returned an empty draft")
	}
	validation, err := adapter.Validate(ctx, assessment.PolicyValidationRequest{
		Resume: request.Resume, ResumeVersion: request.ResumeVersion, Policy: request.Policy,
		Candidate: assessment.Policy{Version: 0, Name: "候选策略", Rules: []string{draft.Text}},
		Samples:   request.Samples, GenerationSampleIDs: []int64{1},
	})
	if err != nil {
		t.Fatalf("validate live Pi policy: %v", err)
	}
	if len(validation.Results) != len(request.Samples) {
		t.Fatalf("live Pi policy validation results = %d, want %d", len(validation.Results), len(request.Samples))
	}
	seen := make(map[int64]struct{}, len(validation.Results))
	for _, result := range validation.Results {
		if result.JobID != 1 && result.JobID != 2 {
			t.Fatalf("live Pi policy returned unexpected job ID: %#v", result)
		}
		if _, duplicate := seen[result.JobID]; duplicate {
			t.Fatalf("live Pi policy returned duplicate job ID: %d", result.JobID)
		}
		seen[result.JobID] = struct{}{}
		if !validPolicyLiveStatus(result.CurrentStatus) || !validPolicyLiveStatus(result.CandidateStatus) {
			t.Fatalf("live Pi policy returned invalid status: %#v", result)
		}
	}
	for _, sample := range request.Samples {
		if _, ok := seen[sample.JobID]; !ok {
			t.Fatalf("live Pi policy omitted job ID: %d", sample.JobID)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = adapter.Generate(canceled, request)
	var classified *assessment.PolicyAdvisorError
	if !errors.As(err, &classified) || classified.Category != "transient" {
		t.Fatalf("canceled live Pi policy error = %v, want transient classification", err)
	}
}

func assertCompletePolicyLiveRequest(t *testing.T, request assessment.PolicyGenerationRequest) {
	t.Helper()
	if request.ResumeVersion <= 0 || len(request.Resume.JobIntentions) == 0 || len(request.Resume.WorkExperiences) == 0 || len(request.Resume.ProjectExperiences) == 0 || len(request.Resume.Educations) == 0 || len(request.Resume.Skills) == 0 {
		t.Fatal("live policy request does not contain a complete resume")
	}
	if request.Policy.Version <= 0 || len(request.Policy.Rules) == 0 || len(request.Samples) < 2 {
		t.Fatal("live policy request does not contain a complete current policy and sample set")
	}
	for _, sample := range request.Samples {
		if sample.JobID <= 0 || sample.PlatformJobID == "" || sample.CanonicalURL == "" || sample.JobTitle == "" || sample.CompanyName == "" || sample.City == "" || sample.Salary == "" || sample.FullJD == "" || sample.JDHash == "" || !validPolicyLiveVerdict(sample.Verdict) {
			t.Fatalf("live policy request contains incomplete sample: %#v", sample)
		}
	}
}

func validPolicyLiveVerdict(verdict jobpool.HumanVerdict) bool {
	return verdict == jobpool.HumanVerdictSuitable || verdict == jobpool.HumanVerdictUnsuitable
}

func validPolicyLiveStatus(status jobpool.AssessmentStatus) bool {
	switch status {
	case jobpool.AssessmentStatusSuitable, jobpool.AssessmentStatusUnsuitable, jobpool.AssessmentStatusNeedsUserConfirmation:
		return true
	default:
		return false
	}
}

func TestPolicyLiveServicePersistsOneTraceForGeneration(t *testing.T) {
	if os.Getenv("PI_POLICY_LIVE") != "1" {
		t.Skip("set PI_POLICY_LIVE=1 to run the real Pi policy trace probe")
	}
	logPath := filepath.Join(t.TempDir(), "policy-live.jsonl")
	db, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "policy-live.db"))
	if err != nil {
		t.Fatalf("open live policy database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(logPath)
	t.Cleanup(func() { _ = logs.Close() })
	content := onlineresume.ResumeContent{
		JobIntentions:      []onlineresume.JobIntention{{Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职"}},
		WorkExperiences:    []string{"三年 Go 后端开发经验"},
		ProjectExperiences: []string{"使用 Go 和 SQLite 构建本地服务"},
		Educations:         []string{"计算机本科"},
		Skills:             []string{"Go", "SQLite"},
	}
	resumes := onlineresume.New(db, liveResumeReader{content: content}, logs, func() time.Time { return time.UnixMilli(1000) })
	if _, err := resumes.RefreshFromBoss(t.Context()); err != nil {
		t.Fatalf("save live policy resume: %v", err)
	}
	pool := jobpool.New(db)
	job, err := pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: "live-trace-job", CanonicalURL: "https://example.invalid/live-trace-job",
		JobTitle: "Go 后端工程师", CompanyName: "合成测试公司", City: "福州", Salary: "20-30K",
		FullJD: "使用 Go 开发服务\n熟悉 Go", PlatformStatus: jobpool.PlatformStatusOpen,
		ObservedAt: time.UnixMilli(1100),
	})
	if err != nil {
		t.Fatalf("observe live policy job: %v", err)
	}
	if err := pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: job.ID, ExpectedJDHash: job.JDHash, Verdict: jobpool.HumanVerdictSuitable,
	}}); err != nil {
		t.Fatalf("review live policy job: %v", err)
	}
	adapter := New(nil)
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	service := assessment.New(db, resumes, pool, nil, adapter, adapter, logs, func() time.Time { return time.UnixMilli(1200) })
	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure live policy default: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if _, err := service.GeneratePolicyDraft(ctx, []int64{job.ID}); err != nil {
		t.Fatalf("generate live policy through service: %v", err)
	}
	assertPolicyGenerationTrace(t, logPath)
}

type liveResumeReader struct {
	content onlineresume.ResumeContent
}

func (r liveResumeReader) Read(context.Context) (onlineresume.ResumeContent, error) {
	return r.content, nil
}

func assertPolicyGenerationTrace(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // The path is created inside this live test's private temporary directory.
	if err != nil {
		t.Fatalf("open live policy log: %v", err)
	}
	defer func() { _ = file.Close() }()
	var traceID string
	started, finished := 0, 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode live policy log: %v", err)
		}
		if record["operation"] != string(runlog.OperationGeneratePolicy) {
			continue
		}
		currentTrace, ok := record["trace_id"].(string)
		if !ok || currentTrace == "" {
			t.Fatal("live policy generation record has no trace ID")
		}
		if traceID == "" {
			traceID = currentTrace
		}
		if currentTrace != traceID {
			t.Fatalf("live policy generation trace changed from %q to %q", traceID, currentTrace)
		}
		switch record["event"] {
		case "external_attempt_started":
			started++
		case "external_attempt_finished":
			finished++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read live policy log: %v", err)
	}
	if started != 1 || finished != 1 {
		t.Fatalf("live policy generation trace records = started:%v finished:%v", started, finished)
	}
}
