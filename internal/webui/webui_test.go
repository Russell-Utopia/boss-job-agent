package webui

import (
	"database/sql"
	"encoding/json"
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
	Handler http.Handler
	db      *sql.DB
	logs    *runlog.Log
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
	pool := jobpool.New()
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
	resumeVersions := onlineresume.New(db)
	logs := runlog.Open(logPath)
	return &testWeb{
		Handler: New(Dependencies{
			Resume:     resumeVersions,
			Discovery:  discovery.New(resumeVersions),
			Assessment: assessmentService,
			Settings:   settings,
			Runlog:     logs,
		}),
		db:   db,
		logs: logs,
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
