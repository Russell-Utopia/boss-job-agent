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

func (d *webJobDiscovery) FetchPage(
	context.Context,
	discovery.SearchRange,
	int,
) (discovery.DiscoveryPage, error) {
	return discovery.DiscoveryPage{
		Observations: []discovery.JobObservation{
			webDiscoveredJob("boss-job-1", "Go 后端工程师", "示例科技"),
			webDiscoveredJob("boss-job-2", "Go 平台工程师", "另一科技"),
		},
		HasMore: false,
	}, nil
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

func openTestWeb(t *testing.T, path string) *testWeb {
	t.Helper()
	return openTestWebWithLogPath(t, path, filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
}

func openTestWebWithLogPath(t *testing.T, databasePath, logPath string) *testWeb {
	t.Helper()
	db, err := storage.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	pool := jobpool.New(db)
	settings := automationsettings.New(db, pool)
	assessmentService := assessment.New(db)
	now := time.UnixMilli(1000)
	if err := assessmentService.EnsureDefaultPolicy(t.Context(), now); err != nil {
		_ = db.Close()
		t.Fatalf("ensure default policy: %v", err)
	}
	if err := settings.EnsureSafeDefaults(t.Context(), now); err != nil {
		_ = db.Close()
		t.Fatalf("ensure safe automation settings: %v", err)
	}
	logs := runlog.Open(logPath)
	resumeReader := &webResumeReader{content: webResumeContent("Go 后端工程师")}
	resumeVersions := onlineresume.New(db, resumeReader, logs, func() time.Time { return now })
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
		"<dd>按已配置时间段打招呼</dd>",
		"当前没有可真实打招呼的岗位",
	})
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

func postFormResponse(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(""))
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
	for _, observation := range []discovery.JobObservation{
		webDiscoveredJob("boss-job-1", "Go 后端工程师", "示例科技"),
		webDiscoveredJob("boss-job-2", "Go 平台工程师", "另一科技"),
	} {
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
