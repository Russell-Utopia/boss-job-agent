package boss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
)

const (
	jobSearchURL        = "https://www.zhipin.com/web/geek/job"
	jobDiscoverySession = "boss-job-agent-discovery"
	jobDiscoveryGroup   = "BOSS Job Agent 岗位发现"
)

// JobDiscovery reads complete BOSS search pages through the local Kimi
// WebBridge daemon and one discovery-owned authenticated Chrome session.
type JobDiscovery struct {
	bridge *webBridge
}

func NewDefaultJobDiscovery() *JobDiscovery {
	return NewJobDiscovery(defaultWebBridgeEndpoint, http.DefaultClient)
}

func NewJobDiscovery(endpoint string, client *http.Client) *JobDiscovery {
	return &JobDiscovery{bridge: newWebBridge(endpoint, client, jobDiscoverySession)}
}

func (a *JobDiscovery) FetchPage(
	ctx context.Context,
	searchRange discovery.SearchRange,
	pageNo int,
) (discovery.DiscoveryPage, error) {
	if err := validateSearchInput(searchRange, pageNo); err != nil {
		return discovery.DiscoveryPage{}, classifyDiscoveryError(err)
	}
	if err := a.prepareSearchTab(ctx); err != nil {
		return discovery.DiscoveryPage{}, classifyDiscoveryError(err)
	}
	page, err := a.evaluatePage(ctx, searchRange, pageNo)
	if err != nil {
		return discovery.DiscoveryPage{}, classifyDiscoveryError(err)
	}
	return page, nil
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

func (a *JobDiscovery) evaluatePage(
	ctx context.Context,
	searchRange discovery.SearchRange,
	pageNo int,
) (discovery.DiscoveryPage, error) {
	script, err := buildJobDiscoveryScript(searchRange, pageNo)
	if err != nil {
		return discovery.DiscoveryPage{}, err
	}
	var evaluation evaluationResult
	if err := a.bridge.command(ctx, "evaluate", map[string]any{"code": script}, &evaluation); err != nil {
		return discovery.DiscoveryPage{}, fmt.Errorf("fetch BOSS discovery page: %w", err)
	}
	if evaluation.Type != "string" || evaluation.Value == "" {
		return discovery.DiscoveryPage{}, newAdapterFailure(
			adapterFailureInvalidProtocol,
			errors.New("BOSS discovery extraction returned a non-string result"),
		)
	}
	var page discovery.DiscoveryPage
	if err := json.Unmarshal([]byte(evaluation.Value), &page); err != nil {
		return discovery.DiscoveryPage{}, newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("decode BOSS discovery page: %w", err),
		)
	}
	if err := discovery.ValidatePage(page); err != nil {
		return discovery.DiscoveryPage{}, newAdapterFailure(adapterFailureInvalidResponse, err)
	}
	return page, nil
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

func buildJobDiscoveryScript(searchRange discovery.SearchRange, pageNo int) (string, error) {
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
			fmt.Errorf("encode BOSS discovery search input: %w", err),
		)
	}
	return strings.Replace(fetchJobDiscoveryPageScript, "__SEARCH_INPUT__", string(encoded), 1), nil
}

func classifyDiscoveryError(cause error) error {
	category := discoveryCategoryFromCause(cause)
	return &discovery.FetchError{Category: category, Cause: cause}
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
		strings.Contains(message, "boss_discovery_unreliable_page"):
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

const fetchJobDiscoveryPageScript = `(async () => {
  const input = __SEARCH_INPUT__;
  const pageText = document.body?.innerText || "";
  if (/login/i.test(location.pathname) || pageText.includes("登录/注册")) {
    throw new Error("BOSS_AUTHENTICATION_REQUIRED");
  }
  if (pageText.includes("安全验证") || pageText.includes("请输入验证码")) {
    throw new Error("BOSS_VERIFICATION_REQUIRED");
  }
  if (pageText.includes("访问过于频繁") || pageText.includes("操作过于频繁")) {
    throw new Error("BOSS_PLATFORM_LIMITED");
  }

  const request = async (path, params = {}) => {
    const url = new URL(path, location.origin);
    Object.entries(params).forEach(([key, value]) => url.searchParams.set(key, value));
    const response = await fetch(url, {credentials: "include", headers: {"X-Requested-With": "XMLHttpRequest"}});
    if (!response.ok) throw new Error("BOSS_DISCOVERY_HTTP_" + response.status);
    const payload = await response.json();
    if ([7, 1011, 120, 121, 122].includes(payload.code)) throw new Error("BOSS_AUTHENTICATION_REQUIRED");
    if (payload.code === 5012) throw new Error("BOSS_VERIFICATION_REQUIRED");
    if ([31, 32, 35, 36, 37, 5002, 5003, 5004].includes(payload.code)) throw new Error("BOSS_PLATFORM_LIMITED");
    if (payload.code !== 0 || !payload.zpData) throw new Error("BOSS_DISCOVERY_UNRELIABLE_PAGE");
    return payload.zpData;
  };

  const normalized = value => String(value || "").trim();
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

  const cityData = await request("/wapi/zpCommon/data/city.json");
  const cityOption = findNamedOption(cityData, input.city);
  if (!cityOption) throw new Error("BOSS_SEARCH_FILTER_UNRESOLVED:city");
  const cityCode = optionCode(cityOption);
  const conditions = await request("/wapi/zpgeek/search/job/condition.json", {city: cityCode, query: input.role});
  const salaryOption = findNamedOption(conditions, input.salary);
  const employmentOption = findNamedOption(conditions, input.employmentType);
  if (!salaryOption || !employmentOption) throw new Error("BOSS_SEARCH_FILTER_UNRESOLVED:salary_or_employment_type");

  const list = await request("/wapi/zpgeek/search/joblist.json", {
    scene: 1,
    query: input.role,
    city: cityCode,
    salary: optionCode(salaryOption),
    jobType: optionCode(employmentOption),
    page: input.page,
    pageSize: 30
  });
  if (!Array.isArray(list.jobList) || typeof list.hasMore !== "boolean") {
    throw new Error("BOSS_DISCOVERY_UNRELIABLE_PAGE");
  }

  const splitJD = value => {
    const full = normalized(value).replace(/\r\n?/g, "\n");
    if (!full) throw new Error("BOSS_DISCOVERY_UNRELIABLE_PAGE:missing_jd");
    const marker = full.search(/(?:任职要求|岗位要求|职位要求|任职资格)\s*[：:]/);
    if (marker > 0) {
      return {responsibilities: full.slice(0, marker).trim(), requirements: full.slice(marker).trim()};
    }
    return {responsibilities: full, requirements: full};
  };

  const observations = [];
  for (const entry of list.jobList) {
    const card = entry.jobCard || entry;
    const platformJobId = normalized(card.encryptJobId || card.jobId);
    if (!platformJobId) throw new Error("BOSS_DISCOVERY_UNRELIABLE_PAGE:missing_stable_id");
    const detail = await request("/wapi/zpgeek/job/detail.json", {
      securityId: card.securityId || "",
      jobId: platformJobId,
      lid: card.lid || list.lid || ""
    });
    const info = detail.jobInfo || detail.jobCard || detail;
    const jd = splitJD(info.postDescription || info.jobDescription);
    const observation = {
      platformJobId,
      canonicalUrl: "https://www.zhipin.com/job_detail/" + platformJobId + ".html",
      jobTitle: normalized(info.jobName || card.jobName),
      companyName: normalized(info.brandName || card.brandName),
      city: normalized(info.cityName || card.cityName),
      salary: normalized(info.salaryDesc || card.salaryDesc),
      responsibilities: jd.responsibilities,
      requirements: jd.requirements,
      platformStatus: "open"
    };
    if (Object.values(observation).some(value => !normalized(value))) {
      throw new Error("BOSS_DISCOVERY_UNRELIABLE_PAGE:missing_job_field");
    }
    observations.push(observation);
  }
  return JSON.stringify({observations, hasMore: list.hasMore});
})()`
