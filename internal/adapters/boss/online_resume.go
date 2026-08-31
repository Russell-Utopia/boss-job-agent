package boss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
)

const (
	defaultWebBridgeEndpoint = "http://127.0.0.1:10086"
	onlineResumeURL          = "https://www.zhipin.com/web/geek/resume"
	onlineResumeSession      = "boss-job-agent-online-resume"
	onlineResumeGroupTitle   = "BOSS Job Agent 在线简历"
)

// OnlineResume reads the user's BOSS online resume through the local Kimi
// WebBridge daemon and the already authenticated Chrome session.
type OnlineResume struct {
	bridge *webBridge
}

func NewDefaultOnlineResume() *OnlineResume {
	return NewOnlineResume(defaultWebBridgeEndpoint, http.DefaultClient)
}

func NewOnlineResume(endpoint string, client *http.Client) *OnlineResume {
	return &OnlineResume{bridge: newWebBridge(endpoint, client, onlineResumeSession)}
}

func (a *OnlineResume) Read(ctx context.Context) (onlineresume.ResumeContent, error) {
	newTab, err := a.bridge.tabNeedsOpening(ctx, onlineResumeURL)
	if err != nil {
		return onlineresume.ResumeContent{}, classifyReadError(err)
	}
	navigateArgs := map[string]any{"url": onlineResumeURL, "newTab": newTab}
	if newTab {
		navigateArgs["group_title"] = onlineResumeGroupTitle
	}
	var navigation navigationResult
	if err := a.bridge.command(ctx, "navigate", navigateArgs, &navigation); err != nil {
		return onlineresume.ResumeContent{}, classifyReadError(fmt.Errorf("navigate to BOSS online resume: %w", err))
	}
	if !navigation.Success {
		return onlineresume.ResumeContent{}, classifyReadError(newAdapterFailure(
			adapterFailureInvalidResponse,
			errors.New("navigate to BOSS online resume returned unsuccessful result"),
		))
	}

	var evaluation evaluationResult
	if err := a.bridge.command(ctx, "evaluate", map[string]any{"code": extractOnlineResumeScript}, &evaluation); err != nil {
		return onlineresume.ResumeContent{}, classifyReadError(fmt.Errorf("extract BOSS online resume: %w", err))
	}
	if evaluation.Type != "string" || evaluation.Value == "" {
		return onlineresume.ResumeContent{}, classifyReadError(newAdapterFailure(
			adapterFailureInvalidProtocol,
			errors.New("BOSS online resume extraction returned a non-string result"),
		))
	}
	var content onlineresume.ResumeContent
	if err := json.Unmarshal([]byte(evaluation.Value), &content); err != nil {
		return onlineresume.ResumeContent{}, classifyReadError(newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("decode BOSS online resume extraction: %w", err),
		))
	}
	if err := onlineresume.ValidateContent(content); err != nil {
		return onlineresume.ResumeContent{}, classifyReadError(newAdapterFailure(adapterFailureInvalidResponse, err))
	}
	return content, nil
}

func classifyReadError(cause error) error {
	var category onlineresume.ReadErrorCategory
	message := strings.ToLower(cause.Error())
	switch {
	case strings.Contains(message, "boss_authentication_required"):
		category = onlineresume.ReadErrorAuthenticationExpired
	case strings.Contains(message, "boss_verification_required"):
		category = onlineresume.ReadErrorVerificationRequired
	case strings.Contains(message, "boss_platform_limited"):
		category = onlineresume.ReadErrorPlatformLimited
	case strings.Contains(message, "boss_online_resume_required_section_missing"),
		strings.Contains(message, "boss_online_resume_job_intention_incomplete"):
		category = onlineresume.ReadErrorInvalidResponse
	default:
		category = categoryFromAdapterFailure(cause)
	}
	return &onlineresume.ReadError{
		Category:   category,
		UserReason: readErrorReason(category),
		Cause:      cause,
	}
}

type adapterFailureKind uint8

const (
	adapterFailureTransient adapterFailureKind = iota + 1
	adapterFailureInvalidResponse
	adapterFailureInvalidProtocol
)

type adapterFailure struct {
	kind  adapterFailureKind
	cause error
}

func newAdapterFailure(kind adapterFailureKind, cause error) error {
	return &adapterFailure{kind: kind, cause: cause}
}

func (e *adapterFailure) Error() string {
	return e.cause.Error()
}

func (e *adapterFailure) Unwrap() error {
	return e.cause
}

func categoryFromAdapterFailure(err error) onlineresume.ReadErrorCategory {
	var failure *adapterFailure
	if !errors.As(err, &failure) {
		return onlineresume.ReadErrorUnknown
	}
	switch failure.kind {
	case adapterFailureTransient:
		return onlineresume.ReadErrorTransient
	case adapterFailureInvalidResponse:
		return onlineresume.ReadErrorInvalidResponse
	case adapterFailureInvalidProtocol:
		return onlineresume.ReadErrorInvalidProtocol
	default:
		return onlineresume.ReadErrorUnknown
	}
}

func readErrorReason(category onlineresume.ReadErrorCategory) string {
	switch category {
	case onlineresume.ReadErrorAuthenticationExpired:
		return "BOSS 登录已失效，请在 Chrome 重新登录后再刷新"
	case onlineresume.ReadErrorVerificationRequired:
		return "BOSS 要求完成安全验证，请在 Chrome 处理后再刷新"
	case onlineresume.ReadErrorPlatformLimited:
		return "BOSS 暂时限制访问，请稍后手工刷新"
	case onlineresume.ReadErrorInvalidResponse, onlineresume.ReadErrorInvalidProtocol:
		return "BOSS 在线简历页面无法完整读取，已保留上一次可靠版本"
	default:
		return "暂时无法连接 BOSS，请确认 Chrome 与 Kimi WebBridge 正常后重试"
	}
}

const extractOnlineResumeScript = `(() => {
  const text = element => (element?.innerText || element?.textContent || "")
    .replace(/\r\n?/g, "\n")
    .split("\n")
    .map(value => value.trim())
    .filter(Boolean)
    .join("\n");
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
  const sections = {
    intentions: document.querySelector(".resume-purpose.resume-expectList"),
    work: document.querySelector(".resume-history.resume-workExpList"),
    projects: document.querySelector(".resume-project.resume-projectExpList"),
    educations: document.querySelector(".resume-education.resume-educationExpList"),
    skills: document.querySelector(".resume-professional-skill.resume-professionalSkill")
  };
  if (Object.values(sections).some(section => !section)) {
    throw new Error("BOSS_ONLINE_RESUME_REQUIRED_SECTION_MISSING");
  }
  const employmentType = text(sections.intentions.querySelector(".expect-list-title")).replace(/职位$/, "");
  const jobIntentions = [...sections.intentions.querySelectorAll(".expect-list-item .primary-info")].map(item => ({
    role: text(item.querySelector(".position-item .label-text")),
    city: text(item.querySelector(".city-item")),
    salary: text(item.querySelector(".money-item")),
    employmentType
  }));
  const collect = section => [...section.querySelectorAll(":scope > .item-primary > ul > li > .primary-info")].map(text);
  const result = {
    jobIntentions,
    workExperiences: collect(sections.work),
    projectExperiences: collect(sections.projects),
    educations: collect(sections.educations),
    skills: collect(sections.skills)
  };
  if (!jobIntentions.length || jobIntentions.some(item => Object.values(item).some(value => !value))) {
    throw new Error("BOSS_ONLINE_RESUME_JOB_INTENTION_INCOMPLETE");
  }
  return JSON.stringify(result);
})()`
