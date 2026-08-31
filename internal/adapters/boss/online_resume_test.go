package boss

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
)

func TestOnlineResumeReadsCompleteContentThroughKimiWebBridge(t *testing.T) {
	t.Parallel()

	want := onlineresume.ResumeContent{
		JobIntentions: []onlineresume.JobIntention{{
			Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
		}},
		WorkExperiences:    []string{"某公司\n后端工程师\n负责 Go 服务"},
		ProjectExperiences: []string{"招聘助手\n负责状态机"},
		Educations:         []string{"某大学\n计算机本科"},
		Skills:             []string{"Go、SQLite"},
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode resume fixture: %v", err)
	}
	fixture := &successfulWebBridgeFixture{t: t, encodedResume: string(encoded)}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)

	adapter := NewOnlineResume(server.URL, server.Client())
	got, err := adapter.Read(t.Context())
	if err != nil {
		t.Fatalf("read online resume: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("online resume = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(fixture.actions, []string{"find_tab", "navigate", "evaluate"}) {
		t.Errorf("WebBridge actions = %#v", fixture.actions)
	}
	for _, selector := range []string{
		"resume-purpose", "resume-workExpList", "resume-projectExpList",
		"resume-educationExpList", "resume-professionalSkill",
	} {
		if !strings.Contains(fixture.evaluationScript, selector) {
			t.Errorf("evaluation script does not contain %q", selector)
		}
	}
	for _, forbidden := range []string{"resume-userinfo", "fz-tel", "fz-weixin", "fz-mail"} {
		if strings.Contains(fixture.evaluationScript, forbidden) {
			t.Errorf("evaluation script reads forbidden contact selector %q", forbidden)
		}
	}
}

type successfulWebBridgeFixture struct {
	t                *testing.T
	encodedResume    string
	actions          []string
	evaluationScript string
}

func (f *successfulWebBridgeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Action  string         `json:"action"`
		Args    map[string]any `json:"args"`
		Session string         `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
		f.t.Errorf("decode WebBridge command: %v", err)
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	}
	f.actions = append(f.actions, command.Action)
	if command.Session != onlineResumeSession {
		f.t.Errorf("session = %q, want %q", command.Session, onlineResumeSession)
	}
	switch command.Action {
	case "find_tab":
		f.writeMissingTab(w)
	case "navigate":
		f.writeNavigation(w, command.Args)
	case "evaluate":
		f.evaluationScript, _ = command.Args["code"].(string)
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"type": "string", "value": f.encodedResume},
		})
	default:
		f.t.Errorf("unexpected WebBridge action %q", command.Action)
		http.Error(w, "unexpected action", http.StatusBadRequest)
	}
}

func (f *successfulWebBridgeFixture) writeMissingTab(w http.ResponseWriter) {
	writeFixtureJSON(f.t, w, map[string]any{
		"ok": false,
		"error": map[string]any{
			"code": "extension_error", "message": "find_tab: no tab matching resume",
		},
	})
}

func (f *successfulWebBridgeFixture) writeNavigation(w http.ResponseWriter, args map[string]any) {
	if args["url"] != onlineResumeURL {
		f.t.Errorf("navigate URL = %#v, want %q", args["url"], onlineResumeURL)
	}
	if args["newTab"] != true {
		f.t.Errorf("navigate newTab = %#v, want true", args["newTab"])
	}
	writeFixtureJSON(f.t, w, map[string]any{
		"ok": true, "data": map[string]any{"success": true, "url": onlineResumeURL, "tabId": 42},
	})
}

func TestOnlineResumeClassifiesAnExpiredBossLoginWithoutExposingBrowserDetails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Errorf("decode WebBridge command: %v", err)
			http.Error(w, "invalid command", http.StatusBadRequest)
			return
		}
		switch command.Action {
		case "find_tab", "navigate":
			writeFixtureJSON(t, w, map[string]any{
				"ok":   true,
				"data": map[string]any{"success": true, "url": onlineResumeURL, "tabId": 42},
			})
		case "evaluate":
			writeFixtureJSON(t, w, map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":    "extension_error",
					"message": "evaluate failed: BOSS_AUTHENTICATION_REQUIRED cookie=secret",
				},
			})
		default:
			t.Errorf("unexpected WebBridge action %q", command.Action)
		}
	}))
	t.Cleanup(server.Close)

	_, err := NewOnlineResume(server.URL, server.Client()).Read(t.Context())
	var readErr *onlineresume.ReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("read error = %v, want onlineresume.ReadError", err)
	}
	if readErr.Category != onlineresume.ReadErrorAuthenticationExpired {
		t.Errorf("error category = %q, want %q", readErr.Category, onlineresume.ReadErrorAuthenticationExpired)
	}
	if readErr.UserReason != "BOSS 登录已失效，请在 Chrome 重新登录后再刷新" {
		t.Errorf("user reason = %q", readErr.UserReason)
	}
	if strings.Contains(readErr.UserReason, "cookie") {
		t.Errorf("user reason exposes browser details: %q", readErr.UserReason)
	}
}

func TestOnlineResumeClassifiesMalformedNavigationProtocolByCause(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Errorf("decode WebBridge command: %v", err)
			http.Error(w, "invalid command", http.StatusBadRequest)
			return
		}
		if command.Action == "find_tab" {
			writeFixtureJSON(t, w, map[string]any{
				"ok":   true,
				"data": map[string]any{"success": true, "url": onlineResumeURL, "tabId": 42},
			})
			return
		}
		_, _ = io.WriteString(w, `{not-json`)
	}))
	t.Cleanup(server.Close)

	_, err := NewOnlineResume(server.URL, server.Client()).Read(t.Context())
	assertReadErrorCategory(t, err, onlineresume.ReadErrorInvalidProtocol)
}

func TestOnlineResumeClassifiesEvaluateTransportFailureByCause(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var command struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
			t.Fatalf("decode WebBridge command: %v", err)
		}
		if command.Action == "evaluate" {
			return nil, errors.New("WebBridge connection closed")
		}
		body := `{"ok":true,"data":{"success":true,"url":"` + onlineResumeURL + `","tabId":42}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}

	_, err := NewOnlineResume("http://webbridge.test", client).Read(t.Context())
	assertReadErrorCategory(t, err, onlineresume.ReadErrorTransient)
}

func TestOnlineResumeRejectsAnEmptyRequiredSectionItem(t *testing.T) {
	t.Parallel()

	content := onlineresume.ResumeContent{
		JobIntentions: []onlineresume.JobIntention{{
			Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
		}},
		WorkExperiences:    []string{""},
		ProjectExperiences: []string{},
		Educations:         []string{},
		Skills:             []string{},
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("encode resume fixture: %v", err)
	}
	server := httptest.NewServer(&successfulWebBridgeFixture{t: t, encodedResume: string(encoded)})
	t.Cleanup(server.Close)

	_, err = NewOnlineResume(server.URL, server.Client()).Read(t.Context())
	assertReadErrorCategory(t, err, onlineresume.ReadErrorInvalidResponse)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func assertReadErrorCategory(t *testing.T, err error, want onlineresume.ReadErrorCategory) {
	t.Helper()
	var readErr *onlineresume.ReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("read error = %v, want onlineresume.ReadError", err)
	}
	if readErr.Category != want {
		t.Errorf("error category = %q, want %q", readErr.Category, want)
	}
}

func writeFixtureJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write WebBridge fixture: %v", err)
	}
}
