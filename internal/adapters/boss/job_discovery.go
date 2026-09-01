package boss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
)

const (
	jobSearchURL        = "https://www.zhipin.com/web/geek/job"
	jobDiscoverySession = "boss-job-agent-discovery"
	jobDiscoveryGroup   = "BOSS Job Agent 岗位发现"
)

// JobDiscovery lists BOSS pages and reads one listed job at a time through the
// local Kimi WebBridge daemon and one discovery-owned authenticated session.
type JobDiscovery struct {
	bridge *webBridge
	mu     sync.RWMutex
	listed map[string]rawListedJob
}

func NewDefaultJobDiscovery() *JobDiscovery {
	return NewJobDiscovery(defaultWebBridgeEndpoint, http.DefaultClient)
}

func NewJobDiscovery(endpoint string, client *http.Client) *JobDiscovery {
	return &JobDiscovery{
		bridge: newWebBridge(endpoint, client, jobDiscoverySession),
		listed: make(map[string]rawListedJob),
	}
}

func (a *JobDiscovery) ListPage(
	ctx context.Context,
	searchRange discovery.SearchRange,
	pageNo int,
) (discovery.JobPage, error) {
	if err := validateSearchInput(searchRange, pageNo); err != nil {
		return discovery.JobPage{}, classifyDiscoveryError(err)
	}
	if err := a.prepareSearchTab(ctx); err != nil {
		return discovery.JobPage{}, classifyDiscoveryError(err)
	}
	script, err := buildListJobDiscoveryScript(searchRange, pageNo)
	if err != nil {
		return discovery.JobPage{}, classifyDiscoveryError(err)
	}
	value, err := a.evaluateString(ctx, script, "list BOSS discovery page")
	if err != nil {
		return discovery.JobPage{}, classifyDiscoveryError(err)
	}
	page, listed, err := decodeReliableJobList(value)
	if err != nil {
		return discovery.JobPage{}, classifyDiscoveryError(newAdapterFailure(
			adapterFailureInvalidResponse,
			fmt.Errorf("decode reliable BOSS discovery list: %w", err),
		))
	}
	a.mu.Lock()
	a.listed = listed
	a.mu.Unlock()
	return page, nil
}

func (a *JobDiscovery) ReadJob(ctx context.Context, platformJobID string) (discovery.JobObservation, error) {
	platformJobID = strings.TrimSpace(platformJobID)
	a.mu.RLock()
	listed, ok := a.listed[platformJobID]
	a.mu.RUnlock()
	if platformJobID == "" || !ok {
		return discovery.JobObservation{}, classifyDiscoveryError(newAdapterFailure(
			adapterFailureInvalidResponse,
			errors.New("requested BOSS discovery job is not in the current listed page"),
		))
	}
	script, err := buildReadDiscoveryJobScript(listed)
	if err != nil {
		return discovery.JobObservation{}, classifyDiscoveryError(err)
	}
	value, err := a.evaluateString(ctx, script, "read BOSS discovery job")
	if err != nil {
		return discovery.JobObservation{}, classifyDiscoveryError(err)
	}
	var rawJob rawDiscoveryJob
	if err := json.Unmarshal([]byte(value), &rawJob); err != nil {
		return discovery.JobObservation{}, classifyDiscoveryError(newAdapterFailure(
			adapterFailureInvalidResponse,
			fmt.Errorf("decode reliable BOSS discovery job: %w", err),
		))
	}
	observation, err := observationFromReliableSearchDetail(rawJob)
	if err != nil {
		return discovery.JobObservation{}, classifyDiscoveryError(newAdapterFailure(
			adapterFailureInvalidResponse,
			err,
		))
	}
	return observation, nil
}

func (a *JobDiscovery) evaluateString(ctx context.Context, script, operation string) (string, error) {
	var evaluation evaluationResult
	if err := a.bridge.command(ctx, "evaluate", map[string]any{"code": script}, &evaluation); err != nil {
		return "", fmt.Errorf("%s: %w", operation, err)
	}
	if evaluation.Type != "string" || evaluation.Value == "" {
		return "", newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("%s extraction returned a non-string result", operation),
		)
	}
	return evaluation.Value, nil
}

func (a *JobDiscovery) prepareSearchTab(ctx context.Context) error {
	newTab, err := a.bridge.tabNeedsOpening(ctx, jobSearchURL)
	if err != nil {
		return err
	}
	navigateArgs := map[string]any{"url": jobSearchURL, "newTab": newTab}
	if newTab {
		navigateArgs["group_title"] = jobDiscoveryGroup
	}
	var navigation navigationResult
	if err := a.bridge.command(ctx, "navigate", navigateArgs, &navigation); err != nil {
		return fmt.Errorf("navigate to BOSS job search: %w", err)
	}
	if !navigation.Success {
		return newAdapterFailure(
			adapterFailureInvalidResponse,
			errors.New("navigate to BOSS job search returned unsuccessful result"),
		)
	}
	return nil
}

type rawJobList struct {
	Jobs    []rawListedJob `json:"jobs"`
	HasMore *bool          `json:"hasMore"`
}

type rawListedJob struct {
	PlatformJobID string `json:"platformJobId"`
	SecurityID    string `json:"securityId"`
	LID           string `json:"lid"`
	JobTitle      string `json:"jobTitle"`
	CompanyName   string `json:"companyName"`
	City          string `json:"city"`
	Salary        string `json:"salary"`
}

type rawDiscoveryJob struct {
	PlatformJobID          string `json:"platformJobId"`
	DetailPlatformJobID    string `json:"detailPlatformJobId"`
	PlatformStatusEvidence string `json:"platformStatusEvidence"`
	CanonicalURL           string `json:"canonicalUrl"`
	JobTitle               string `json:"jobTitle"`
	CompanyName            string `json:"companyName"`
	City                   string `json:"city"`
	Salary                 string `json:"salary"`
	SalaryEvidence         string `json:"salaryEvidence,omitempty"`
	FullJD                 string `json:"fullJD"`
}

var requirementsHeading = regexp.MustCompile(
	`(?m)^[\t ]*(?:任职要求|岗位要求|职位要求|任职资格)[\t ]*(?:[：:][\t ]*|\n)`,
)

func decodeReliableJobList(value string) (discovery.JobPage, map[string]rawListedJob, error) {
	var rawPage rawJobList
	if err := json.Unmarshal([]byte(value), &rawPage); err != nil {
		return discovery.JobPage{}, nil, err
	}
	if rawPage.Jobs == nil || rawPage.HasMore == nil {
		return discovery.JobPage{}, nil, errors.New("BOSS discovery list requires jobs and explicit hasMore")
	}
	page := discovery.JobPage{
		PlatformJobIDs: make([]string, 0, len(rawPage.Jobs)),
		HasMore:        *rawPage.HasMore,
	}
	listed := make(map[string]rawListedJob, len(rawPage.Jobs))
	for index, job := range rawPage.Jobs {
		job.PlatformJobID = strings.TrimSpace(job.PlatformJobID)
		if job.PlatformJobID == "" {
			return discovery.JobPage{}, nil, fmt.Errorf("BOSS discovery list job %d has no stable ID", index+1)
		}
		if _, duplicate := listed[job.PlatformJobID]; duplicate {
			return discovery.JobPage{}, nil, errors.New("BOSS discovery list repeats a stable ID")
		}
		page.PlatformJobIDs = append(page.PlatformJobIDs, job.PlatformJobID)
		listed[job.PlatformJobID] = job
	}
	return page, listed, nil
}

func observationFromReliableSearchDetail(rawJob rawDiscoveryJob) (discovery.JobObservation, error) {
	platformJobID := strings.TrimSpace(rawJob.PlatformJobID)
	if platformJobID == "" || strings.TrimSpace(rawJob.DetailPlatformJobID) != platformJobID {
		return discovery.JobObservation{}, errors.New("BOSS live detail does not confirm the listed platform job")
	}
	if strings.TrimSpace(rawJob.PlatformStatusEvidence) != "招聘中" {
		return discovery.JobObservation{}, errors.New("BOSS live detail has no reliable open status")
	}
	salary, err := reliableDiscoverySalary(rawJob.Salary, rawJob.SalaryEvidence)
	if err != nil {
		return discovery.JobObservation{}, err
	}
	responsibilities, requirements, err := splitReliableJD(rawJob.FullJD)
	if err != nil {
		return discovery.JobObservation{}, fmt.Errorf("BOSS live detail: %w", err)
	}
	// A matching live detail identity plus BOSS's explicit 招聘中 status is
	// this adapter's reliable evidence that the job is open.
	return discovery.JobObservation{
		PlatformJobID:    platformJobID,
		CanonicalURL:     strings.TrimSpace(rawJob.CanonicalURL),
		JobTitle:         strings.TrimSpace(rawJob.JobTitle),
		CompanyName:      strings.TrimSpace(rawJob.CompanyName),
		City:             strings.TrimSpace(rawJob.City),
		Salary:           salary,
		Responsibilities: responsibilities,
		Requirements:     requirements,
		PlatformStatus:   discovery.PlatformStatusOpen,
	}, nil
}

func reliableDiscoverySalary(value, evidence string) (string, error) {
	salary := strings.TrimSpace(value)
	switch strings.TrimSpace(evidence) {
	case "readable":
		if salary == "" || containsPrivateUseCharacters(salary) {
			return "", errors.New("BOSS live detail salary claimed readable without reliable text")
		}
		return salary, nil
	case "unavailable":
		if salary != "" {
			return "", errors.New("BOSS live detail must clear unavailable salary text")
		}
		return "", nil
	default:
		return "", errors.New("BOSS live detail has invalid salary evidence")
	}
}

func splitReliableJD(value string) (string, string, error) {
	fullJD := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
	boundary := requirementsHeading.FindStringIndex(fullJD)
	if boundary == nil || boundary[0] == 0 {
		return "", "", errors.New("complete JD has no reliable responsibilities and requirements boundary")
	}
	responsibilities := strings.TrimSpace(fullJD[:boundary[0]])
	requirements := strings.TrimSpace(fullJD[boundary[1]:])
	if responsibilities == "" || requirements == "" {
		return "", "", errors.New("complete JD responsibilities or requirements are empty")
	}
	return responsibilities, requirements, nil
}

func validateSearchInput(searchRange discovery.SearchRange, pageNo int) error {
	if pageNo <= 0 {
		return newAdapterFailure(adapterFailureInvalidResponse, errors.New("BOSS discovery page must be positive"))
	}
	inputs := []string{
		searchRange.Role, searchRange.City, searchRange.Salary, searchRange.EmploymentType,
	}
	for _, input := range inputs {
		if strings.TrimSpace(input) == "" {
			return newAdapterFailure(
				adapterFailureInvalidResponse,
				errors.New("BOSS discovery requires role, city, salary, and employment type"),
			)
		}
	}
	return nil
}

type discoveryScriptInput struct {
	Role           string `json:"role"`
	City           string `json:"city"`
	Salary         string `json:"salary"`
	EmploymentType string `json:"employmentType"`
	Page           int    `json:"page"`
}

func buildListJobDiscoveryScript(searchRange discovery.SearchRange, pageNo int) (string, error) {
	encoded, err := json.Marshal(discoveryScriptInput{
		Role:           searchRange.Role,
		City:           searchRange.City,
		Salary:         searchRange.Salary,
		EmploymentType: searchRange.EmploymentType,
		Page:           pageNo,
	})
	if err != nil {
		return "", newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("encode BOSS discovery list input: %w", err),
		)
	}
	return renderJobDiscoveryScript(listJobDiscoveryPageScript, "__SEARCH_INPUT__", string(encoded)), nil
}

func buildReadDiscoveryJobScript(listed rawListedJob) (string, error) {
	encoded, err := json.Marshal(listed)
	if err != nil {
		return "", newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("encode BOSS discovery job input: %w", err),
		)
	}
	return renderJobDiscoveryScript(readDiscoveryJobScript, "__JOB_INPUT__", string(encoded)), nil
}

func renderJobDiscoveryScript(template, inputMarker, encodedInput string) string {
	script := strings.Replace(template, inputMarker, encodedInput, 1)
	return strings.Replace(script, "__DISCOVERY_COMMON__", jobDiscoveryCommonScript, 1)
}

func classifyDiscoveryError(cause error) error {
	category := discoveryCategoryFromCause(cause)
	return &discovery.FetchError{
		Category: category,
		Evidence: discoveryFailureEvidence(cause),
		Cause:    cause,
	}
}

var discoveryFailureEvidencePattern = regexp.MustCompile(
	`\|request_ordinal=(\d+)\|stage=([a-z_]+)\|detail_ordinal=(\d+)(?:\|upstream_code=([A-Za-z0-9_-]+))?`,
)

func discoveryFailureEvidence(cause error) *discovery.FetchFailureEvidence {
	match := discoveryFailureEvidencePattern.FindStringSubmatch(cause.Error())
	if match == nil {
		return nil
	}
	requestOrdinal, requestErr := strconv.Atoi(match[1])
	detailOrdinal, detailErr := strconv.Atoi(match[3])
	if requestErr != nil || detailErr != nil {
		return nil
	}
	return &discovery.FetchFailureEvidence{
		RequestOrdinal: requestOrdinal,
		Stage:          match[2],
		DetailOrdinal:  detailOrdinal,
		UpstreamCode:   match[4],
	}
}

func discoveryCategoryFromCause(cause error) discovery.FetchErrorCategory {
	message := strings.ToLower(cause.Error())
	switch {
	case strings.Contains(message, "boss_authentication_required"):
		return discovery.FetchErrorAuthenticationExpired
	case strings.Contains(message, "boss_verification_required"):
		return discovery.FetchErrorVerificationRequired
	case strings.Contains(message, "boss_platform_limited"):
		return discovery.FetchErrorPlatformLimited
	case strings.Contains(message, "boss_search_filter_unresolved"),
		strings.Contains(message, "boss_discovery_unreliable_page"),
		strings.Contains(message, "boss_visible_page_unreliable"):
		return discovery.FetchErrorInvalidResponse
	}
	var failure *adapterFailure
	if !errors.As(cause, &failure) {
		return discovery.FetchErrorTransient
	}
	switch failure.kind {
	case adapterFailureInvalidResponse:
		return discovery.FetchErrorInvalidResponse
	case adapterFailureInvalidProtocol:
		return discovery.FetchErrorInvalidProtocol
	default:
		return discovery.FetchErrorTransient
	}
}

const jobDiscoveryCommonScript = `
  let requestOrdinal = 0;
  const fail = (kind, stage, upstreamCode = "") => {
    const evidence = [
      "request_ordinal=" + requestOrdinal,
      "stage=" + stage,
      "detail_ordinal=0"
    ];
    const code = String(upstreamCode ?? "");
    if (/^-?\d{1,10}$/.test(code)) evidence.push("upstream_code=" + code);
    throw new Error(kind + "|" + evidence.join("|"));
  };
  const pageText = document.body?.innerText || "";
  if (/login/i.test(location.pathname) || pageText.includes("登录/注册")) {
    fail("BOSS_AUTHENTICATION_REQUIRED", preflightStage);
  }
  if (pageText.includes("安全验证") || pageText.includes("请输入验证码")) {
    fail("BOSS_VERIFICATION_REQUIRED", preflightStage);
  }
  if (pageText.includes("访问过于频繁") || pageText.includes("操作过于频繁")) {
    fail("BOSS_PLATFORM_LIMITED", preflightStage);
  }
  const reliableData = (payload, stage) => {
    const upstreamCode = payload && typeof payload === "object" ? payload.code : "";
    if ([7, 1011, 120, 121, 122].includes(upstreamCode)) {
      fail("BOSS_AUTHENTICATION_REQUIRED", stage, upstreamCode);
    }
    if (upstreamCode === 5012) fail("BOSS_VERIFICATION_REQUIRED", stage, upstreamCode);
    if ([31, 32, 35, 36, 37, 5002, 5003, 5004].includes(upstreamCode)) {
      fail("BOSS_PLATFORM_LIMITED", stage, upstreamCode);
    }
    if (upstreamCode !== 0 || !payload.zpData) {
      fail("BOSS_DISCOVERY_UNRELIABLE_PAGE", stage, upstreamCode);
    }
    return payload.zpData;
  };
  const normalized = value => String(value || "").trim();
`

const listJobDiscoveryPageScript = `(async () => {
  const input = __SEARCH_INPUT__;
  const preflightStage = "page_preflight";
  __DISCOVERY_COMMON__
  const request = async (stage, path, params = {}) => {
    requestOrdinal++;
    const url = new URL(path, location.origin);
    Object.entries(params).forEach(([key, value]) => url.searchParams.set(key, value));
    let response;
    try {
      response = await fetch(url, {credentials: "include", headers: {"X-Requested-With": "XMLHttpRequest"}});
    } catch (_) {
      fail("BOSS_DISCOVERY_NETWORK_ERROR", stage);
    }
    if (!response.ok) fail("BOSS_DISCOVERY_HTTP_" + response.status, stage);
    let payload;
    try {
      payload = await response.json();
    } catch (_) {
      fail("BOSS_DISCOVERY_INVALID_JSON", stage);
    }
    return reliableData(payload, stage);
  };
  const optionCode = value => value?.code ?? value?.cityCode ?? value?.value ?? value?.id;
  const findNamedOption = (value, wanted) => {
    if (!value || typeof value !== "object") return null;
    const name = value.name ?? value.cityName ?? value.label ?? value.text;
    if (normalized(name) === normalized(wanted) && optionCode(value) != null) return value;
    for (const child of Object.values(value)) {
      if (Array.isArray(child)) {
        for (const entry of child) {
          const match = findNamedOption(entry, wanted);
          if (match) return match;
        }
      } else if (child && typeof child === "object") {
        const match = findNamedOption(child, wanted);
        if (match) return match;
      }
    }
    return null;
  };
  const salaryRange = value => {
    const label = normalized(value).toUpperCase();
    const numbers = [...label.matchAll(/\d+(?:\.\d+)?/g)].map(match => Number(match[0]));
    if (!numbers.length || numbers.some(number => !Number.isFinite(number))) return null;
    if (label.includes("以下")) return {min: 0, max: numbers[0]};
    if (label.includes("以上")) return {min: numbers[0], max: Number.POSITIVE_INFINITY};
    if (numbers.length < 2 || numbers[0] >= numbers[1]) return null;
    return {min: numbers[0], max: numbers[1]};
  };
  const resolveSalaryOption = (options, wanted) => {
    const exact = findNamedOption(options, wanted);
    if (exact) return exact;
    const wantedRange = salaryRange(wanted);
    if (!wantedRange || !Array.isArray(options)) return null;
    let best = null;
    let bestOverlap = 0;
    for (const option of options) {
      if (Number(optionCode(option)) === 0) continue;
      const optionRange = salaryRange(option.name ?? option.label ?? option.text);
      if (!optionRange) continue;
      const overlap = Math.min(wantedRange.max, optionRange.max) - Math.max(wantedRange.min, optionRange.min);
      if (overlap > bestOverlap) {
        best = option;
        bestOverlap = overlap;
      }
    }
    return best;
  };
  const cityData = await request("city_metadata", "/wapi/zpCommon/data/city.json");
  const cityOption = findNamedOption(cityData, input.city);
  if (!cityOption) fail("BOSS_SEARCH_FILTER_UNRESOLVED:city", "filter_resolution");
  const cityCode = optionCode(cityOption);
  const conditions = await request("filter_conditions", "/wapi/zpgeek/search/job/condition.json", {
    city: cityCode, query: input.role
  });
  const salaryOption = resolveSalaryOption(conditions.salaryList, input.salary);
  const employmentOption = findNamedOption(conditions, input.employmentType);
  if (!salaryOption || !employmentOption) {
    fail("BOSS_SEARCH_FILTER_UNRESOLVED:salary_or_employment_type", "filter_resolution");
  }
  const list = await request("job_list", "/wapi/zpgeek/search/joblist.json", {
    scene: 1,
    query: input.role,
    city: cityCode,
    salary: optionCode(salaryOption),
    jobType: optionCode(employmentOption),
    page: input.page,
    pageSize: 15
  });
  if (!Array.isArray(list.jobList) || typeof list.hasMore !== "boolean") {
    fail("BOSS_DISCOVERY_UNRELIABLE_PAGE", "job_list_validation");
  }
  const jobs = list.jobList.map(entry => {
    const card = entry.jobCard || entry;
    const platformJobId = normalized(card.encryptJobId || card.jobId);
    if (!platformJobId) {
      fail("BOSS_DISCOVERY_UNRELIABLE_PAGE:missing_stable_id", "job_card_validation");
    }
    return {
      platformJobId,
      securityId: normalized(card.securityId),
      lid: normalized(card.lid || list.lid),
      jobTitle: normalized(card.jobName),
      companyName: normalized(card.brandName),
      city: normalized(card.cityName),
      salary: normalized(card.salaryDesc)
    };
  });
  return JSON.stringify({jobs, hasMore: list.hasMore});
})()`

const readDiscoveryJobScript = `(async () => {
  const input = __JOB_INPUT__;
  const preflightStage = "job_preflight";
  __DISCOVERY_COMMON__
  const url = new URL("/wapi/zpgeek/job/detail.json", location.origin);
  Object.entries({securityId: input.securityId || "", jobId: input.platformJobId, lid: input.lid || ""})
    .forEach(([key, value]) => url.searchParams.set(key, value));
  requestOrdinal++;
  let response;
  try {
    response = await fetch(url, {credentials: "include", headers: {"X-Requested-With": "XMLHttpRequest"}});
  } catch (_) {
    fail("BOSS_DISCOVERY_NETWORK_ERROR", "job_detail");
  }
  if (!response.ok) fail("BOSS_DISCOVERY_HTTP_" + response.status, "job_detail");
  let payload;
  try {
    payload = await response.json();
  } catch (_) {
    fail("BOSS_DISCOVERY_INVALID_JSON", "job_detail");
  }
  const hasPrivateUseCharacters = value => /[\uE000-\uF8FF\u{F0000}-\u{FFFFD}\u{100000}-\u{10FFFD}]/u.test(value);
  const detail = reliableData(payload, "job_detail");
  const info = detail.jobInfo || detail.jobCard || detail;
  const detailPlatformJobId = normalized(info.encryptId || info.encryptJobId || info.jobId);
  if (detailPlatformJobId !== input.platformJobId) {
    fail("BOSS_DISCOVERY_UNRELIABLE_PAGE:detail_identity_mismatch", "job_detail_validation");
  }
  const renderedSalary = normalized(info.salaryDesc || input.salary);
  const salaryReadable = renderedSalary !== "" && !hasPrivateUseCharacters(renderedSalary);
  const job = {
    platformJobId: input.platformJobId,
    detailPlatformJobId,
    platformStatusEvidence: normalized(info.jobStatusDesc),
    canonicalUrl: "https://www.zhipin.com/job_detail/" + input.platformJobId + ".html",
    jobTitle: normalized(info.jobName || input.jobTitle),
    companyName: normalized(info.brandName || input.companyName),
    city: normalized(info.cityName || input.city),
    salary: salaryReadable ? renderedSalary : "",
    salaryEvidence: salaryReadable ? "readable" : "unavailable",
    fullJD: normalized(info.postDescription || info.jobDescription).replace(/\r\n?/g, "\n")
  };
  const required = [
    job.platformJobId, job.detailPlatformJobId, job.platformStatusEvidence,
    job.canonicalUrl, job.jobTitle, job.companyName, job.city, job.fullJD
  ];
  if (required.some(value => !normalized(value))) {
    fail("BOSS_DISCOVERY_UNRELIABLE_PAGE:missing_job_field", "job_detail_validation");
  }
  return JSON.stringify(job);
})()`
