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

func TestJobDiscoveryListsStableIDsThenReadsOneReliableJob(t *testing.T) {
	t.Parallel()

	listJSON := mustEncodeDiscoveryFixture(t, map[string]any{
		"jobs": []map[string]any{{
			"platformJobId": "boss-job-1",
			"securityId":    "ephemeral-security",
			"lid":           "ephemeral-list",
			"jobTitle":      "Go 后端工程师",
			"companyName":   "示例科技",
			"city":          "福州",
			"salary":        "20-30K",
		}},
		"hasMore": false,
	})
	jobJSON := mustEncodeDiscoveryFixture(t, map[string]any{
		"platformJobId":          "boss-job-1",
		"detailPlatformJobId":    "boss-job-1",
		"platformStatusEvidence": "招聘中",
		"canonicalUrl":           "https://www.zhipin.com/job_detail/boss-job-1.html",
		"jobTitle":               "Go 后端工程师",
		"companyName":            "示例科技",
		"city":                   "福州",
		"salary":                 "20-30K",
		"salaryEvidence":         "readable",
		"fullJD":                 "负责 Go 服务开发\n任职要求：熟悉 Go 与 SQLite",
	})
	fixture := &separatedJobDiscoveryFixture{
		t:                 t,
		evaluationResults: []string{listJSON, jobJSON},
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	adapter := NewJobDiscovery(server.URL, server.Client())
	searchRange := discovery.SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}

	page, err := adapter.ListPage(t.Context(), searchRange, 3)
	if err != nil {
		t.Fatalf("list discovery page: %v", err)
	}
	if want := (discovery.JobPage{PlatformJobIDs: []string{"boss-job-1"}, HasMore: false}); !reflect.DeepEqual(page, want) {
		t.Errorf("discovery list = %#v, want %#v", page, want)
	}
	job, err := adapter.ReadJob(t.Context(), "boss-job-1")
	if err != nil {
		t.Fatalf("read discovery job: %v", err)
	}
	wantJob := discovery.JobObservation{
		PlatformJobID: "boss-job-1", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html",
		JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
		FullJD:         "负责 Go 服务开发\n任职要求：熟悉 Go 与 SQLite",
		PlatformStatus: discovery.PlatformStatusOpen,
	}
	if !reflect.DeepEqual(job, wantJob) {
		t.Errorf("discovery job = %#v, want %#v", job, wantJob)
	}
	if !reflect.DeepEqual(fixture.actions, []string{"find_tab", "navigate", "evaluate", "evaluate"}) {
		t.Errorf("WebBridge actions = %#v", fixture.actions)
	}
	if len(fixture.evaluationScripts) != 2 {
		t.Fatalf("evaluation scripts = %d, want 2", len(fixture.evaluationScripts))
	}
	if strings.Contains(fixture.evaluationScripts[0], "/wapi/zpgeek/job/detail.json") {
		t.Error("ListPage script reads job details")
	}
	if !strings.Contains(fixture.evaluationScripts[1], "/wapi/zpgeek/job/detail.json") ||
		strings.Contains(fixture.evaluationScripts[1], "/wapi/zpgeek/search/joblist.json") {
		t.Error("ReadJob script does not isolate one detail read")
	}
}

func TestJobDiscoveryPreservesUnavailableSalaryAndRejectsPrivateUseText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		salary         string
		salaryEvidence string
		wantSalary     string
		wantError      bool
	}{
		{name: "unavailable salary remains empty", salaryEvidence: "unavailable"},
		{
			name: "private-use salary cannot be claimed as readable", salary: "\ue031\ue032K",
			salaryEvidence: "readable", wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			job, script, err := readDiscoveryFixtureJob(t, map[string]any{
				"platformJobId": "boss-job-1", "detailPlatformJobId": "boss-job-1",
				"platformStatusEvidence": "招聘中",
				"canonicalUrl":           "https://www.zhipin.com/job_detail/boss-job-1.html",
				"jobTitle":               "Go 后端工程师", "companyName": "示例科技", "city": "福州",
				"salary": test.salary, "salaryEvidence": test.salaryEvidence,
				"fullJD": "负责 Go 服务开发\n任职要求：熟悉 Go 与 SQLite",
			})
			if test.wantError {
				var fetchErr *discovery.FetchError
				if !errors.As(err, &fetchErr) || fetchErr.Category != discovery.FetchErrorInvalidResponse {
					t.Fatalf("read salary error = %v, want invalid_response", err)
				}
				return
			}
			if err != nil || job.Salary != test.wantSalary {
				t.Fatalf("read salary = %q, err=%v, want %q", job.Salary, err, test.wantSalary)
			}
			for _, want := range []string{"salaryEvidence", "hasPrivateUseCharacters"} {
				if !strings.Contains(script, want) {
					t.Errorf("ReadJob script does not contain %q", want)
				}
			}
		})
	}
}

func TestJobDiscoveryAcceptsProseOnlyJDWithoutResponsibilityRequirementBoundary(t *testing.T) {
	t.Parallel()

	// A legitimate prose JD with no 职责/要求 heading must be read whole, not
	// rejected: the adapter no longer imposes a responsibilities/requirements
	// split, so one such job can no longer fail the discovery run.
	job, _, err := readDiscoveryFixtureJob(t, map[string]any{
		"platformJobId": "boss-job-1", "detailPlatformJobId": "boss-job-1",
		"platformStatusEvidence": "招聘中",
		"canonicalUrl":           "https://www.zhipin.com/job_detail/boss-job-1.html",
		"jobTitle":               "AI 原生全栈工程师", "companyName": "示例科技",
		"city": "福州", "salary": "30-50K", "salaryEvidence": "readable",
		"fullJD": "我们在找能独立把想法做成产品的工程师，端到端负责 AI 应用，熟悉 Go 或 TypeScript 优先。",
	})
	if err != nil {
		t.Fatalf("read prose-only JD job: %v", err)
	}
	if job.FullJD != "我们在找能独立把想法做成产品的工程师，端到端负责 AI 应用，熟悉 Go 或 TypeScript 优先。" {
		t.Errorf("prose-only JD = %q, want the whole JD preserved", job.FullJD)
	}
}

func TestJobDiscoveryTreatsEmptyJDAsRetryableReadFailureWithoutInvalidatingTheList(t *testing.T) {
	t.Parallel()

	// An empty JD is a single-job read failure (partial or anti-bot page), not a
	// list-integrity problem: it must be retryable (transient), decoupled from
	// invalid_response, so it cannot fail the whole discovery run.
	listJSON := mustEncodeDiscoveryFixture(t, reliableListedJobFixture())
	jobJSON := mustEncodeDiscoveryFixture(t, map[string]any{
		"platformJobId": "boss-job-1", "detailPlatformJobId": "boss-job-1",
		"platformStatusEvidence": "招聘中",
		"canonicalUrl":           "https://www.zhipin.com/job_detail/boss-job-1.html",
		"jobTitle":               "JD 尚未加载的岗位", "companyName": "示例科技", "city": "福州",
		"salary": "20-30K", "salaryEvidence": "readable", "fullJD": "   \n  ",
	})
	fixture := &separatedJobDiscoveryFixture{t: t, evaluationResults: []string{listJSON, jobJSON}}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	adapter := NewJobDiscovery(server.URL, server.Client())

	page, err := adapter.ListPage(t.Context(), discovery.SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}, 1)
	if err != nil || !reflect.DeepEqual(page.PlatformJobIDs, []string{"boss-job-1"}) {
		t.Fatalf("list empty-JD job page = %#v, err=%v", page, err)
	}
	_, err = adapter.ReadJob(t.Context(), "boss-job-1")
	var fetchErr *discovery.FetchError
	if !errors.As(err, &fetchErr) {
		t.Fatalf("read error = %v, want discovery.FetchError", err)
	}
	if fetchErr.Category != discovery.FetchErrorTransient {
		t.Errorf("fetch error category = %q, want transient", fetchErr.Category)
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

func TestJobDiscoveryRejectsUnconfirmedLiveDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		fullJD              string
		detailPlatformJobID string
		platformStatus      string
	}{
		{
			name: "live detail does not confirm the listed stable ID", fullJD: "负责 Go 服务开发",
			detailPlatformJobID: "another-job", platformStatus: "招聘中",
		},
		{
			name: "live detail does not confirm the job is open", fullJD: "负责 Go 服务开发",
			detailPlatformJobID: "boss-job-1", platformStatus: "已关闭",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			listJSON := mustEncodeDiscoveryFixture(t, reliableListedJobFixture())
			jobJSON := mustEncodeDiscoveryFixture(t, map[string]any{
				"platformJobId": "boss-job-1", "detailPlatformJobId": test.detailPlatformJobID,
				"platformStatusEvidence": test.platformStatus,
				"canonicalUrl":           "https://www.zhipin.com/job_detail/boss-job-1.html",
				"jobTitle":               "Go 后端工程师", "companyName": "示例科技",
				"city": "福州", "salary": "20-30K", "salaryEvidence": "readable", "fullJD": test.fullJD,
			})
			fixture := &separatedJobDiscoveryFixture{
				t: t, evaluationResults: []string{listJSON, jobJSON},
			}
			server := httptest.NewServer(fixture)
			t.Cleanup(server.Close)
			adapter := NewJobDiscovery(server.URL, server.Client())

			_, err := adapter.ListPage(t.Context(), discovery.SearchRange{
				Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
			}, 1)
			if err != nil {
				t.Fatalf("list discovery page: %v", err)
			}
			_, err = adapter.ReadJob(t.Context(), "boss-job-1")
			var fetchErr *discovery.FetchError
			if !errors.As(err, &fetchErr) || fetchErr.Category != discovery.FetchErrorInvalidResponse {
				t.Fatalf("fetch error = %v, want invalid_response", err)
			}
		})
	}
}

func readDiscoveryFixtureJob(
	t *testing.T,
	rawJob map[string]any,
) (discovery.JobObservation, string, error) {
	t.Helper()
	fixture := &separatedJobDiscoveryFixture{
		t: t,
		evaluationResults: []string{
			mustEncodeDiscoveryFixture(t, reliableListedJobFixture()),
			mustEncodeDiscoveryFixture(t, rawJob),
		},
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	adapter := NewJobDiscovery(server.URL, server.Client())
	_, err := adapter.ListPage(t.Context(), discovery.SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}, 1)
	if err != nil {
		t.Fatalf("list discovery page: %v", err)
	}
	job, err := adapter.ReadJob(t.Context(), "boss-job-1")
	return job, fixture.evaluationScripts[1], err
}

func reliableListedJobFixture() map[string]any {
	return map[string]any{
		"jobs": []map[string]any{{
			"platformJobId": "boss-job-1", "securityId": "ephemeral-security", "lid": "ephemeral-list",
			"jobTitle": "Go 后端工程师", "companyName": "示例科技", "city": "福州", "salary": "20-30K",
		}},
		"hasMore": false,
	}
}

func mustEncodeDiscoveryFixture(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode discovery fixture: %v", err)
	}
	return string(encoded)
}

type separatedJobDiscoveryFixture struct {
	t                 *testing.T
	evaluationResults []string
	actions           []string
	evaluationScripts []string
}

func (f *separatedJobDiscoveryFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		script, _ := command.Args["code"].(string)
		f.evaluationScripts = append(f.evaluationScripts, script)
		resultIndex := len(f.evaluationScripts) - 1
		if resultIndex >= len(f.evaluationResults) {
			f.t.Errorf("unexpected evaluation %d", resultIndex+1)
			http.Error(w, "unexpected evaluation", http.StatusBadRequest)
			return
		}
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"type": "string", "value": f.evaluationResults[resultIndex]},
		})
	default:
		f.t.Errorf("unexpected WebBridge action %q", command.Action)
		http.Error(w, "unexpected action", http.StatusBadRequest)
	}
}
