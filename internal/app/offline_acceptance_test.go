package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/outreach"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
	"github.com/Russell-Utopia/boss-job-agent/internal/webui"
)

type offlineAcceptanceRuntime struct {
	db         *sql.DB
	logs       *runlog.Log
	handler    http.Handler
	resume     *offlineResumeReader
	discovery  *offlineJobDiscovery
	assessment *offlineAssessmentAdapter
	outreach   *offlineOutreachAdapter
	pool       *jobpool.Pool
	service    *offlineServices
}

type offlineServices struct {
	discovery  *discovery.Service
	assessment *assessment.Service
	outreach   *outreach.Service
}

type offlineResumeReader struct {
	content onlineresume.ResumeContent
	mu      sync.Mutex
	calls   int
}

func (r *offlineResumeReader) Read(context.Context) (onlineresume.ResumeContent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.content, nil
}

type offlineJobDiscovery struct {
	jobs        map[string]discovery.JobObservation
	readStarted chan struct{}
	releaseRead chan struct{}
	readOnce    sync.Once
	mu          sync.Mutex
	list        int
	read        int
}

func (d *offlineJobDiscovery) ListPage(
	context.Context,
	discovery.SearchRange,
	int,
) (discovery.JobPage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.list++
	return discovery.JobPage{PlatformJobIDs: []string{"offline-job-1"}, HasMore: false}, nil
}

func (d *offlineJobDiscovery) ReadJob(ctx context.Context, platformJobID string) (discovery.JobObservation, error) {
	d.mu.Lock()
	d.read++
	job, ok := d.jobs[platformJobID]
	d.mu.Unlock()
	if d.readStarted != nil && d.releaseRead != nil {
		d.readOnce.Do(func() { close(d.readStarted) })
		select {
		case <-d.releaseRead:
		case <-ctx.Done():
			return discovery.JobObservation{}, ctx.Err()
		}
	}
	if !ok {
		return discovery.JobObservation{}, fmt.Errorf("受控 Adapter 没有岗位 %q", platformJobID)
	}
	return job, nil
}

type offlineAssessmentAdapter struct {
	service  *assessment.Service
	mu       sync.Mutex
	requests []assessment.AssessmentRequest
	closes   int
}

func (a *offlineAssessmentAdapter) Submit(ctx context.Context, request assessment.AssessmentRequest) error {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	if a.service == nil {
		return errors.New("受控鉴定 Adapter 尚未连接业务服务")
	}
	results := make([]assessment.AssessmentConfirmation, 0, len(request.Jobs))
	expected := make([]assessment.ConfirmationAttempt, 0, len(request.Jobs))
	for _, job := range request.Jobs {
		results = append(results, assessment.AssessmentConfirmation{
			JobID: job.JobID, AttemptNo: job.AttemptNo,
			Status:   jobpool.AssessmentStatusSuitable,
			Reason:   "受控 Adapter 返回明确匹配证据",
			Evidence: json.RawMessage(`{"source":"controlled-adapter"}`),
		})
		expected = append(expected, assessment.ConfirmationAttempt{JobID: job.JobID, AttemptNo: job.AttemptNo})
	}
	_, err := a.service.Confirm(ctx, assessment.ConfirmationBatch{
		TraceID: request.TraceID, Results: results, ExpectedAttempts: expected,
	})
	return err
}

func (a *offlineAssessmentAdapter) Close(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closes++
	return nil
}

type offlinePolicyAdvisor struct{}

func (offlinePolicyAdvisor) Generate(context.Context, assessment.PolicyGenerationRequest) (assessment.PolicyDraft, error) {
	return assessment.PolicyDraft{Text: "明确匹配时判为适合，信息不足时交给人工确认"}, nil
}

func (offlinePolicyAdvisor) Validate(
	_ context.Context,
	request assessment.PolicyValidationRequest,
) (assessment.PolicyValidationResult, error) {
	results := make([]assessment.PolicyValidationComparison, 0, len(request.Samples))
	for _, sample := range request.Samples {
		candidate := jobpool.AssessmentStatusUnsuitable
		if sample.Verdict == jobpool.HumanVerdictSuitable {
			candidate = jobpool.AssessmentStatusSuitable
		}
		results = append(results, assessment.PolicyValidationComparison{
			JobID:           sample.JobID,
			CurrentStatus:   jobpool.AssessmentStatusNeedsUserConfirmation,
			CandidateStatus: candidate,
		})
	}
	return assessment.PolicyValidationResult{Results: results}, nil
}

type offlineOutreachAdapter struct {
	mu     sync.Mutex
	checks []outreach.PlatformJobRef
	sends  []outreach.FirstContactRequest
	status outreach.ContactStatus
	result outreach.FirstContactResult
}

func (a *offlineOutreachAdapter) Check(_ context.Context, ref outreach.PlatformJobRef) (outreach.ContactStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checks = append(a.checks, ref)
	return a.status, nil
}

func (a *offlineOutreachAdapter) Send(_ context.Context, request outreach.FirstContactRequest) (outreach.FirstContactResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sends = append(a.sends, request)
	return a.result, nil
}

func newOfflineAcceptanceRuntime(t *testing.T, databasePath, logPath string) *offlineAcceptanceRuntime {
	t.Helper()
	db, err := storage.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("打开离线验收 SQLite: %v", err)
	}
	logs := runlog.Open(logPath)
	now := func() time.Time { return time.UnixMilli(1000) }
	pool := jobpool.New(db)
	settings := automationsettings.New(db, pool)
	resumeReader := &offlineResumeReader{content: offlineResumeContent()}
	resumeVersions := onlineresume.New(db, resumeReader, logs, now)
	assessmentAdapter := &offlineAssessmentAdapter{}
	assessmentService := assessment.New(
		db, resumeVersions, pool, settings, assessmentAdapter, offlinePolicyAdvisor{}, logs, now,
	)
	assessmentAdapter.service = assessmentService
	if err := assessmentService.EnsureDefaultPolicy(t.Context(), now()); err != nil {
		_ = logs.Close()
		_ = db.Close()
		t.Fatalf("初始化默认岗位鉴定策略: %v", err)
	}
	if err := settings.EnsureSafeDefaults(t.Context(), now()); err != nil {
		_ = logs.Close()
		_ = db.Close()
		t.Fatalf("初始化安全自动化设置: %v", err)
	}
	discoveryAdapter := &offlineJobDiscovery{jobs: map[string]discovery.JobObservation{
		"offline-job-1": offlineJobObservation(),
	}}
	discoveryService := discovery.New(db, resumeVersions, pool, discoveryAdapter, logs, now)
	outreachAdapter := &offlineOutreachAdapter{
		status: outreach.ContactStatus{
			Open: true, Evidence: json.RawMessage(`{"open":true,"contacted":false}`),
		},
		result: outreach.FirstContactResult{
			Effect:   outreach.OutreachEffectConfirmedSent,
			Evidence: json.RawMessage(`{"sent":true}`),
		},
	}
	outreachService := outreach.New(pool, settings, outreachAdapter, logs)
	return &offlineAcceptanceRuntime{
		db: db, logs: logs, pool: pool, resume: resumeReader,
		discovery: discoveryAdapter, assessment: assessmentAdapter, outreach: outreachAdapter,
		service: &offlineServices{discovery: discoveryService, assessment: assessmentService, outreach: outreachService},
		handler: webui.New(webui.Dependencies{
			Resume: resumeVersions, Discovery: discoveryService, Jobs: pool,
			Assessment: assessmentService, Settings: settings, Runlog: logs,
		}),
	}
}

func closeOfflineAcceptanceRuntime(t *testing.T, runtime *offlineAcceptanceRuntime) {
	t.Helper()
	if err := runtime.logs.Close(); err != nil {
		t.Errorf("关闭离线验收 runlog: %v", err)
	}
	if err := runtime.db.Close(); err != nil {
		t.Errorf("关闭离线验收 SQLite: %v", err)
	}
}

func offlineResumeContent() onlineresume.ResumeContent {
	return onlineresume.ResumeContent{
		JobIntentions: []onlineresume.JobIntention{{
			Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
		}},
		WorkExperiences: []string{"后端工程师"}, ProjectExperiences: []string{"招聘助手"},
		Educations: []string{"计算机本科"}, Skills: []string{"Go", "SQLite"},
	}
}

func offlineJobObservation() discovery.JobObservation {
	return discovery.JobObservation{
		PlatformJobID: "offline-job-1",
		CanonicalURL:  "https://www.zhipin.com/job_detail/offline-job-1.html",
		JobTitle:      "Go 后端工程师", CompanyName: "受控科技", City: "福州", Salary: "20-30K",
		Responsibilities: "负责 Go 服务开发", Requirements: "熟悉 Go 与 SQLite",
		PlatformStatus: discovery.PlatformStatusOpen,
	}
}

func TestOfflineMVPPathUsesControlledAdaptersAndRestoresSQLiteState(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "boss-job-agent.db")
	logPath := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	runtime := newOfflineAcceptanceRuntime(t, databasePath, logPath)
	server, job := completeOfflineDiscovery(t, runtime)
	server, job = completeOfflineAssessment(t, runtime, server, job)
	completeOfflineOutreach(t, runtime, server, job)
	closeOfflineAcceptanceRuntime(t, runtime)

	assertOfflineRestartRestoresState(t, databasePath, logPath)
}

func TestOfflinePolicyOptimizationAndValidationKeepDraftInPageSession(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "boss-job-agent.db")
	logPath := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	runtime := newOfflineAcceptanceRuntime(t, databasePath, logPath)
	server := httptest.NewServer(runtime.handler)
	client := server.Client()

	status, body := offlinePostFormBody(t, client, server.URL+"/resume/refresh", "")
	assertOfflineResponse(t, status, body, http.StatusOK, "已保存在线简历 v1", "刷新在线简历")
	first, _ := prepareOfflinePolicySamples(t, runtime)
	draft := generateOfflinePolicyDraft(t, client, server.URL, first.ID)
	validateOfflinePolicyDraft(t, client, server.URL, draft)
	server.Close()
	closeOfflineAcceptanceRuntime(t, runtime)

	assertOfflineDraftNotRestored(t, databasePath, logPath, draft.Text)
}

func TestOfflineUnhealthyRunlogClosesAllExternalWorkerGates(t *testing.T) {
	root := t.TempDir()
	blockedParent := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("conflict"), 0o600); err != nil {
		t.Fatalf("创建 runlog 阻断路径: %v", err)
	}
	runtime := newOfflineAcceptanceRuntime(t, ":memory:", filepath.Join(blockedParent, "boss-job-agent.jsonl"))
	t.Cleanup(func() { closeOfflineAcceptanceRuntime(t, runtime) })
	if runtime.logs.Health().Healthy {
		t.Fatal("runlog 处于健康状态，want fail-closed")
	}
	prepareOfflineUnhealthyWorkerWork(t, runtime)

	runOfflineWorkersForDuration(t, runtime, 100*time.Millisecond)
	assertOfflineNoExternalCalls(t, runtime)
	if err := os.Remove(blockedParent); err != nil {
		t.Fatalf("移除阻断的 runlog 父路径: %v", err)
	}
	if err := os.Mkdir(blockedParent, 0o700); err != nil {
		t.Fatalf("恢复 runlog 父目录: %v", err)
	}
	health := runtime.logs.Recheck(t.Context(), runlog.RepairDecision{})
	if !health.Healthy {
		t.Fatalf("runlog 恢复后健康状态 = %#v，want healthy", health)
	}
	runOfflineWorkerForDuration(t, runtime.service.assessment.Run, 100*time.Millisecond)
	runOfflineWorkerForDuration(t, runtime.service.discovery.Run, 100*time.Millisecond)
	runOfflineWorkerForDuration(t, runtime.service.outreach.Run, 100*time.Millisecond)
	if runtime.discovery.list == 0 || runtime.discovery.read == 0 || len(runtime.assessment.requests) == 0 || len(runtime.outreach.sends) == 0 {
		t.Fatalf("runlog 恢复后后台外部调用 = discovery %d/%d, assessment %d, outreach %d，want 三条流程继续执行",
			runtime.discovery.list, runtime.discovery.read, len(runtime.assessment.requests), len(runtime.outreach.sends))
	}
}

func TestOfflineMigrationKeepsFiveBusinessTablesAndNoSimulationContract(t *testing.T) {
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("打开迁移数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assertOfflineBusinessTables(t, db)
	assertOfflineProductionContract(t)
}

func completeOfflineDiscovery(t *testing.T, runtime *offlineAcceptanceRuntime) (*httptest.Server, jobpool.JobView) {
	t.Helper()
	server := httptest.NewServer(runtime.handler)
	client := server.Client()
	status, body := offlinePostFormBody(t, client, server.URL+"/resume/refresh", "")
	assertOfflineResponse(t, status, body, http.StatusOK, "已保存在线简历 v1", "刷新在线简历")
	if runtime.resume.calls != 1 {
		t.Fatalf("受控在线简历读取次数 = %d，want 1", runtime.resume.calls)
	}
	runtime.discovery.readStarted = make(chan struct{})
	runtime.discovery.releaseRead = make(chan struct{})
	startOfflineDiscovery(t, client, server.URL)
	runOfflineDiscoveryWhilePageCloses(t, runtime, server)
	if runtime.discovery.list != 1 || runtime.discovery.read != 1 {
		t.Fatalf("岗位发现受控调用 = list %d/read %d，want 1/1", runtime.discovery.list, runtime.discovery.read)
	}

	server = httptest.NewServer(runtime.handler)
	status, body = offlineGetBody(t, server.Client(), server.URL+"/jobs")
	assertOfflineResponse(t, status, body, http.StatusOK, "Go 后端工程师", "恢复后的岗位页")
	jobs, err := runtime.pool.ListJobs(t.Context())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("读取发现岗位 = %#v err=%v", jobs, err)
	}
	return server, jobs[0]
}

func runOfflineDiscoveryWhilePageCloses(t *testing.T, runtime *offlineAcceptanceRuntime, server *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runtime.service.discovery.Run(ctx)
		close(done)
	}()
	started := time.NewTimer(5 * time.Second)
	defer started.Stop()
	select {
	case <-runtime.discovery.readStarted:
		server.Close()
		close(runtime.discovery.releaseRead)
	case <-started.C:
		cancel()
		<-done
		t.Fatal("岗位发现未在关闭页面前开始外部读取")
	}
	waitOfflineModuleUntil(t, done, cancel, func() bool {
		run, err := runtime.service.discovery.GetLatestRun(t.Context())
		return err == nil && run != nil && run.Status == discovery.StatusCompleted
	})
	cancel()
	<-done
}

func startOfflineDiscovery(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	status, body := offlinePostJSONBody(t, client, baseURL+"/api/discovery-runs", `{}`)
	var startResult struct {
		DiscoveryRunID int64 `json:"discoveryRunId"`
	}
	if err := json.Unmarshal([]byte(body), &startResult); err != nil {
		t.Fatalf("读取岗位发现运行编号: %v", err)
	}
	if status != http.StatusCreated || startResult.DiscoveryRunID == 0 {
		t.Fatalf("创建岗位发现运行响应 = %d %#v", status, startResult)
	}
}

func completeOfflineAssessment(t *testing.T, runtime *offlineAcceptanceRuntime, server *httptest.Server, job jobpool.JobView) (*httptest.Server, jobpool.JobView) {
	t.Helper()
	status, body := offlinePostJSONBody(t, server.Client(), server.URL+"/api/assessments", fmt.Sprintf(`{"jobIds":[%d]}`, job.ID))
	assertOfflineResponse(t, status, body, http.StatusOK, `"succeeded":1`, "安排 AI 鉴定")
	server.Close()
	runOfflineModuleUntil(t, runtime.service.assessment.Run, func() bool {
		current, err := runtime.pool.GetJob(t.Context(), job.ID)
		return err == nil && current.AssessmentStatus == jobpool.AssessmentStatusSuitable
	})
	if len(runtime.assessment.requests) != 1 {
		t.Fatalf("受控 Pi 请求次数 = %d，want 1", len(runtime.assessment.requests))
	}
	return httptest.NewServer(runtime.handler), job
}

func completeOfflineOutreach(t *testing.T, runtime *offlineAcceptanceRuntime, server *httptest.Server, job jobpool.JobView) {
	t.Helper()
	client := server.Client()
	status, body := offlinePostFormBody(t, client, fmt.Sprintf("%s/jobs/%d/review", server.URL, job.ID),
		"jdHash="+job.JDHash+"&verdict=suitable")
	assertOfflineResponse(t, status, body, http.StatusOK, "", "人工复核")
	status, body = offlinePostJSONBody(t, client, server.URL+"/api/outreach/settings", `{
		"automaticOutreachEnabled":false,
		"greetingText":"您好，想和您聊聊",
		"timeWindows":[],
		"confirmation":{"confirmed":false}
	}`)
	assertOfflineResponse(t, status, body, http.StatusOK, "您好，想和您聊聊", "保存关闭状态下的固定招呼语")
	if len(runtime.outreach.sends) != 0 {
		t.Fatalf("未授权前受控外部写调用次数 = %d，want 0", len(runtime.outreach.sends))
	}

	status, body = offlinePostJSONBody(t, client, server.URL+"/api/outreach/real", fmt.Sprintf(`{
		"jobIds":[%d],
		"confirmation":{"jobCount":1,"greetingText":"您好，想和您聊聊","timeDescription":"全天可打招呼","confirmed":true}
	}`, job.ID))
	assertOfflineResponse(t, status, body, http.StatusOK, `"succeeded":1`, "确认真实打招呼")
	server.Close()
	runOfflineModuleUntil(t, runtime.service.outreach.Run, func() bool {
		current, err := runtime.pool.GetJob(t.Context(), job.ID)
		return err == nil && current.OutreachStatus == jobpool.OutreachStatusContacted
	})
	if len(runtime.outreach.sends) != 1 || runtime.outreach.sends[0].GreetingText != "您好，想和您聊聊" {
		t.Fatalf("受控首次打招呼调用 = %#v，want 1 次且使用冻结招呼语", runtime.outreach.sends)
	}
}

func prepareOfflinePolicySamples(t *testing.T, runtime *offlineAcceptanceRuntime) (jobpool.JobView, jobpool.JobView) {
	t.Helper()
	first := observeOfflineJob(t, runtime, "offline-policy-1", "Go 后端工程师")
	second := observeOfflineJob(t, runtime, "offline-policy-2", "Java 后端工程师")
	if err := runtime.pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: first.ID, ExpectedJDHash: first.JDHash, Verdict: jobpool.HumanVerdictSuitable,
	}, {
		JobID: second.ID, ExpectedJDHash: second.JDHash, Verdict: jobpool.HumanVerdictUnsuitable,
	}}); err != nil {
		t.Fatalf("保存策略样本人工复核: %v", err)
	}
	return first, second
}

func generateOfflinePolicyDraft(t *testing.T, client *http.Client, baseURL string, jobID int64) assessment.PolicyDraft {
	t.Helper()
	status, body := offlinePostJSONBody(t, client, baseURL+"/api/policy/draft", fmt.Sprintf(`{"jobIds":[%d],"validationEnabled":true}`, jobID))
	if status != http.StatusOK {
		t.Fatalf("生成策略候选稿响应 = %d %s", status, body)
	}
	var draft assessment.PolicyDraft
	if err := json.Unmarshal([]byte(body), &draft); err != nil {
		t.Fatalf("读取策略候选稿: %v", err)
	}
	if !draft.ValidationEnabled || draft.GenerationSampleCount != 1 || draft.Text == "" {
		t.Fatalf("策略候选稿 = %#v，want 页面会话候选稿", draft)
	}
	return draft
}

func validateOfflinePolicyDraft(t *testing.T, client *http.Client, baseURL string, draft assessment.PolicyDraft) {
	t.Helper()
	status, body := offlinePostJSONBody(t, client, baseURL+"/api/policy/validate", offlineJSON(t, draft))
	if status != http.StatusOK {
		t.Fatalf("验收策略候选稿响应 = %d %s", status, body)
	}
	var report assessment.PolicyValidationReport
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("读取策略验收报告: %v", err)
	}
	if report.Status != assessment.PolicyValidationPassed || len(report.FullResults) != 2 || len(report.UngeneratedResults) != 1 {
		t.Fatalf("策略验收报告 = %#v，want 全量 2 条且未生成样本 1 条", report)
	}
}

func assertOfflineDraftNotRestored(t *testing.T, databasePath, logPath, draftText string) {
	t.Helper()
	restarted := newOfflineAcceptanceRuntime(t, databasePath, logPath)
	t.Cleanup(func() { closeOfflineAcceptanceRuntime(t, restarted) })
	server := httptest.NewServer(restarted.handler)
	t.Cleanup(server.Close)
	status, body := offlineGetBody(t, server.Client(), server.URL+"/assessments")
	if status != http.StatusOK || strings.Contains(body, draftText) {
		t.Fatalf("重启后岗位鉴定页 = %d %s，候选稿不应恢复", status, body)
	}
}

func prepareOfflineUnhealthyWorkerWork(t *testing.T, runtime *offlineAcceptanceRuntime) {
	t.Helper()
	if err := seedOfflineResume(t, runtime.db); err != nil {
		t.Fatalf("准备离线在线简历: %v", err)
	}
	runID, err := runtime.service.discovery.Start(t.Context())
	if err != nil {
		t.Fatalf("创建 runlog 不健康时的发现运行: %v", err)
	}
	job := observeOfflineJobWithRun(t, runtime, runID, "offline-health-job", "Go 后端工程师")
	if _, err := runtime.pool.QueueAssessments(t.Context(), []int64{job.ID}); err != nil {
		t.Fatalf("准备鉴定队列: %v", err)
	}
	if err := runtime.pool.Review(t.Context(), []jobpool.ReviewDecision{{
		JobID: job.ID, ExpectedJDHash: job.JDHash, Verdict: jobpool.HumanVerdictSuitable,
	}}); err != nil {
		t.Fatalf("准备打招呼资格: %v", err)
	}
	if _, err := runtime.pool.QueueAuthorizedOutreach(t.Context(), []int64{job.ID}, jobpool.OutreachAuthorization{
		GreetingText: "您好，想和您聊聊", TimeDescription: "全天可打招呼",
	}); err != nil {
		t.Fatalf("准备打招呼队列: %v", err)
	}
}

func assertOfflineNoExternalCalls(t *testing.T, runtime *offlineAcceptanceRuntime) {
	t.Helper()
	if runtime.discovery.list != 0 || runtime.discovery.read != 0 || len(runtime.assessment.requests) != 0 || len(runtime.outreach.checks) != 0 || len(runtime.outreach.sends) != 0 {
		t.Fatalf("runlog 不健康时受控外部调用 = discovery %d/%d, assessment %d, outreach %d/%d，want 全部为 0",
			runtime.discovery.list, runtime.discovery.read, len(runtime.assessment.requests), len(runtime.outreach.checks), len(runtime.outreach.sends))
	}
}

func observeOfflineJob(t *testing.T, runtime *offlineAcceptanceRuntime, platformJobID, title string) jobpool.JobView {
	t.Helper()
	return observeOfflineJobWithRun(t, runtime, 1, platformJobID, title)
}

func observeOfflineJobWithRun(t *testing.T, runtime *offlineAcceptanceRuntime, runID int64, platformJobID, title string) jobpool.JobView {
	t.Helper()
	observation := offlineJobObservation()
	observation.PlatformJobID = platformJobID
	observation.CanonicalURL = "https://www.zhipin.com/job_detail/" + platformJobID + ".html"
	observation.JobTitle = title
	job, err := runtime.pool.Observe(t.Context(), runID, jobpool.Observation{
		PlatformJobID: observation.PlatformJobID, CanonicalURL: observation.CanonicalURL,
		JobTitle: observation.JobTitle, CompanyName: observation.CompanyName, City: observation.City,
		Salary: observation.Salary, Responsibilities: observation.Responsibilities,
		Requirements: observation.Requirements, PlatformStatus: jobpool.PlatformStatusOpen,
		ObservedAt: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatalf("保存受控岗位 %s: %v", platformJobID, err)
	}
	return job
}

func seedOfflineResume(t *testing.T, db *sql.DB) error {
	t.Helper()
	encoded, err := json.Marshal(offlineResumeContent())
	if err != nil {
		return err
	}
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (version_no, resume_json, resume_hash, is_current, created_at)
		VALUES (1, ?, 'offline-resume-v1', 1, 1000)
	`, string(encoded))
	return err
}

func runOfflineWorkersForDuration(t *testing.T, runtime *offlineAcceptanceRuntime, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{}, 3)
	go func() { runtime.service.discovery.Run(ctx); done <- struct{}{} }()
	go func() { runtime.service.assessment.Run(ctx); done <- struct{}{} }()
	go func() { runtime.service.outreach.Run(ctx); done <- struct{}{} }()
	timer := time.NewTimer(duration)
	<-timer.C
	cancel()
	for range 3 {
		<-done
	}
}

func runOfflineWorkerForDuration(t *testing.T, run func(context.Context), duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		run(ctx)
		close(done)
	}()
	timer := time.NewTimer(duration)
	<-timer.C
	cancel()
	<-done
}

func assertOfflineRestartRestoresState(t *testing.T, databasePath, logPath string) {
	t.Helper()
	restarted := newOfflineAcceptanceRuntime(t, databasePath, logPath)
	t.Cleanup(func() { closeOfflineAcceptanceRuntime(t, restarted) })
	server := httptest.NewServer(restarted.handler)
	t.Cleanup(server.Close)
	status, body := offlineGetBody(t, server.Client(), server.URL+"/api/startup-state")
	if status != http.StatusOK {
		t.Fatalf("重启后启动状态响应 = %d %s", status, body)
	}
	if !strings.Contains(body, `"version":1`) || !strings.Contains(body, `"automaticAssessmentEnabled":false`) {
		t.Fatalf("重启后启动状态未恢复正式设置 = %s", body)
	}
	restored, err := restarted.pool.ListJobs(t.Context())
	if err != nil || len(restored) != 1 || restored[0].OutreachStatus != jobpool.OutreachStatusContacted {
		t.Fatalf("重启后岗位状态 = %#v err=%v，want contacted", restored, err)
	}
	if len(restarted.outreach.sends) != 0 || len(restarted.assessment.requests) != 0 {
		t.Fatalf("重启后重复外部写/鉴定调用 = sends %d assessments %d，want 0/0", len(restarted.outreach.sends), len(restarted.assessment.requests))
	}
}

func assertOfflineBusinessTables(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
		SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version'
		ORDER BY name
	`)
	if err != nil {
		t.Fatalf("读取业务表: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("关闭业务表查询: %v", err)
		}
	}()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读取业务表名称: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历业务表: %v", err)
	}
	if got := strings.Join(tables, ","); got != "assessment_policy_versions,automation_settings,discovery_runs,online_resume_versions,platform_jobs" {
		t.Fatalf("业务表 = %q，want 五张正式业务表", got)
	}
}

func assertOfflineProductionContract(t *testing.T) {
	t.Helper()
	files := []string{
		"cmd/boss-job-agent/main.go",
		"internal/automationsettings/settings.go",
		"internal/webui/templates/page.html",
		"internal/webui/assets/app.js",
		"internal/webui/assets/app.css",
		"internal/sqlite/migrations/00001_initial.sql",
		"internal/sqlite/migrations/00002_allow_unprepared_discovery_termination.sql",
		"internal/sqlite/migrations/00003_add_per_job_discovery_checkpoint.sql",
	}
	for _, relativePath := range files {
		// 路径只来自上面的固定列表，测试不接受用户输入的路径。
		content, err := os.ReadFile(filepath.Join(repositoryRootForTest(t), relativePath)) //nolint:gosec // fixed repository contract paths
		if err != nil {
			t.Fatalf("读取 %s: %v", relativePath, err)
		}
		lower := strings.ToLower(string(content))
		if strings.Contains(lower, "simulation") || strings.Contains(string(content), "模拟") {
			t.Errorf("%s 暴露了已删除的 Simulation/模拟契约", relativePath)
		}
	}
}

func assertOfflineResponse(t *testing.T, status int, body string, wantStatus int, wantText, operation string) {
	t.Helper()
	if status != wantStatus || !strings.Contains(body, wantText) {
		t.Fatalf("%s响应 = %d %s，want %d 且包含 %q", operation, status, body, wantStatus, wantText)
	}
}

func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("读取测试工作目录: %v", err)
	}
	return filepath.Clean(filepath.Join(workingDirectory, "../.."))
}

func runOfflineModuleUntil(t *testing.T, run func(context.Context), ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		run(ctx)
		close(done)
	}()
	waitOfflineModuleUntil(t, done, cancel, ready)
	cancel()
	<-done
}

func waitOfflineModuleUntil(t *testing.T, done <-chan struct{}, cancel context.CancelFunc, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-deadline.C:
			cancel()
			<-done
			t.Fatal("等待离线 Worker 完成超时")
		case <-ticker.C:
		}
	}
}

func offlinePostFormBody(t *testing.T, client *http.Client, target, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("创建表单请求: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return offlineDoBody(t, client, request)
}

func offlinePostJSONBody(t *testing.T, client *http.Client, target, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("创建 JSON 请求: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	return offlineDoBody(t, client, request)
}

func offlineGetBody(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("创建 GET 请求: %v", err)
	}
	return offlineDoBody(t, client, request)
}

func offlineJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码离线 JSON: %v", err)
	}
	return string(encoded)
}

func offlineDoBody(t *testing.T, client *http.Client, request *http.Request) (int, string) {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("发送 GET 请求: %v", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("关闭 HTTP 响应: %v", err)
		}
	}()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取 HTTP 响应: %v", err)
	}
	return response.StatusCode, string(content)
}
