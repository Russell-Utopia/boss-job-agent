package boss

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
)

func TestJobDiscoveryFetchesOneCompleteReliablePageThroughKimiWebBridge(t *testing.T) {
	t.Parallel()

	want := discovery.DiscoveryPage{
		Observations: []discovery.JobObservation{{
			PlatformJobID:    "boss-job-1",
			CanonicalURL:     "https://www.zhipin.com/job_detail/boss-job-1.html",
			JobTitle:         "Go 后端工程师",
			CompanyName:      "示例科技",
			City:             "福州",
			Salary:           "20-30K",
			Responsibilities: "负责 Go 服务开发",
			Requirements:     "熟悉 Go 与 SQLite",
			PlatformStatus:   discovery.PlatformStatusOpen,
		}},
		HasMore: false,
	}
	rawPage := map[string]any{
		"jobs": []map[string]any{{
			"platformJobId":          "boss-job-1",
			"detailPlatformJobId":    "boss-job-1",
			"platformStatusEvidence": "招聘中",
			"canonicalUrl":           "https://www.zhipin.com/job_detail/boss-job-1.html",
			"jobTitle":               "Go 后端工程师",
			"companyName":            "示例科技",
			"city":                   "福州",
			"salary":                 "20-30K",
			"fullJD":                 "负责 Go 服务开发\n任职要求：熟悉 Go 与 SQLite",
		}},
		"hasMore": false,
	}
	encoded, err := json.Marshal(rawPage)
	if err != nil {
		t.Fatalf("encode discovery page fixture: %v", err)
	}
	fixture := &successfulJobDiscoveryFixture{t: t, encodedPage: string(encoded)}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	adapter := NewJobDiscovery(server.URL, server.Client())
	searchRange := discovery.SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}

	got, err := adapter.FetchPage(t.Context(), searchRange, 3)
	if err != nil {
		t.Fatalf("fetch discovery page: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("discovery page = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(fixture.actions, []string{"find_tab", "navigate", "evaluate"}) {
		t.Errorf("WebBridge actions = %#v", fixture.actions)
	}
	for _, expected := range []string{
		"Go 后端工程师", "福州", "20-30K", "全职", `"page":3`,
		"/wapi/zpgeek/search/joblist.json", "/wapi/zpgeek/job/detail.json", "hasMore",
		"resolveSalaryOption", "conditions.salaryList", "requestOrdinal++",
		`request("city_metadata", 0`, `request("filter_conditions", 0`,
		`request("job_list", 0`, `request("job_detail", detailOrdinal`,
	} {
		if !strings.Contains(fixture.evaluationScript, expected) {
			t.Errorf("evaluation script does not contain %q", expected)
		}
	}
}

func TestJobDiscoveryRejectsTheWholePageWhenOneObservationIsUnreliable(t *testing.T) {
	t.Parallel()

	page := discovery.DiscoveryPage{
		Observations: []discovery.JobObservation{{
			JobTitle: "缺少稳定 ID 的岗位",
		}},
		HasMore: false,
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("encode unreliable discovery page: %v", err)
	}
	fixture := &successfulJobDiscoveryFixture{t: t, encodedPage: string(encoded)}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)

	_, err = NewJobDiscovery(server.URL, server.Client()).FetchPage(t.Context(), discovery.SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}, 1)
	var fetchErr *discovery.FetchError
	if !errors.As(err, &fetchErr) {
		t.Fatalf("fetch error = %v, want discovery.FetchError", err)
	}
	if fetchErr.Category != discovery.FetchErrorInvalidResponse {
		t.Errorf("fetch error category = %q, want invalid_response", fetchErr.Category)
	}
}

func TestJobDiscoveryClassifiesAndPreservesFirstFailedRequestEvidence(t *testing.T) {
	t.Parallel()

	err := classifyDiscoveryError(errors.New(
		"extension_error: evaluate: Error: BOSS_PLATFORM_LIMITED" +
			"|request_ordinal=7|stage=job_detail|detail_ordinal=4|upstream_code=37",
	))
	var fetchErr *discovery.FetchError
	if !errors.As(err, &fetchErr) {
		t.Fatalf("fetch error = %v, want discovery.FetchError", err)
	}
	if fetchErr.Category != discovery.FetchErrorPlatformLimited {
		t.Errorf("category = %q, want platform_limited", fetchErr.Category)
	}
	want := &discovery.FetchFailureEvidence{
		RequestOrdinal: 7,
		Stage:          "job_detail",
		DetailOrdinal:  4,
		UpstreamCode:   "37",
	}
	if !reflect.DeepEqual(fetchErr.Evidence, want) {
		t.Errorf("failure evidence = %#v, want %#v", fetchErr.Evidence, want)
	}
}

func TestJobDiscoveryRejectsAmbiguousJDAndUnconfirmedLiveDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		fullJD              string
		detailPlatformJobID string
		platformStatus      string
	}{
		{
			name: "JD has no reliable responsibility requirement boundary", fullJD: "负责 Go 服务开发",
			detailPlatformJobID: "boss-job-1", platformStatus: "招聘中",
		},
		{
			name: "live detail does not confirm the listed stable ID", fullJD: "负责 Go 服务开发\n任职要求：熟悉 Go",
			detailPlatformJobID: "another-job", platformStatus: "招聘中",
		},
		{
			name: "live detail does not confirm the job is open", fullJD: "负责 Go 服务开发\n任职要求：熟悉 Go",
			detailPlatformJobID: "boss-job-1", platformStatus: "已关闭",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rawPage := map[string]any{
				"jobs": []map[string]any{{
					"platformJobId": "boss-job-1", "detailPlatformJobId": test.detailPlatformJobID,
					"platformStatusEvidence": test.platformStatus,
					"canonicalUrl":           "https://www.zhipin.com/job_detail/boss-job-1.html",
					"jobTitle":               "Go 后端工程师", "companyName": "示例科技",
					"city": "福州", "salary": "20-30K", "fullJD": test.fullJD,
				}},
				"hasMore": false,
			}
			encoded, err := json.Marshal(rawPage)
			if err != nil {
				t.Fatalf("encode raw page: %v", err)
			}
			fixture := &successfulJobDiscoveryFixture{t: t, encodedPage: string(encoded)}
			server := httptest.NewServer(fixture)
			t.Cleanup(server.Close)

			_, err = NewJobDiscovery(server.URL, server.Client()).FetchPage(t.Context(), discovery.SearchRange{
				Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
			}, 1)
			var fetchErr *discovery.FetchError
			if !errors.As(err, &fetchErr) || fetchErr.Category != discovery.FetchErrorInvalidResponse {
				t.Fatalf("fetch error = %v, want invalid_response", err)
			}
		})
	}
}

type successfulJobDiscoveryFixture struct {
	t                *testing.T
	encodedPage      string
	actions          []string
	evaluationScript string
}

func (f *successfulJobDiscoveryFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if command.Session != jobDiscoverySession {
		f.t.Errorf("session = %q, want %q", command.Session, jobDiscoverySession)
	}
	switch command.Action {
	case "find_tab":
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": false,
			"error": map[string]any{
				"code": "extension_error", "message": "find_tab: no tab matching BOSS job search",
			},
		})
	case "navigate":
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"success": true, "url": jobSearchURL, "tabId": 43},
		})
	case "evaluate":
		f.evaluationScript, _ = command.Args["code"].(string)
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"type": "string", "value": f.encodedPage},
		})
	default:
		f.t.Errorf("unexpected WebBridge action %q", command.Action)
		http.Error(w, "unexpected action", http.StatusBadRequest)
	}
}
