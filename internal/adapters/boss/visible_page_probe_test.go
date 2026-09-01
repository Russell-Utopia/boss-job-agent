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

func TestVisiblePageProbeReadsStrictDOMObservationsWithoutPrivateRequests(t *testing.T) {
	t.Parallel()

	raw := rawVisiblePageProbeResult{
		Jobs: []rawDiscoveryJob{{
			PlatformJobID:          "boss-job-1",
			DetailPlatformJobID:    "boss-job-1",
			PlatformStatusEvidence: "招聘中",
			CanonicalURL:           "https://www.zhipin.com/job_detail/boss-job-1.html",
			JobTitle:               "Go 后端工程师",
			CompanyName:            "示例科技",
			City:                   "福州",
			Salary:                 "20-30K",
			SalaryEvidence:         string(visibleSalaryReadable),
			FullJD:                 "负责 Go 服务开发\n任职要求：熟悉 Go 与 SQLite",
		}},
		ScannedCardCount:   3,
		Truncated:          true,
		ExhaustionEvidence: visiblePageExhaustionUnavailable,
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode visible page probe fixture: %v", err)
	}
	fixture := &visiblePageProbeFixture{t: t, encodedResult: string(encoded)}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)

	got, err := newVisiblePageProbe(server.URL, server.Client()).read(
		t.Context(),
		"https://www.zhipin.com/web/geek/job?query=Go",
		visiblePageProbeMaxJobs,
	)
	if err != nil {
		t.Fatalf("read visible page probe: %v", err)
	}
	want := visiblePageProbeResult{
		Jobs: []visiblePageJob{{
			PlatformJobID:    "boss-job-1",
			CanonicalURL:     "https://www.zhipin.com/job_detail/boss-job-1.html",
			JobTitle:         "Go 后端工程师",
			CompanyName:      "示例科技",
			City:             "福州",
			Salary:           "20-30K",
			SalaryEvidence:   visibleSalaryReadable,
			Responsibilities: "负责 Go 服务开发",
			Requirements:     "熟悉 Go 与 SQLite",
			FullJD:           "负责 Go 服务开发\n任职要求：熟悉 Go 与 SQLite",
			JDStructure:      visibleJDExplicitSplit,
			PlatformStatus:   discovery.PlatformStatusOpen,
		}},
		ScannedCardCount:   3,
		Truncated:          true,
		ExhaustionEvidence: visiblePageExhaustionUnavailable,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("visible page probe result = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(fixture.actions, []string{"find_tab", "navigate", "evaluate"}) {
		t.Errorf("WebBridge actions = %#v", fixture.actions)
	}
	for _, forbidden := range []string{"/wapi/", "fetch(", "credentials:", "securityId"} {
		if strings.Contains(fixture.evaluationScript, forbidden) {
			t.Errorf("visible page script contains forbidden private-request material %q", forbidden)
		}
	}
	for _, expected := range []string{
		".job-card-box", "scrollIntoView", ".click()", "detail_identity_mismatch",
		"BOSS_PLATFORM_LIMITED", `"limit":8`, "waitForStableCards",
		".job-salary", ".boss-name", ".company-location", ".more-job-btn", ".op-btn-chat", ".innerText",
		"salaryEvidence", "unavailable", "cardIdentity",
	} {
		if !strings.Contains(fixture.evaluationScript, expected) {
			t.Errorf("visible page script does not contain %q", expected)
		}
	}
}

func TestVisiblePageProbeKeepsJobWhenSalaryIsUnavailable(t *testing.T) {
	t.Parallel()

	raw := rawVisiblePageProbeResult{
		Jobs: []rawDiscoveryJob{{
			PlatformJobID:          "boss-job-obfuscated",
			DetailPlatformJobID:    "boss-job-obfuscated",
			PlatformStatusEvidence: "招聘中",
			CanonicalURL:           "https://www.zhipin.com/job_detail/boss-job-obfuscated.html",
			JobTitle:               "Go 工程师",
			CompanyName:            "示例科技",
			City:                   "厦门",
			Salary:                 "",
			SalaryEvidence:         string(visibleSalaryUnavailable),
			FullJD:                 "完整职位描述",
		}},
		ScannedCardCount:   1,
		ExhaustionEvidence: visiblePageExhaustionUnavailable,
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode unavailable salary fixture: %v", err)
	}
	result, err := decodeVisiblePageProbeResult(string(encoded))
	if err != nil {
		t.Fatalf("decode unavailable salary fixture: %v", err)
	}
	if result.Jobs[0].Salary != "" || result.Jobs[0].SalaryEvidence != visibleSalaryUnavailable {
		t.Errorf("job salary = (%q, %q), want unavailable empty salary", result.Jobs[0].Salary, result.Jobs[0].SalaryEvidence)
	}
}

func TestVisiblePageProbeRejectsPrivateUseSalaryClaimedAsReadable(t *testing.T) {
	t.Parallel()

	raw := rawVisiblePageProbeResult{
		Jobs: []rawDiscoveryJob{{
			PlatformJobID:          "boss-job-obfuscated",
			DetailPlatformJobID:    "boss-job-obfuscated",
			PlatformStatusEvidence: "招聘中",
			CanonicalURL:           "https://www.zhipin.com/job_detail/boss-job-obfuscated.html",
			JobTitle:               "Go 工程师",
			CompanyName:            "示例科技",
			City:                   "厦门",
			Salary:                 "-K",
			SalaryEvidence:         string(visibleSalaryReadable),
			FullJD:                 "完整职位描述",
		}},
		ScannedCardCount:   1,
		ExhaustionEvidence: visiblePageExhaustionUnavailable,
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode private-use salary fixture: %v", err)
	}
	_, err = decodeVisiblePageProbeResult(string(encoded))
	if err == nil || !strings.Contains(err.Error(), "claims a readable salary") {
		t.Fatalf("decode private-use salary error = %v", err)
	}
}

func TestClassifyVisibleJDKeepsFullTextWhenStructuredPartsAreMissing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		fullJD               string
		wantResponsibilities string
		wantRequirements     string
		wantStructure        visibleJDStructure
	}{
		{
			name:                 "explicit split",
			fullJD:               "岗位职责：负责 Go 服务开发\n任职要求：熟悉 Go 与 SQLite",
			wantResponsibilities: "负责 Go 服务开发",
			wantRequirements:     "熟悉 Go 与 SQLite",
			wantStructure:        visibleJDExplicitSplit,
		},
		{
			name:                 "responsibilities only",
			fullJD:               "岗位职责:侧重招聘、员工关系、企业文化职位描述至少3年工作经验",
			wantResponsibilities: "侧重招聘、员工关系、企业文化职位描述至少3年工作经验",
			wantStructure:        visibleJDResponsibilitiesOnly,
		},
		{
			name:             "requirements only",
			fullJD:           "任职要求：至少 3 年工作经验",
			wantRequirements: "至少 3 年工作经验",
			wantStructure:    visibleJDRequirementsOnly,
		},
		{
			name:          "unstructured",
			fullJD:        "负责招聘和员工关系，至少 3 年工作经验",
			wantStructure: visibleJDUnstructured,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotResponsibilities, gotRequirements, gotStructure := classifyVisibleJD(tt.fullJD)
			if gotResponsibilities != tt.wantResponsibilities || gotRequirements != tt.wantRequirements || gotStructure != tt.wantStructure {
				t.Errorf(
					"classifyVisibleJD() = (%q, %q, %q), want (%q, %q, %q)",
					gotResponsibilities, gotRequirements, gotStructure,
					tt.wantResponsibilities, tt.wantRequirements, tt.wantStructure,
				)
			}
		})
	}
}

func TestVisiblePageProbeAcceptsRenderedFullJDWithoutInventingRequirements(t *testing.T) {
	t.Parallel()

	raw := rawVisiblePageProbeResult{
		Jobs: []rawDiscoveryJob{{
			PlatformJobID:          "boss-job-hrbp",
			DetailPlatformJobID:    "boss-job-hrbp",
			PlatformStatusEvidence: "招聘中",
			CanonicalURL:           "https://www.zhipin.com/job_detail/boss-job-hrbp.html",
			JobTitle:               "HRBP",
			CompanyName:            "示例组织",
			City:                   "厦门",
			Salary:                 "15-20K",
			SalaryEvidence:         string(visibleSalaryReadable),
			FullJD:                 "岗位职责:侧重招聘、员工关系、企业文化职位描述至少3年工作经验",
		}},
		ScannedCardCount:   1,
		ExhaustionEvidence: visiblePageExhaustionUnavailable,
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode visible full JD fixture: %v", err)
	}
	result, err := decodeVisiblePageProbeResult(string(encoded))
	if err != nil {
		t.Fatalf("decode visible full JD fixture: %v", err)
	}
	job := result.Jobs[0]
	if job.FullJD != raw.Jobs[0].FullJD || job.Responsibilities == "" || job.Requirements != "" || job.JDStructure != visibleJDResponsibilitiesOnly {
		t.Errorf("visible full JD job = %#v", job)
	}
}

func TestVisiblePageProbeRejectsUnsafeTargetAndUnreliableDetail(t *testing.T) {
	t.Parallel()

	probe := newVisiblePageProbe("http://127.0.0.1:1", http.DefaultClient)
	for _, target := range []string{
		"http://www.zhipin.com/web/geek/job",
		"https://example.com/web/geek/job",
		"https://www.zhipin.com/job_detail/boss-job-1.html",
	} {
		if _, err := probe.read(t.Context(), target, 1); err == nil {
			t.Errorf("unsafe visible page target %q succeeded", target)
		}
	}

	encoded, err := json.Marshal(rawVisiblePageProbeResult{
		Jobs: []rawDiscoveryJob{{
			PlatformJobID: "boss-job-1", DetailPlatformJobID: "boss-job-2",
			PlatformStatusEvidence: "招聘中", CanonicalURL: "https://www.zhipin.com/job_detail/boss-job-1.html",
			JobTitle: "Go 后端工程师", CompanyName: "示例科技", City: "福州", Salary: "20-30K",
			SalaryEvidence: string(visibleSalaryReadable),
			FullJD:         "负责 Go 服务开发\n任职要求：熟悉 Go",
		}},
		ScannedCardCount: 1, ExhaustionEvidence: visiblePageExhaustionUnavailable,
	})
	if err != nil {
		t.Fatalf("encode unreliable visible page result: %v", err)
	}
	_, err = decodeVisiblePageProbeResult(string(encoded))
	if err == nil || !strings.Contains(err.Error(), "does not confirm") {
		t.Fatalf("decode unreliable visible page error = %v", err)
	}
}

func TestVisiblePageProbeDOMFailuresClassifyAsInvalidResponse(t *testing.T) {
	t.Parallel()

	err := classifyDiscoveryError(errors.New(
		"BOSS_VISIBLE_PAGE_UNRELIABLE:detail_identity_mismatch|stage=job_detail|detail_ordinal=2",
	))
	var fetchErr *discovery.FetchError
	if !errors.As(err, &fetchErr) || fetchErr.Category != discovery.FetchErrorInvalidResponse {
		t.Fatalf("visible DOM failure = %v, want invalid_response", err)
	}
}

type visiblePageProbeFixture struct {
	t                *testing.T
	encodedResult    string
	actions          []string
	evaluationScript string
}

func (f *visiblePageProbeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var command struct {
		Action  string         `json:"action"`
		Args    map[string]any `json:"args"`
		Session string         `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
		f.t.Errorf("decode visible page WebBridge command: %v", err)
		http.Error(w, "invalid command", http.StatusBadRequest)
		return
	}
	f.actions = append(f.actions, command.Action)
	if command.Session != visiblePageProbeSession {
		f.t.Errorf("session = %q, want %q", command.Session, visiblePageProbeSession)
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
		if command.Args["url"] != "https://www.zhipin.com/web/geek/job?query=Go" {
			f.t.Errorf("navigate URL = %#v", command.Args["url"])
		}
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"success": true, "url": command.Args["url"], "tabId": 51},
		})
	case "evaluate":
		f.evaluationScript, _ = command.Args["code"].(string)
		writeFixtureJSON(f.t, w, map[string]any{
			"ok": true, "data": map[string]any{"type": "string", "value": f.encodedResult},
		})
	default:
		f.t.Errorf("unexpected WebBridge action %q", command.Action)
		http.Error(w, "unexpected action", http.StatusBadRequest)
	}
}
