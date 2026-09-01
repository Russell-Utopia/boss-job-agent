package boss

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/outreach"
)

type outreachWebBridgeFixture struct {
	t             *testing.T
	actions       []string
	evaluation    []string
	checkResponse string
	sendResponse  string
}

func (f *outreachWebBridgeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if command.Session != outreachSession {
		f.t.Errorf("session = %q, want %q", command.Session, outreachSession)
	}
	switch command.Action {
	case "find_tab":
		writeFixtureJSON(f.t, w, map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "extension_error", "message": "no tab matching job"},
		})
	case "navigate":
		if command.Args["url"] != "https://www.zhipin.com/job_detail/boss-job-1.html" {
			f.t.Errorf("navigate URL = %#v", command.Args["url"])
		}
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"success": true, "url": command.Args["url"], "tabId": 42},
		})
	case "evaluate":
		code, _ := command.Args["code"].(string)
		f.evaluation = append(f.evaluation, code)
		response := f.checkResponse
		if len(f.evaluation) > 1 {
			response = f.sendResponse
		}
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"type": "string", "value": response},
		})
	default:
		f.t.Errorf("unexpected WebBridge action %q", command.Action)
		http.Error(w, "unexpected action", http.StatusBadRequest)
	}
}

func TestOutreachAdapterChecksAndSendsThroughOneBOSSWebBridgeSession(t *testing.T) {
	t.Parallel()

	fixture := &outreachWebBridgeFixture{
		t:             t,
		checkResponse: `{"platformJobId":"boss-job-1","open":true,"alreadyContacted":false,"evidence":{"page":"job"}}`,
		sendResponse:  `{"platformJobId":"boss-job-1","effect":"confirmed_sent","evidence":{"message":"您好，想聊聊"}}`,
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	adapter := NewOutreach(server.URL, server.Client())

	status := mustCheckOutreach(t, adapter)
	if !status.Open || status.AlreadyContacted {
		t.Errorf("contact status = %#v, want open and not contacted", status)
	}
	result := mustSendOutreach(t, adapter)
	if result.Effect != outreach.OutreachEffectConfirmedSent || !json.Valid(result.Evidence) {
		t.Errorf("send result = %#v, want confirmed sent with evidence", result)
	}
	assertOutreachFixture(t, fixture)
}

func mustCheckOutreach(t *testing.T, adapter *Outreach) outreach.ContactStatus {
	t.Helper()
	status, err := adapter.Check(t.Context(), outreach.PlatformJobRef{
		PlatformJobID: "boss-job-1", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html",
	})
	if err != nil {
		t.Fatalf("check contact status: %v", err)
	}
	return status
}

func mustSendOutreach(t *testing.T, adapter *Outreach) outreach.FirstContactResult {
	t.Helper()
	result, err := adapter.Send(t.Context(), outreach.FirstContactRequest{
		PlatformJobID: "boss-job-1", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html", GreetingText: "您好，想聊聊",
	})
	if err != nil {
		t.Fatalf("send first contact: %v", err)
	}
	return result
}

func assertOutreachFixture(t *testing.T, fixture *outreachWebBridgeFixture) {
	t.Helper()
	if strings.Join(fixture.actions, ",") != "find_tab,navigate,evaluate,find_tab,navigate,evaluate" {
		t.Errorf("WebBridge actions = %#v", fixture.actions)
	}
	if len(fixture.evaluation) != 2 {
		t.Errorf("evaluation count = %d, want 2", len(fixture.evaluation))
		return
	}
	if !strings.Contains(fixture.evaluation[0], "boss-job-1") || !strings.Contains(fixture.evaluation[1], "您好，想聊聊") {
		t.Errorf("evaluation scripts do not contain trusted request data: %#v", fixture.evaluation)
	}
}

func TestOutreachAdapterClassifiesAuthenticationFailureAndRejectsInvalidResult(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Errorf("decode command: %v", err)
			return
		}
		if command.Action == "find_tab" || command.Action == "navigate" {
			writeFixtureJSON(t, w, map[string]any{"ok": true, "data": map[string]any{"success": true, "url": "https://www.zhipin.com/job_detail/boss-job-1.html", "tabId": 42}})
			return
		}
		writeFixtureJSON(t, w, map[string]any{"ok": false, "error": map[string]any{"code": "extension_error", "message": "BOSS_AUTHENTICATION_REQUIRED"}})
	}))
	t.Cleanup(server.Close)
	_, err := NewOutreach(server.URL, server.Client()).Check(t.Context(), outreach.PlatformJobRef{
		PlatformJobID: "boss-job-1", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html",
	})
	var actionErr *outreach.ActionError
	if !errors.As(err, &actionErr) || actionErr.Category != outreach.ErrorAuthenticationExpired {
		t.Fatalf("authentication error = %v, want classified outreach error", err)
	}

	fixture := &outreachWebBridgeFixture{
		t:             t,
		checkResponse: `{"platformJobId":"boss-job-1","effect":"unexpected","evidence":{"message":"bad"}}`,
	}
	badServer := httptest.NewServer(fixture)
	t.Cleanup(badServer.Close)
	badResult, err := NewOutreach(badServer.URL, badServer.Client()).Send(t.Context(), outreach.FirstContactRequest{
		PlatformJobID: "boss-job-1", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html", GreetingText: "您好",
	})
	if err == nil {
		t.Fatal("invalid send result succeeded")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("invalid send result error = %v, want protocol detail", err)
	}
	if badResult.Effect != outreach.OutreachEffectPossiblyEffective || !json.Valid(badResult.Evidence) {
		t.Errorf("invalid send result = %#v, want possible effect with fallback evidence", badResult)
	}
}
