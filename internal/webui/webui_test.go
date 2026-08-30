package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/application"
)

func TestFirstUseWebProvidesFourStableEntriesAndSafeState(t *testing.T) {
	t.Parallel()

	app, err := application.Open(context.Background(), application.Config{DatabasePath: ":memory:"})
	if err != nil {
		t.Fatalf("open application: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("close application: %v", err)
		}
	})

	server := httptest.NewServer(New(app))
	t.Cleanup(server.Close)

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
			response, err := http.Get(server.URL + page.path)
			if err != nil {
				t.Fatalf("get page: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.StatusCode)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			text := string(body)
			for _, entry := range []string{
				`href="/jobs"`,
				`href="/assessments"`,
				`href="/outreach"`,
				`href="/resume"`,
			} {
				if !strings.Contains(text, entry) {
					t.Errorf("body does not contain navigation entry %q", entry)
				}
			}
			if strings.Contains(text, "执行情况") {
				t.Error("body contains a standalone execution-status entry")
			}
			if strings.Contains(text, "Simulation") || strings.Contains(text, "模拟打招呼") {
				t.Error("body exposes the removed simulation product capability")
			}
			for _, want := range page.want {
				if !strings.Contains(text, want) {
					t.Errorf("body does not contain %q", want)
				}
			}
		})
	}
}

func TestRemovedSimulationCommandIsNotRoutable(t *testing.T) {
	t.Parallel()

	app, err := application.Open(context.Background(), application.Config{DatabasePath: ":memory:"})
	if err != nil {
		t.Fatalf("open application: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewServer(New(app))
	t.Cleanup(server.Close)

	response, err := http.Post(server.URL+"/api/outreach/simulation", "application/json", strings.NewReader(`{"jobIds":[]}`))
	if err != nil {
		t.Fatalf("post removed simulation command: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func TestWebCommandsReturnBusinessRejections(t *testing.T) {
	t.Parallel()

	app, err := application.Open(context.Background(), application.Config{DatabasePath: ":memory:"})
	if err != nil {
		t.Fatalf("open application: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewServer(New(app))
	t.Cleanup(server.Close)

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
			response, err := http.Post(server.URL+command.path, "application/json", strings.NewReader(command.body))
			if err != nil {
				t.Fatalf("post command: %v", err)
			}
			defer response.Body.Close()
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
	first, err := application.Open(context.Background(), application.Config{DatabasePath: path})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first application: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	if _, err := tx.Exec(`UPDATE assessment_policy_versions SET is_active = 0 WHERE version_no = 1`); err != nil {
		t.Fatalf("deactivate default policy: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (2, '{"rules":["用户保存的策略"]}', 1, '用户采用', 2000)
	`); err != nil {
		t.Fatalf("insert saved policy: %v", err)
	}
	if _, err := tx.Exec(`
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

	restarted, err := application.Open(context.Background(), application.Config{DatabasePath: path})
	if err != nil {
		t.Fatalf("restart application: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	server := httptest.NewServer(New(restarted))
	t.Cleanup(server.Close)

	assertPageContains(t, server.URL+"/assessments", []string{
		"<h2>策略 v2</h2>",
		"<dd>已开启</dd>",
		"<dd>12</dd>",
	})
	assertPageContains(t, server.URL+"/outreach", []string{
		"<dd>您好，想和您聊聊这个岗位</dd>",
		"<dd>按已配置时间段打招呼</dd>",
	})
}

func TestWebPagesDoNotDependOnUnrelatedDownstreamState(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "boss-job-agent.db")
	app, err := application.Open(context.Background(), application.Config{DatabasePath: path})
	if err != nil {
		t.Fatalf("open application: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewServer(New(app))
	t.Cleanup(server.Close)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`DELETE FROM assessment_policy_versions`); err != nil {
		t.Fatalf("remove unrelated policy: %v", err)
	}

	assertPageStatus(t, server.URL+"/jobs", http.StatusOK)
	assertPageStatus(t, server.URL+"/outreach", http.StatusOK)
	assertPageStatus(t, server.URL+"/resume", http.StatusOK)

	if _, err := db.Exec(`DELETE FROM automation_settings`); err != nil {
		t.Fatalf("remove unrelated automation settings: %v", err)
	}
	assertPageStatus(t, server.URL+"/jobs", http.StatusOK)
	assertPageStatus(t, server.URL+"/resume", http.StatusOK)
}

func assertPageContains(t *testing.T, url string, wants []string) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	for _, want := range wants {
		if !strings.Contains(string(body), want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func assertPageStatus(t *testing.T, url string, want int) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Errorf("page status = %d, want %d", response.StatusCode, want)
	}
}
