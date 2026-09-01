package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type testWeb struct {
	Handler   http.Handler
	db        *sql.DB
	logs      *runlog.Log
	resume    *webResumeReader
	discovery *webJobDiscovery
	pool      *jobpool.Pool
}

type webPolicyAdvisor struct{}

func (webPolicyAdvisor) Generate(context.Context, assessment.PolicyGenerationRequest) (assessment.PolicyDraft, error) {
	return assessment.PolicyDraft{Text: "明确匹配时判为适合\n信息不足时交给人工确认"}, nil
}

func (webPolicyAdvisor) Validate(_ context.Context, request assessment.PolicyValidationRequest) (assessment.PolicyValidationResult, error) {
	results := make([]assessment.PolicyValidationComparison, 0, len(request.Samples))
	for _, sample := range request.Samples {
		current := jobpool.AssessmentStatusNeedsUserConfirmation
		candidate := jobpool.AssessmentStatusUnsuitable
		if sample.Verdict == jobpool.HumanVerdictSuitable {
			current = jobpool.AssessmentStatusSuitable
			candidate = jobpool.AssessmentStatusSuitable
		}
		results = append(results, assessment.PolicyValidationComparison{
			JobID: sample.JobID, CurrentStatus: current, CandidateStatus: candidate,
		})
	}
	return assessment.PolicyValidationResult{Results: results}, nil
}

type webResumeReader struct {
	content onlineresume.ResumeContent
	err     error
	calls   int
}

func (r *webResumeReader) Read(context.Context) (onlineresume.ResumeContent, error) {
	r.calls++
	return r.content, r.err
}

type webJobDiscovery struct{}

func (d *webJobDiscovery) ListPage(
	context.Context,
	discovery.SearchRange,
	int,
) (discovery.JobPage, error) {
	return discovery.JobPage{PlatformJobIDs: []string{"boss-job-1", "boss-job-2"}, HasMore: false}, nil
}

func (d *webJobDiscovery) ReadJob(_ context.Context, platformJobID string) (discovery.JobObservation, error) {
	if platformJobID == "boss-job-2" {
		job := webDiscoveredJob(platformJobID, "Go 平台工程师", "另一科技")
		job.Salary = ""
		return job, nil
	}
	return webDiscoveredJob(platformJobID, "Go 后端工程师", "示例科技"), nil
}

func webDiscoveredJob(platformJobID, title, company string) discovery.JobObservation {
	return discovery.JobObservation{
		PlatformJobID:    platformJobID,
		CanonicalURL:     "https://www.zhipin.com/job_detail/" + platformJobID + ".html",
		JobTitle:         title,
		CompanyName:      company,
		City:             "福州",
		Salary:           "20-30K",
		Responsibilities: "负责 Go 服务开发",
		Requirements:     "熟悉 Go 与 SQLite",
		PlatformStatus:   discovery.PlatformStatusOpen,
	}
}

func webResumeContent(role string) onlineresume.ResumeContent {
	return onlineresume.ResumeContent{
		JobIntentions: []onlineresume.JobIntention{{
			Role: role, City: "福州", Salary: "20-30K", EmploymentType: "全职",
		}},
		WorkExperiences:    []string{"某公司｜后端工程师"},
		ProjectExperiences: []string{"招聘助手"},
		Educations:         []string{"某大学｜计算机本科"},
		Skills:             []string{"Go", "SQLite"},
	}
}

func mustObserveWebJob(t *testing.T, pool *jobpool.Pool, observation jobpool.Observation) jobpool.JobView {
	t.Helper()
	job, err := pool.Observe(t.Context(), 1, observation)
	if err != nil {
		t.Fatalf("observe web job: %v", err)
	}
	return job
}

func seedWebAssessmentInputs(t *testing.T, db *sql.DB, resumeVersion, policyVersion int64) (int64, int64) {
	t.Helper()
	resumeResult, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (?, '{"jobIntentions":[]}', ?, 0, 1000)
	`, resumeVersion, fmt.Sprintf("resume-v%d", resumeVersion))
	if err != nil {
		t.Fatalf("seed assessment online resume: %v", err)
	}
	resumeID, err := resumeResult.LastInsertId()
	if err != nil {
		t.Fatalf("read assessment online resume ID: %v", err)
	}
	policyResult, err := db.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, created_at
		) VALUES (?, '{"rules":["match"]}', 0, 1000)
	`, policyVersion)
	if err != nil {
		t.Fatalf("seed assessment policy: %v", err)
	}
	policyID, err := policyResult.LastInsertId()
	if err != nil {
		t.Fatalf("read assessment policy ID: %v", err)
	}
	return resumeID, policyID
}

func mustCompleteWebAssessment(
	t *testing.T,
	runtime *testWeb,
	job jobpool.JobView,
	resumeVersion, policyVersion, evaluatorVersion int64,
	status jobpool.AssessmentStatus,
	reason string,
	evidence json.RawMessage,
) {
	t.Helper()
	resumeID, policyID := seedWebAssessmentInputs(t, runtime.db, resumeVersion, policyVersion)
	queueResult, err := runtime.pool.QueueAssessments(t.Context(), []int64{job.ID})
	if err != nil || queueResult.Succeeded != 1 {
		t.Fatalf("queue assessment: result=%#v err=%v", queueResult, err)
	}
	work, err := runtime.pool.ClaimAssessments(t.Context(), jobpool.AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: evaluatorVersion, ProcessingLimit: 1,
		ClaimedAt: time.UnixMilli(1500), LeaseUntil: time.UnixMilli(2500),
	})
	if err != nil || len(work) != 1 {
		t.Fatalf("claim assessment: work=%#v err=%v", work, err)
	}
	finishResult, err := runtime.pool.FinishAssessments(t.Context(), []jobpool.AssessmentOutcome{{
		JobID: job.ID, AttemptNo: work[0].AttemptNo, Status: status,
		Reason: reason, Evidence: evidence, CompletedAt: time.UnixMilli(2000),
	}})
	if err != nil || finishResult.Succeeded != 1 {
		t.Fatalf("finish assessment: result=%#v err=%v", finishResult, err)
	}
}

func mustGetWebJobDetail(t *testing.T, runtime *testWeb, jobID int64) jobpool.JobDetailView {
	t.Helper()
	detail, err := runtime.pool.GetJobDetail(t.Context(), jobID)
	if err != nil {
		t.Fatalf("get reviewed job detail: %v", err)
	}
	return detail
}

func assertReviewRedirect(t *testing.T, response *http.Response, jobID int64) {
	t.Helper()
	wantLocation := fmt.Sprintf("/jobs/%d?reviewed=1", jobID)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != wantLocation {
		t.Fatalf("review response = %d location %q, want 303 to %q", response.StatusCode, response.Header.Get("Location"), wantLocation)
	}
}

func assertAssessmentUnchangedByReview(t *testing.T, detail jobpool.JobDetailView) {
	t.Helper()
	if detail.AssessmentAttemptNo != 1 || detail.AssessmentStatus != jobpool.AssessmentStatusUnsuitable ||
		detail.AssessmentReason != "缺少明确的高并发证据" {
		t.Errorf("assessment changed during human review: %#v", detail.JobView)
	}
}

func openTestWeb(t *testing.T, path string) *testWeb {
	t.Helper()
	return openTestWebWithAdvisor(t, path, filepath.Join(t.TempDir(), "boss-job-agent.jsonl"), nil)
}

func openTestWebWithLogPath(t *testing.T, databasePath, logPath string) *testWeb {
	t.Helper()
	return openTestWebWithAdvisor(t, databasePath, logPath, nil)
}

func openTestWebWithAdvisor(t *testing.T, databasePath, logPath string, advisor assessment.PolicyAdvisor) *testWeb {
	t.Helper()
	db, err := storage.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	pool := jobpool.New(db)
	settings := automationsettings.New(db, pool)
	now := time.UnixMilli(1000)
	logs := runlog.Open(logPath)
	resumeReader := &webResumeReader{content: webResumeContent("Go 后端工程师")}
	resumeVersions := onlineresume.New(db, resumeReader, logs, func() time.Time { return now })
	assessmentService := assessment.New(db, resumeVersions, pool, settings, nil, advisor, logs, func() time.Time { return now })
	if err := assessmentService.EnsureDefaultPolicy(t.Context(), now); err != nil {
		_ = db.Close()
		t.Fatalf("ensure default policy: %v", err)
	}
	if err := settings.EnsureSafeDefaults(t.Context(), now); err != nil {
		_ = db.Close()
		t.Fatalf("ensure safe automation settings: %v", err)
	}
	discoveryAdapter := &webJobDiscovery{}
	discoveryService := discovery.New(db, resumeVersions, pool, discoveryAdapter, logs, func() time.Time { return now })
	return &testWeb{
		Handler: New(Dependencies{
			Resume:     resumeVersions,
			Discovery:  discoveryService,
			Jobs:       pool,
			Assessment: assessmentService,
			Settings:   settings,
			Runlog:     logs,
		}),
		db:        db,
		logs:      logs,
		resume:    resumeReader,
		discovery: discoveryAdapter,
		pool:      pool,
	}
}

func closeTestWeb(t *testing.T, runtime *testWeb) {
	t.Helper()
	if err := runtime.logs.Close(); err != nil {
		t.Errorf("close test runlog: %v", err)
	}
	if err := runtime.db.Close(); err != nil {
		t.Errorf("close test sqlite: %v", err)
	}
}

func TestDegradedRunlogKeepsSQLiteWebAvailableAndSupportsImmediateRecheck(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blockedParent := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("conflict"), 0o600); err != nil {
		t.Fatalf("write blocking parent: %v", err)
	}
	runtime := openTestWebWithLogPath(t, ":memory:", filepath.Join(blockedParent, "boss-job-agent.jsonl"))
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)

	assertPageContains(t, server.Client(), server.URL+"/jobs", []string{
		"运行日志不可用",
		"新的 BOSS/Pi 外部动作已关闭",
		`action="/runlog/recheck?return=jobs"`,
		"尚无岗位",
	})
	if err := os.Remove(blockedParent); err != nil {
		t.Fatalf("remove blocking parent: %v", err)
	}
	response := postJSONResponse(t, server.Client(), server.URL+"/api/runlog/recheck", `{}`)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recheck status = %d, want 200", response.StatusCode)
	}
	var health runlog.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode recheck health: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("recheck health = %#v, want healthy", health)
	}
}

func TestFirstUseWebProvidesFourStableEntriesAndSafeState(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	pages := []struct {
		path string
		want []string
	}{
		{path: "/jobs", want: []string{"岗位工作台", "尚无岗位", "请先刷新在线简历，再开始岗位发现", "disabled"}},
		{path: "/assessments", want: []string{"岗位鉴定", "默认策略 v1", "自动岗位鉴定", "已关闭", "同时鉴定数", "5"}},
		{path: "/outreach", want: []string{"打招呼", "自动打招呼", "已关闭", "未配置", "全天可打招呼", "请先配置固定招呼语"}},
		{path: "/resume", want: []string{"在线简历", "尚无在线简历版本"}},
	}

	for _, page := range pages {
		t.Run(page.path, func(t *testing.T) {
			assertFirstUsePage(t, client, server.URL+page.path, page.want)
		})
	}
	if runtime.resume.calls != 0 {
		t.Errorf("page reads triggered %d BOSS online resume calls, want 0", runtime.resume.calls)
	}
}

func TestAssessmentPageConfiguresAutomaticAdmissionAndAnUncappedPositiveLimit(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	assertPageContains(t, client, server.URL+"/assessments", []string{
		`action="/assessments/settings"`,
		`name="automaticAssessmentEnabled"`,
		`name="assessmentProcessingLimit"`,
		`min="1"`,
		`value="5"`,
		"保存鉴定设置",
	})
	response := postFormResponse(t, client, server.URL+"/assessments/settings", url.Values{
		"automaticAssessmentEnabled": {"on"},
		"assessmentProcessingLimit":  {"37"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("configure assessment status = %d, want redirected 200; body=%s", response.StatusCode, body)
	}
	assertTextContains(t, body, []string{`checked`, `value="37"`, "已开启"})

	var enabled int
	var limit int
	if err := runtime.db.QueryRowContext(t.Context(), `
		SELECT automatic_assessment_enabled, assessment_processing_limit
		FROM automation_settings WHERE id = 1
	`).Scan(&enabled, &limit); err != nil {
		t.Fatalf("read configured assessment settings: %v", err)
	}
	if enabled != 1 || limit != 37 {
		t.Errorf("stored assessment settings = enabled %d, limit %d; want 1 and 37", enabled, limit)
	}
}

func TestAssessmentPageRejectsANonPositiveProcessingLimit(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)

	response := postFormResponse(t, server.Client(), server.URL+"/assessments/settings", url.Values{
		"automaticAssessmentEnabled": {"on"},
		"assessmentProcessingLimit":  {"0"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("configure invalid assessment limit status = %d, want 400; body=%s", response.StatusCode, body)
	}
	assertTextContains(t, body, []string{"AI 同时鉴定数必须是正整数"})
}

func TestOnlineResumePageRunsTheCompleteControlledRefreshFlow(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	assertPageContains(t, client, server.URL+"/resume", []string{
		"尚无在线简历版本",
		`action="/resume/refresh"`,
		">刷新在线简历</button>",
	})
	refreshResumePage(t, client, server.URL, http.StatusOK, []string{
		"已保存在线简历 v1",
		"在线简历 v1",
		"当前没有进行中的岗位发现",
		"下一次岗位发现和尚未开始的鉴定将使用 v1",
	})
	refreshResumePage(t, client, server.URL, http.StatusOK, []string{"内容未变化", "继续使用在线简历 v1"})

	seedWebActiveDiscovery(t, runtime.db, currentResumeVersionID(t, runtime.db))
	runtime.resume.content = webResumeContent("Go 研发工程师")
	refreshResumePage(t, client, server.URL, http.StatusOK, []string{
		"已保存在线简历 v2",
		"当前岗位发现运行继续使用 v1",
		"v2 将用于下一次岗位发现和尚未开始的鉴定",
	})

	runtime.resume.err = errors.New("browser response includes cookie=secret")
	failedBody := refreshResumePage(t, client, server.URL, http.StatusConflict, []string{
		"读取 BOSS 在线简历失败，已保留上一次可靠版本",
		"在线简历 v2",
	})
	assertTextAbsent(t, failedBody, "cookie=secret")
	assertResumeReadCount(t, runtime.resume, 4)
}

func TestJobsPageDisplaysDiscoveryInputsProgressSettingsHintsAndGlobalJobs(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	refreshResumePage(t, client, server.URL, http.StatusOK, []string{"已保存在线简历 v1"})
	assertPageContains(t, client, server.URL+"/jobs", []string{
		"准备创建岗位发现",
		"在线简历 v1 的实际搜索输入",
		"Go 后端工程师",
		"福州",
		"20-30K",
		"全职",
		"确认并开始岗位发现",
	})
	response := postJSONResponse(t, client, server.URL+"/api/discovery-runs", `{}`)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("start discovery status = %d, want 201; body=%s", response.StatusCode, body)
	}
	assertTextContains(t, body, []string{`"discoveryRunId":1`})
	completeWebDiscovery(t, runtime, 1)

	assertPageContains(t, client, server.URL+"/jobs", []string{
		"岗位发现：发现完成",
		"在线简历 v1",
		"已完成 1 / 1",
		"Go 后端工程师",
		"福州",
		"下一页",
		"已发现岗位数",
		">2</dd>",
		"自动岗位鉴定已关闭：新发现岗位将只保存",
		"自动打招呼已关闭：合适岗位暂不进入打招呼队列",
		"固定招呼语未配置：这不阻止岗位发现",
		"示例科技",
		"Go 平台工程师",
		"另一科技",
		`style="width: 100%"`,
		"薪资仅可在 BOSS 页面查看",
	})

	runtime.resume.content = webResumeContent("Go 研发工程师")
	refreshResumePage(t, client, server.URL, http.StatusOK, []string{"已保存在线简历 v2"})
	assertPageContains(t, client, server.URL+"/jobs", []string{
		"在线简历 v1 的实际搜索输入",
		"下一轮将采用在线简历 v2 的实际搜索输入",
		"Go 研发工程师",
		"确认并开始岗位发现",
	})
}

func TestJobDetailShowsCompleteAssessmentInputsAndReviewDoesNotStartAnotherAssessment(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	job := mustObserveWebJob(t, runtime.pool, jobpool.Observation{
		PlatformJobID: "boss-job-review", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-review.html",
		JobTitle: "Go 平台工程师", CompanyName: "示例科技", City: "福州", Salary: "25-35K",
		Responsibilities: "负责高并发 Go 服务\n维护关键链路",
		Requirements:     "熟悉 Go、SQLite\n有可观测性经验",
		PlatformStatus:   jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	mustCompleteWebAssessment(
		t, runtime, job, 3, 4, 9, jobpool.AssessmentStatusUnsuitable,
		"缺少明确的高并发证据", json.RawMessage(`{"missing":["高并发"]}`),
	)

	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	detailURL := fmt.Sprintf("%s/jobs/%d", server.URL, job.ID)
	assertPageContains(t, server.Client(), detailURL, []string{
		"Go 平台工程师", "示例科技", "福州", "25-35K",
		"负责高并发 Go 服务", "维护关键链路", "熟悉 Go、SQLite", "有可观测性经验",
		"AI 结论", "不适合", "缺少明确的高并发证据", "高并发",
		"在线简历 v3", "岗位鉴定策略 v4", "鉴定器 v9",
		`action="/jobs/1/review"`, "适合", "不适合", "可选说明",
	})

	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response := postFormResponse(t, client, detailURL+"/review", url.Values{
		"verdict": {"suitable"},
		"note":    {"项目经历可以覆盖"},
		"jdHash":  {job.JDHash},
	})
	defer closeResponseBody(t, response.Body)
	assertReviewRedirect(t, response, job.ID)

	detail := mustGetWebJobDetail(t, runtime, job.ID)
	if detail.HumanVerdict != jobpool.HumanVerdictSuitable || detail.HumanReviewNote != "项目经历可以覆盖" {
		t.Errorf("human review = %#v, want suitable with note", detail.JobView)
	}
	assertAssessmentUnchangedByReview(t, detail)
	assertPageContains(t, server.Client(), detailURL+"?reviewed=1", []string{
		"人工复核已保存", "当前判断", "人工复核 · 适合", "可作为策略优化监督标注",
	})
}

func TestJobDetailQueuesAndRetriesAssessmentWithSeparateCommands(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	job := mustObserveWebJob(t, runtime.pool, jobpool.Observation{
		PlatformJobID: "boss-job-assessment-command",
		CanonicalURL:  "https://www.zhipin.com/job_detail/boss-job-assessment-command.html",
		JobTitle:      "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	detailURL := fmt.Sprintf("%s/jobs/%d", server.URL, job.ID)
	assertPageContains(t, server.Client(), detailURL, []string{
		"安排 AI 鉴定", fmt.Sprintf(`action="/jobs/%d/assessment"`, job.ID),
		"此操作只加入待鉴定队列，尚未选择在线简历、JD、策略或鉴定器版本",
	})

	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response := postFormResponse(t, client, detailURL+"/assessment", url.Values{})
	defer closeResponseBody(t, response.Body)
	wantLocation := fmt.Sprintf("/jobs/%d?assessment=queued", job.ID)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != wantLocation {
		t.Fatalf("queue response = %d location %q, want 303 to %q", response.StatusCode, response.Header.Get("Location"), wantLocation)
	}
	assertWebAssessmentStatus(t, runtime.pool, job.ID, jobpool.AssessmentStatusPending)
	assertPageContains(t, server.Client(), detailURL+"?assessment=queued", []string{
		"已加入 AI 鉴定队列", "待鉴定", "岗位已在等待 AI 鉴定",
	})

	resumeID, policyID := seedWebAssessmentInputs(t, runtime.db, 2, 2)
	work, err := runtime.pool.ClaimAssessments(t.Context(), jobpool.AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(2000), LeaseUntil: time.UnixMilli(3000),
	})
	if err != nil || len(work) != 1 {
		t.Fatalf("claim queued assessment: work=%#v err=%v", work, err)
	}
	failed, err := runtime.pool.FinishAssessments(t.Context(), []jobpool.AssessmentOutcome{{
		JobID: job.ID, AttemptNo: work[0].AttemptNo, Status: jobpool.AssessmentStatusFailed,
		Reason: "Pi 请求失败", Evidence: json.RawMessage(`{"code":"pi_failed"}`),
		CompletedAt: time.UnixMilli(2500),
	}})
	if err != nil || failed.Succeeded != 1 {
		t.Fatalf("fail assessment: result=%#v err=%v", failed, err)
	}
	assertPageContains(t, server.Client(), detailURL, []string{
		"重试 AI 鉴定", fmt.Sprintf(`action="/jobs/%d/assessment/retry"`, job.ID),
	})
	response = postFormResponse(t, client, detailURL+"/assessment/retry", url.Values{})
	defer closeResponseBody(t, response.Body)
	wantLocation = fmt.Sprintf("/jobs/%d?assessment=retried", job.ID)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != wantLocation {
		t.Fatalf("retry response = %d location %q, want 303 to %q", response.StatusCode, response.Header.Get("Location"), wantLocation)
	}
	assertWebAssessmentStatus(t, runtime.pool, job.ID, jobpool.AssessmentStatusPending)
}

func TestJobDetailDisablesAssessmentFailureRetryAfterTheJobCloses(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	observation := jobpool.Observation{
		PlatformJobID: "boss-job-closed-assessment-retry",
		CanonicalURL:  "https://www.zhipin.com/job_detail/boss-job-closed-assessment-retry.html",
		JobTitle:      "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1_000),
	}
	job := mustObserveWebJob(t, runtime.pool, observation)
	queued, err := runtime.pool.QueueAssessments(t.Context(), []int64{job.ID})
	if err != nil || queued.Succeeded != 1 {
		t.Fatalf("queue assessment: result=%#v err=%v", queued, err)
	}
	resumeID, policyID := seedWebAssessmentInputs(t, runtime.db, 2, 2)
	work, err := runtime.pool.ClaimAssessments(t.Context(), jobpool.AssessmentClaim{
		Worker: "assessment-worker", ResumeVersionID: resumeID, PolicyVersionID: policyID,
		EvaluatorVersion: 1, ProcessingLimit: 1, ClaimedAt: time.UnixMilli(2_000), LeaseUntil: time.UnixMilli(3_000),
	})
	if err != nil || len(work) != 1 {
		t.Fatalf("claim assessment: work=%#v err=%v", work, err)
	}
	failed, err := runtime.pool.FinishAssessments(t.Context(), []jobpool.AssessmentOutcome{{
		JobID: job.ID, AttemptNo: work[0].AttemptNo, Status: jobpool.AssessmentStatusFailed,
		Reason: "Pi 请求失败", Evidence: json.RawMessage(`{"code":"pi_failed"}`),
		CompletedAt: time.UnixMilli(2_500),
	}})
	if err != nil || failed.Succeeded != 1 {
		t.Fatalf("fail assessment: result=%#v err=%v", failed, err)
	}
	observation.PlatformStatus = jobpool.PlatformStatusClosed
	observation.PlatformClosedReason = "岗位已关闭"
	observation.ObservedAt = time.UnixMilli(3_000)
	if _, err := runtime.pool.Observe(t.Context(), 2, observation); err != nil {
		t.Fatalf("observe closed job: %v", err)
	}
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	response := getResponse(t, server.Client(), fmt.Sprintf("%s/jobs/%d", server.URL, job.ID))
	defer closeResponseBody(t, response.Body)
	body := readResponseBody(t, response)
	assertTextContains(t, body, []string{"岗位已关闭，不能开始 AI 鉴定", "重试 AI 鉴定"})
	assertTextAbsent(t, body, fmt.Sprintf(`action="/jobs/%d/assessment/retry"`, job.ID))
}

func assertWebAssessmentStatus(
	t *testing.T,
	pool *jobpool.Pool,
	jobID int64,
	want jobpool.AssessmentStatus,
) {
	t.Helper()
	job, err := pool.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatalf("get platform job %d: %v", jobID, err)
	}
	if job.AssessmentStatus != want {
		t.Errorf("job %d assessment status = %q, want %q", jobID, job.AssessmentStatus, want)
	}
}

func TestJobReviewPageRejectsAReviewBasedOnAStaleJD(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	originalObservation := jobpool.Observation{
		PlatformJobID: "boss-job-stale-web-review", CanonicalURL: "https://www.zhipin.com/job_detail/stale.html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	}
	original := mustObserveWebJob(t, runtime.pool, originalObservation)
	changed := originalObservation
	changed.Requirements = "熟悉 Go、SQLite 与分布式事务"
	changed.ObservedAt = time.UnixMilli(2000)
	if _, err := runtime.pool.Observe(t.Context(), 2, changed); err != nil {
		t.Fatalf("observe changed JD: %v", err)
	}

	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	response := postFormResponse(t, server.Client(), fmt.Sprintf("%s/jobs/%d/review", server.URL, original.ID), url.Values{
		"verdict": {"suitable"},
		"jdHash":  {original.JDHash},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale review status = %d, want 409; body=%s", response.StatusCode, body)
	}
	assertTextContains(t, body, []string{"JD 已变化", "重新查看完整岗位后再复核"})
	current, err := runtime.pool.GetJobDetail(t.Context(), original.ID)
	if err != nil {
		t.Fatalf("get job after stale review: %v", err)
	}
	if current.HumanReviewStatus != jobpool.HumanReviewStatusUnreviewed || current.SupervisionLabel != "" {
		t.Errorf("stale web review changed current job: %#v", current)
	}
}

func TestJobsPageContinuesAndEndsTheSameDiscoveryRun(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	refreshResumePage(t, client, server.URL, http.StatusOK, []string{"已保存在线简历 v1"})
	runID := seedWebActiveDiscovery(t, runtime.db, currentResumeVersionID(t, runtime.db))
	assertPageContains(t, client, server.URL+"/jobs", []string{
		"岗位发现：已暂停",
		fmt.Sprintf(`action="/discovery-runs/%d/continue"`, runID),
		fmt.Sprintf(`action="/discovery-runs/%d/end-early"`, runID),
	})
	response := postJSONResponse(
		t,
		client,
		fmt.Sprintf("%s/api/discovery-runs/%d/continue", server.URL, runID),
		`{}`,
	)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("continue discovery status = %d, want 204", response.StatusCode)
	}
	assertPageContains(t, client, server.URL+"/jobs", []string{"岗位发现：运行中"})
	response = postJSONResponse(
		t,
		client,
		fmt.Sprintf("%s/api/discovery-runs/%d/end-early", server.URL, runID),
		`{}`,
	)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("end discovery status = %d, want 204", response.StatusCode)
	}
	assertPageContains(t, client, server.URL+"/jobs", []string{
		"岗位发现：已提前结束",
		"确认并开始岗位发现",
	})
}

func TestJobsPageExposesAnUnpreparedRunForEarlyTermination(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	refreshResumePage(t, client, server.URL, http.StatusOK, []string{"已保存在线简历 v1"})
	result, err := runtime.db.ExecContext(t.Context(), `
		INSERT INTO discovery_runs (status, attempt_no, created_at, updated_at)
		VALUES ('preparing', 0, 1000, 1000)
	`)
	if err != nil {
		t.Fatalf("seed unprepared discovery: %v", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read unprepared discovery ID: %v", err)
	}
	assertPageContains(t, client, server.URL+"/jobs", []string{
		"岗位发现：准备中",
		"尚未冻结本轮在线简历版本",
		fmt.Sprintf(`action="/discovery-runs/%d/end-early"`, runID),
		"请先处理当前未结束的岗位发现运行",
	})

	response := postJSONResponse(
		t,
		client,
		fmt.Sprintf("%s/api/discovery-runs/%d/end-early", server.URL, runID),
		`{}`,
	)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("end unprepared discovery status = %d, want 204", response.StatusCode)
	}
	assertPageContains(t, client, server.URL+"/jobs", []string{
		"岗位发现：已提前结束",
		"确认并开始岗位发现",
	})
}

func TestRemovedSimulationCommandIsNotRoutable(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)

	response := postJSONResponse(t, server.Client(), server.URL+"/api/outreach/simulation", `{"jobIds":[]}`)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func TestWebServesStartupStateAndCSS(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)

	tests := []struct {
		path        string
		contentType string
		want        string
	}{
		{path: "/api/startup-state", contentType: "application/json; charset=utf-8", want: `"assessmentProcessingLimit":5`},
		{path: "/assets/app.css", contentType: "text/css; charset=utf-8", want: ".sidebar"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response := getResponse(t, server.Client(), server.URL+test.path)
			defer closeResponseBody(t, response.Body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			if got := response.Header.Get("Content-Type"); got != test.contentType {
				t.Errorf("content type = %q, want %q", got, test.contentType)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if !strings.Contains(string(body), test.want) {
				t.Errorf("body does not contain %q", test.want)
			}
		})
	}
}

func TestStartupStateDoesNotExposeResumeDatabaseIDOrContent(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	if _, err := runtime.db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			id, version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (
			42, 7,
			'{"jobIntentions":[{"role":"Go 后端工程师","city":"福州","salary":"20-30K","employmentType":"全职"}],"workExperiences":[],"projectExperiences":[],"educations":[],"skills":[]}',
			'resume-hash', 1, 1000
		)
	`); err != nil {
		t.Fatalf("seed current online resume: %v", err)
	}
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)

	response := getResponse(t, server.Client(), server.URL+"/api/startup-state")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("startup state status = %d, want 200; body=%s", response.StatusCode, body)
	}
	assertTextContains(t, body, []string{`"version":7`, `"createdAt":`})
	for _, forbidden := range []string{`"id":42`, `"content":`, "Go 后端工程师"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("startup state exposes %q: %s", forbidden, body)
		}
	}
	if runtime.resume.calls != 0 {
		t.Errorf("startup state triggered %d BOSS online resume calls, want 0", runtime.resume.calls)
	}
}

func TestRealOutreachCommandReturnsPerJobBatchResult(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	if _, err := runtime.db.ExecContext(t.Context(), `
		UPDATE automation_settings
		SET outreach_greeting_text = '您好，想和您聊聊这个岗位'
		WHERE id = 1
	`); err != nil {
		t.Fatalf("configure outreach greeting: %v", err)
	}
	job, err := runtime.pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: "boss-job-1", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatalf("observe outreach job: %v", err)
	}
	if err := runtime.pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: job.ID, ExpectedJDHash: job.JDHash,
		Verdict: jobpool.HumanVerdictSuitable,
	}}); err != nil {
		t.Fatalf("review outreach job: %v", err)
	}
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)

	response := postJSONResponse(t, server.Client(), server.URL+"/api/outreach/real", fmt.Sprintf(`{
		"jobIds":[%d,999],
		"confirmation":{
			"jobCount":1,
			"greetingText":"您好，想和您聊聊这个岗位",
			"timeDescription":"全天可打招呼",
			"confirmed":true
		}
	}`, job.ID))
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("queue real outreach status = %d, want 200", response.StatusCode)
	}
	var result jobpool.BatchActionResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode outreach batch result: %v", err)
	}
	if result.Succeeded != 1 || len(result.Skipped) != 1 || result.Skipped[0].JobID != 999 {
		t.Errorf("outreach batch result = %#v, want one success and missing job 999", result)
	}
}

func TestOutreachPageDisplaysEligibleBatchAndQueuesConfirmedSelection(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	if _, err := runtime.db.ExecContext(t.Context(), `
		UPDATE automation_settings
		SET outreach_greeting_text = '您好，想和您聊聊这个岗位'
		WHERE id = 1
	`); err != nil {
		t.Fatalf("configure outreach greeting: %v", err)
	}
	job, err := runtime.pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: "boss-job-page", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-page.html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: jobpool.PlatformStatusOpen, ObservedAt: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatalf("observe outreach job: %v", err)
	}
	if err := runtime.pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: job.ID, ExpectedJDHash: job.JDHash, Verdict: jobpool.HumanVerdictSuitable,
	}}); err != nil {
		t.Fatalf("review outreach job: %v", err)
	}
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	assertPageContains(t, client, server.URL+"/outreach", []string{
		"当前可入队岗位：1", "加入真实打招呼队列", "您好，想和您聊聊这个岗位", "全天可打招呼",
	})
	response := postFormResponse(t, client, server.URL+"/outreach/real", url.Values{
		"jobId": {fmt.Sprint(job.ID)}, "jobCount": {"1"},
		"greetingText": {"您好，想和您聊聊这个岗位"}, "timeDescription": {"全天可打招呼"},
		"confirmed": {"true"},
	})
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("queue outreach page status = %d, want redirected page", response.StatusCode)
	}
	view, err := runtime.pool.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("get queued outreach job: %v", err)
	}
	if view.OutreachStatus != jobpool.OutreachStatusPending {
		t.Errorf("page queued outreach status = %q, want pending", view.OutreachStatus)
	}
}

func TestWebCommandsReturnBusinessRejections(t *testing.T) {
	t.Parallel()

	runtime := openTestWeb(t, ":memory:")
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	commands := []struct {
		path string
		body string
		code string
	}{
		{path: "/api/discovery-runs", body: `{}`, code: "online_resume_required"},
		{path: "/api/outreach/real", body: `{"jobIds":[],"confirmation":{}}`, code: "outreach_greeting_required"},
	}

	for _, command := range commands {
		t.Run(command.path, func(t *testing.T) {
			response := postJSONResponse(t, client, server.URL+command.path, command.body)
			defer closeResponseBody(t, response.Body)
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", response.StatusCode)
			}
			var rejection struct {
				Code   string `json:"code"`
				Reason string `json:"reason"`
			}
			if err := json.NewDecoder(response.Body).Decode(&rejection); err != nil {
				t.Fatalf("decode rejection: %v", err)
			}
			if rejection.Code != command.code {
				t.Errorf("rejection code = %q, want %q", rejection.Code, command.code)
			}
			if rejection.Reason == "" {
				t.Error("rejection reason is empty")
			}
		})
	}
}

func TestWebRestoresSavedPolicyAndAutomationSettings(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	first := openTestWeb(t, path)
	closeTestWeb(t, first)

	seedSavedPolicyAndSettings(t, path)

	restarted := openTestWeb(t, path)
	t.Cleanup(func() { closeTestWeb(t, restarted) })
	server := httptest.NewServer(restarted.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	assertPageContains(t, client, server.URL+"/assessments", []string{
		"<h2>策略 v2</h2>",
		"<dd>已开启</dd>",
		"<dd>12</dd>",
	})
	assertPageContains(t, client, server.URL+"/outreach", []string{
		"<dd>您好，想和您聊聊这个岗位</dd>",
		"<dd>10:00-12:00（Asia/Shanghai）</dd>",
		"当前没有可真实打招呼的岗位",
	})
}

func TestPolicyOptimizationPageAndAPIsKeepDraftInTheBrowserSession(t *testing.T) {
	t.Parallel()

	runtime := openTestWebWithAdvisor(t, ":memory:", filepath.Join(t.TempDir(), "policy.jsonl"), webPolicyAdvisor{})
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)

	resumeResponse := postFormResponse(t, server.Client(), server.URL+"/resume/refresh", url.Values{})
	defer closeResponseBody(t, resumeResponse.Body)
	if resumeResponse.StatusCode != http.StatusOK {
		t.Fatalf("refresh test resume status = %d", resumeResponse.StatusCode)
	}
	first := seedPolicyWebSamples(t, runtime.pool)

	assertPageContains(t, server.Client(), server.URL+"/assessments", []string{
		"策略优化", "生成临时策略候选稿", "同时验收候选策略（会产生额外模型调用）", "Go 后端工程师",
	})
	draft := generatePolicyWebDraft(t, server.Client(), server.URL)
	validatePolicyWebDraft(t, server.Client(), server.URL, draft)
	adoptPolicyWebDraft(t, server.Client(), server.URL, draft.PolicyVersionID)
	active, err := runtime.pool.GetJob(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("read platform job after policy adoption: %v", err)
	}
	if active.AssessmentStatus != jobpool.AssessmentStatusNotQueued {
		t.Errorf("platform job assessment status after policy adoption = %q, want unchanged", active.AssessmentStatus)
	}
}

func seedPolicyWebSamples(t *testing.T, pool *jobpool.Pool) jobpool.JobView {
	t.Helper()
	if _, err := pool.Observe(t.Context(), 1, jobpool.Observation{
		PlatformJobID: "web-policy-1", CanonicalURL: "https://www.zhipin.com/job_detail/web-policy-1.html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务", Requirements: "熟悉 Go", PlatformStatus: jobpool.PlatformStatusOpen,
		ObservedAt: time.UnixMilli(2000),
	}); err != nil {
		t.Fatalf("observe first policy job: %v", err)
	}
	first := mustObserveWebJob(t, pool, jobpool.Observation{
		PlatformJobID: "web-policy-2", CanonicalURL: "https://www.zhipin.com/job_detail/web-policy-2.html",
		JobTitle: "Go 平台工程师", CompanyName: "另一科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责平台服务", Requirements: "熟悉 Go", PlatformStatus: jobpool.PlatformStatusOpen,
		ObservedAt: time.UnixMilli(2001),
	})
	firstDetail, err := pool.GetJobDetail(t.Context(), 1)
	if err != nil {
		t.Fatalf("get first policy job detail: %v", err)
	}
	if err := pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: 1, ExpectedJDHash: firstDetail.JDHash, Verdict: jobpool.HumanVerdictUnsuitable,
	}}); err != nil {
		t.Fatalf("review first policy job: %v", err)
	}
	if err := pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: first.ID, ExpectedJDHash: first.JDHash, Verdict: jobpool.HumanVerdictSuitable,
	}}); err != nil {
		t.Fatalf("review second policy job: %v", err)
	}
	return first
}

func generatePolicyWebDraft(t *testing.T, client *http.Client, baseURL string) assessment.PolicyDraft {
	t.Helper()
	response := postJSONResponse(t, client, baseURL+"/api/policy/draft", `{"jobIds":[1],"validationEnabled":true}`)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("generate policy draft status = %d", response.StatusCode)
	}
	var draft assessment.PolicyDraft
	if err := json.NewDecoder(response.Body).Decode(&draft); err != nil {
		t.Fatalf("decode policy draft: %v", err)
	}
	if !draft.ValidationEnabled || draft.GenerationSampleCount != 1 || draft.Text == "" {
		t.Fatalf("draft = %#v, want browser-session metadata", draft)
	}
	return draft
}

func validatePolicyWebDraft(t *testing.T, client *http.Client, baseURL string, draft assessment.PolicyDraft) {
	t.Helper()
	response := postJSONResponse(t, client, baseURL+"/api/policy/validate", mustJSON(t, draft))
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("validate policy draft status = %d", response.StatusCode)
	}
	var report assessment.PolicyValidationReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatalf("decode policy validation report: %v", err)
	}
	if report.Status != assessment.PolicyValidationPassed || len(report.FullResults) != 2 || len(report.UngeneratedResults) != 1 {
		t.Errorf("policy validation report = %#v", report)
	}
}

func adoptPolicyWebDraft(t *testing.T, client *http.Client, baseURL string, policyVersionID int64) {
	t.Helper()
	payload := mustJSON(t, map[string]any{
		"text": "新的完整规则\n信息不足时人工确认", "policyVersionId": policyVersionID,
	})
	response := postJSONResponse(t, client, baseURL+"/api/policy/adopt", payload)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("adopt policy status = %d", response.StatusCode)
	}
	retry := postJSONResponse(t, client, baseURL+"/api/policy/adopt", payload)
	defer closeResponseBody(t, retry.Body)
	if retry.StatusCode != http.StatusConflict {
		t.Fatalf("retry adopt policy status = %d, want conflict", retry.StatusCode)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON fixture: %v", err)
	}
	return string(encoded)
}

func seedSavedPolicyAndSettings(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE assessment_policy_versions SET is_active = 0 WHERE version_no = 1`); err != nil {
		t.Fatalf("deactivate default policy: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (2, '{"rules":["用户保存的策略"]}', 1, '用户采用', 2000)
	`); err != nil {
		t.Fatalf("insert saved policy: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), `
		UPDATE automation_settings
		SET automatic_assessment_enabled = 1,
			assessment_processing_limit = 12,
			automatic_outreach_enabled = 1,
			outreach_greeting_text = '您好，想和您聊聊这个岗位',
			outreach_time_windows_json = '[{"start":"10:00","end":"12:00"}]',
			updated_at = 2000
		WHERE id = 1
	`); err != nil {
		t.Fatalf("save custom settings: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fixture transaction: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}
}

func TestWebPagesDoNotDependOnUnrelatedDownstreamState(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	runtime := openTestWeb(t, path)
	t.Cleanup(func() { closeTestWeb(t, runtime) })
	server := httptest.NewServer(runtime.Handler)
	t.Cleanup(server.Close)
	client := server.Client()

	if _, err := runtime.db.ExecContext(t.Context(), `DELETE FROM assessment_policy_versions`); err != nil {
		t.Fatalf("remove unrelated policy: %v", err)
	}

	assertPageStatus(t, client, server.URL+"/jobs", http.StatusOK)
	assertPageStatus(t, client, server.URL+"/outreach", http.StatusOK)
	assertPageStatus(t, client, server.URL+"/resume", http.StatusOK)

	if _, err := runtime.db.ExecContext(t.Context(), `DELETE FROM automation_settings`); err != nil {
		t.Fatalf("remove unrelated automation settings: %v", err)
	}
	assertPageStatus(t, client, server.URL+"/jobs", http.StatusOK)
	assertPageStatus(t, client, server.URL+"/resume", http.StatusOK)
}

func assertFirstUsePage(t *testing.T, client *http.Client, url string, wants []string) {
	t.Helper()
	response := getResponse(t, client, url)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	text := string(body)
	assertTextContains(t, text, []string{
		`href="/jobs"`,
		`href="/assessments"`,
		`href="/outreach"`,
		`href="/resume"`,
	})
	if strings.Contains(text, "执行情况") {
		t.Error("body contains a standalone execution-status entry")
	}
	if strings.Contains(text, "Simulation") || strings.Contains(text, "模拟打招呼") {
		t.Error("body exposes the removed simulation product capability")
	}
	assertTextContains(t, text, wants)
}

func assertTextContains(t *testing.T, text string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("body does not contain %q", want)
		}
	}
}

func postFormResponse(t *testing.T, client *http.Client, target string, values ...url.Values) *http.Response {
	t.Helper()
	encoded := ""
	if len(values) > 0 {
		encoded = values[0].Encode()
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(encoded))
	if err != nil {
		t.Fatalf("create form request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	return response
}

func refreshResumePage(t *testing.T, client *http.Client, baseURL string, status int, wants []string) string {
	t.Helper()
	response := postFormResponse(t, client, baseURL+"/resume/refresh")
	body := readResponseBody(t, response)
	if response.StatusCode != status {
		t.Fatalf("refresh status = %d, want %d; body=%s", response.StatusCode, status, body)
	}
	assertTextContains(t, body, wants)
	return body
}

func currentResumeVersionID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var versionID int64
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM online_resume_versions WHERE is_current = 1`).Scan(&versionID); err != nil {
		t.Fatalf("query current online resume version ID: %v", err)
	}
	return versionID
}

func seedWebActiveDiscovery(t *testing.T, db *sql.DB, resumeVersionID int64) int64 {
	t.Helper()
	result, err := db.ExecContext(t.Context(), `
		INSERT INTO discovery_runs (
			resume_version_id, current_role, current_city, next_page,
			status, attempt_no, created_at, prepared_at, updated_at
		) VALUES (?, 'Go 后端工程师', '福州', 1, 'paused', 1, 1000, 1000, 1000)
	`, resumeVersionID)
	if err != nil {
		t.Fatalf("seed active discovery using v1: %v", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read seeded discovery run ID: %v", err)
	}
	return runID
}

func completeWebDiscovery(t *testing.T, runtime *testWeb, runID int64) {
	t.Helper()
	observations := []discovery.JobObservation{
		webDiscoveredJob("boss-job-1", "Go 后端工程师", "示例科技"),
		webDiscoveredJob("boss-job-2", "Go 平台工程师", "另一科技"),
	}
	observations[1].Salary = ""
	for _, observation := range observations {
		if _, err := runtime.pool.Observe(t.Context(), runID, jobpool.Observation{
			PlatformJobID: observation.PlatformJobID, CanonicalURL: observation.CanonicalURL,
			JobTitle: observation.JobTitle, CompanyName: observation.CompanyName,
			City: observation.City, Salary: observation.Salary,
			Responsibilities: observation.Responsibilities, Requirements: observation.Requirements,
			PlatformStatus: jobpool.PlatformStatus(observation.PlatformStatus),
			ObservedAt:     time.UnixMilli(2000),
		}); err != nil {
			t.Fatalf("seed discovered job: %v", err)
		}
	}
	if _, err := runtime.db.ExecContext(t.Context(), `
		UPDATE discovery_runs
		SET status = 'completed', worker_owner = NULL, worker_lease_until = NULL,
			last_progress_at = 2000, finished_at = 2000, updated_at = 2000
		WHERE id = ?
	`, runID); err != nil {
		t.Fatalf("complete discovery fixture: %v", err)
	}
}

func assertTextAbsent(t *testing.T, text, forbidden string) {
	t.Helper()
	if strings.Contains(text, forbidden) {
		t.Errorf("text contains forbidden value %q", forbidden)
	}
}

func assertResumeReadCount(t *testing.T, reader *webResumeReader, want int) {
	t.Helper()
	if reader.calls != want {
		t.Errorf("online resume reads = %d, want %d", reader.calls, want)
	}
}

func readResponseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer closeResponseBody(t, response.Body)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(body)
}

func assertPageContains(t *testing.T, client *http.Client, url string, wants []string) {
	t.Helper()
	response := getResponse(t, client, url)
	defer closeResponseBody(t, response.Body)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	assertTextContains(t, string(body), wants)
}

func assertPageStatus(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	response := getResponse(t, client, url)
	defer closeResponseBody(t, response.Body)
	if response.StatusCode != want {
		t.Errorf("page status = %d, want %d", response.StatusCode, want)
	}
}

func getResponse(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("create GET request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	return response
}

func postJSONResponse(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("post command: %v", err)
	}
	return response
}

func closeResponseBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Errorf("close response body: %v", err)
	}
}
