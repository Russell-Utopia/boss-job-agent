package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		{path: "/outreach", want: []string{"首次沟通", "自动首次沟通", "已关闭", "Simulation", "未配置", "全天可发送", "请先配置固定招呼语"}},
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
			for _, want := range page.want {
				if !strings.Contains(text, want) {
					t.Errorf("body does not contain %q", want)
				}
			}
		})
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
		{path: "/api/outreach/simulation", body: `{"jobIds":[]}`, code: "outreach_greeting_required"},
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
