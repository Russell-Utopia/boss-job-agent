package boss

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
)

const (
	visiblePageProbeSession          = "boss-job-agent-visible-page-probe"
	visiblePageProbeGroup            = "BOSS Job Agent 可见页面只读对照"
	visiblePageProbeMaxJobs          = 8
	visiblePageExhaustionUnavailable = "unavailable"
)

//go:embed visible_page_probe.js
var visiblePageProbeSource string

// visiblePageProbe is a deliberately unwired research adapter. It reads only
// the DOM rendered on an explicitly selected BOSS job-search page and cannot
// establish search exhaustion.
type visiblePageProbe struct {
	bridge *webBridge
}

type visiblePageProbeResult struct {
	Jobs               []visiblePageJob
	ScannedCardCount   int
	Truncated          bool
	ExhaustionEvidence string
}

type visibleJDStructure string

const (
	visibleJDExplicitSplit        visibleJDStructure = "explicit_split"
	visibleJDResponsibilitiesOnly visibleJDStructure = "responsibilities_only"
	visibleJDRequirementsOnly     visibleJDStructure = "requirements_only"
	visibleJDUnstructured         visibleJDStructure = "unstructured"
)

type visibleSalaryEvidence string

const (
	visibleSalaryReadable    visibleSalaryEvidence = "readable"
	visibleSalaryUnavailable visibleSalaryEvidence = "unavailable"
)

// visiblePageJob is research evidence, not a production JobObservation. FullJD
// is the authoritative rendered text; the split fields are optional derived
// values whose absence must never be filled with invented content.
type visiblePageJob struct {
	PlatformJobID    string
	CanonicalURL     string
	JobTitle         string
	CompanyName      string
	City             string
	Salary           string
	SalaryEvidence   visibleSalaryEvidence
	FullJD           string
	Responsibilities string
	Requirements     string
	JDStructure      visibleJDStructure
	PlatformStatus   discovery.PlatformStatus
}

type rawVisiblePageProbeResult struct {
	Jobs               []rawDiscoveryJob `json:"jobs"`
	ScannedCardCount   int               `json:"scannedCardCount"`
	Truncated          bool              `json:"truncated"`
	ExhaustionEvidence string            `json:"exhaustionEvidence"`
}

func newVisiblePageProbe(endpoint string, client *http.Client) *visiblePageProbe {
	return &visiblePageProbe{bridge: newWebBridge(endpoint, client, visiblePageProbeSession)}
}

func (p *visiblePageProbe) read(
	ctx context.Context,
	targetURL string,
	limit int,
) (visiblePageProbeResult, error) {
	if err := validateVisiblePageProbeInput(targetURL, limit); err != nil {
		return visiblePageProbeResult{}, classifyDiscoveryError(err)
	}
	if err := p.prepareTab(ctx, targetURL); err != nil {
		return visiblePageProbeResult{}, classifyDiscoveryError(err)
	}
	script, err := buildVisiblePageProbeScript(limit)
	if err != nil {
		return visiblePageProbeResult{}, classifyDiscoveryError(err)
	}
	var evaluation evaluationResult
	if err := p.bridge.command(ctx, "evaluate", map[string]any{"code": script}, &evaluation); err != nil {
		return visiblePageProbeResult{}, classifyDiscoveryError(
			fmt.Errorf("read BOSS visible job page: %w", err),
		)
	}
	if evaluation.Type != "string" || evaluation.Value == "" {
		return visiblePageProbeResult{}, classifyDiscoveryError(newAdapterFailure(
			adapterFailureInvalidProtocol,
			errors.New("BOSS visible page probe returned a non-string result"),
		))
	}
	result, err := decodeVisiblePageProbeResult(evaluation.Value)
	if err != nil {
		return visiblePageProbeResult{}, classifyDiscoveryError(newAdapterFailure(
			adapterFailureInvalidResponse,
			fmt.Errorf("decode BOSS visible page probe: %w", err),
		))
	}
	return result, nil
}

func (p *visiblePageProbe) prepareTab(ctx context.Context, targetURL string) error {
	newTab, err := p.bridge.tabNeedsOpening(ctx, jobSearchURL)
	if err != nil {
		return err
	}
	args := map[string]any{"url": targetURL, "newTab": newTab}
	if newTab {
		args["group_title"] = visiblePageProbeGroup
	}
	var navigation navigationResult
	if err := p.bridge.command(ctx, "navigate", args, &navigation); err != nil {
		return fmt.Errorf("navigate to BOSS visible job page: %w", err)
	}
	if !navigation.Success {
		return newAdapterFailure(
			adapterFailureInvalidResponse,
			errors.New("navigate to BOSS visible job page returned unsuccessful result"),
		)
	}
	return nil
}

func validateVisiblePageProbeInput(targetURL string, limit int) error {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return newAdapterFailure(adapterFailureInvalidResponse, fmt.Errorf("parse visible page target: %w", err))
	}
	if parsed.Scheme != "https" || parsed.Hostname() != "www.zhipin.com" || parsed.Path != "/web/geek/job" || parsed.User != nil {
		return newAdapterFailure(
			adapterFailureInvalidResponse,
			errors.New("visible page probe requires an HTTPS www.zhipin.com/web/geek/job URL"),
		)
	}
	if limit < 1 || limit > visiblePageProbeMaxJobs {
		return newAdapterFailure(
			adapterFailureInvalidResponse,
			fmt.Errorf("visible page probe limit must be between 1 and %d", visiblePageProbeMaxJobs),
		)
	}
	return nil
}

func buildVisiblePageProbeScript(limit int) (string, error) {
	input, err := json.Marshal(struct {
		Limit int `json:"limit"`
	}{Limit: limit})
	if err != nil {
		return "", newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("encode visible page probe input: %w", err),
		)
	}
	return "(async () => {\n" + visiblePageProbeSource +
		"\nreturn await BossVisiblePageProbe.run(" + string(input) + ");\n})()", nil
}

func decodeVisiblePageProbeResult(value string) (visiblePageProbeResult, error) {
	var raw rawVisiblePageProbeResult
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return visiblePageProbeResult{}, err
	}
	if len(raw.Jobs) == 0 || raw.ScannedCardCount < len(raw.Jobs) {
		return visiblePageProbeResult{}, errors.New("visible page probe requires at least one stable job")
	}
	if raw.ExhaustionEvidence != visiblePageExhaustionUnavailable {
		return visiblePageProbeResult{}, errors.New("visible page probe must not infer search exhaustion")
	}
	jobs := make([]visiblePageJob, 0, len(raw.Jobs))
	for _, rawJob := range raw.Jobs {
		job, err := visiblePageJobFromRaw(rawJob)
		if err != nil {
			return visiblePageProbeResult{}, err
		}
		jobs = append(jobs, job)
	}
	return visiblePageProbeResult{
		Jobs:               jobs,
		ScannedCardCount:   raw.ScannedCardCount,
		Truncated:          raw.Truncated,
		ExhaustionEvidence: strings.TrimSpace(raw.ExhaustionEvidence),
	}, nil
}

var (
	visibleResponsibilitiesHeading = regexp.MustCompile(
		`^(?:岗位职责|职位职责|工作职责|职位描述)[\t ]*(?:[：:][\t ]*|\n)`,
	)
	visibleRequirementsHeading = regexp.MustCompile(
		`(?m)(?:^[\t ]*(?:任职要求|岗位要求|职位要求|任职资格)[\t ]*\n|(?:任职要求|岗位要求|职位要求|任职资格)[\t ]*[：:][\t ]*)`,
	)
)

func visiblePageJobFromRaw(rawJob rawDiscoveryJob) (visiblePageJob, error) {
	platformJobID := strings.TrimSpace(rawJob.PlatformJobID)
	if platformJobID == "" || strings.TrimSpace(rawJob.DetailPlatformJobID) != platformJobID {
		return visiblePageJob{}, fmt.Errorf(
			"BOSS visible detail does not confirm listed platform job %q", rawJob.PlatformJobID,
		)
	}
	if strings.TrimSpace(rawJob.PlatformStatusEvidence) != "招聘中" {
		return visiblePageJob{}, fmt.Errorf(
			"BOSS visible platform job %q has no reliable open status", platformJobID,
		)
	}
	canonicalURL := strings.TrimSpace(rawJob.CanonicalURL)
	jobTitle := strings.TrimSpace(rawJob.JobTitle)
	companyName := strings.TrimSpace(rawJob.CompanyName)
	city := strings.TrimSpace(rawJob.City)
	fullJD := normalizeVisibleJD(rawJob.FullJD)
	for _, value := range []string{canonicalURL, jobTitle, companyName, city, fullJD} {
		if value == "" {
			return visiblePageJob{}, fmt.Errorf(
				"BOSS visible platform job %q lacks stable identity, basic information, or rendered JD", platformJobID,
			)
		}
	}
	salary, salaryEvidence, err := visibleSalaryFromRaw(platformJobID, rawJob)
	if err != nil {
		return visiblePageJob{}, err
	}
	responsibilities, requirements, structure := classifyVisibleJD(fullJD)
	return visiblePageJob{
		PlatformJobID:    platformJobID,
		CanonicalURL:     canonicalURL,
		JobTitle:         jobTitle,
		CompanyName:      companyName,
		City:             city,
		Salary:           salary,
		SalaryEvidence:   salaryEvidence,
		FullJD:           fullJD,
		Responsibilities: responsibilities,
		Requirements:     requirements,
		JDStructure:      structure,
		PlatformStatus:   discovery.PlatformStatusOpen,
	}, nil
}

func visibleSalaryFromRaw(
	platformJobID string,
	rawJob rawDiscoveryJob,
) (string, visibleSalaryEvidence, error) {
	salary := strings.TrimSpace(rawJob.Salary)
	salaryEvidence := visibleSalaryEvidence(strings.TrimSpace(rawJob.SalaryEvidence))
	switch salaryEvidence {
	case visibleSalaryReadable:
		if salary == "" || containsPrivateUseCharacters(salary) {
			return "", "", fmt.Errorf(
				"BOSS visible platform job %q claims a readable salary without readable text", platformJobID,
			)
		}
	case visibleSalaryUnavailable:
		if salary != "" {
			return "", "", fmt.Errorf(
				"BOSS visible platform job %q must clear unavailable salary text", platformJobID,
			)
		}
	default:
		return "", "", fmt.Errorf(
			"BOSS visible platform job %q has no salary reliability evidence", platformJobID,
		)
	}
	return salary, salaryEvidence, nil
}

func containsPrivateUseCharacters(value string) bool {
	for _, current := range value {
		if current >= '\uE000' && current <= '\uF8FF' ||
			current >= '\U000F0000' && current <= '\U000FFFFD' ||
			current >= '\U00100000' && current <= '\U0010FFFD' {
			return true
		}
	}
	return false
}

func normalizeVisibleJD(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
}

func classifyVisibleJD(value string) (string, string, visibleJDStructure) {
	fullJD := normalizeVisibleJD(value)
	requirementsBoundary := visibleRequirementsHeading.FindStringIndex(fullJD)
	if requirementsBoundary != nil {
		before := strings.TrimSpace(fullJD[:requirementsBoundary[0]])
		requirements := strings.TrimSpace(fullJD[requirementsBoundary[1]:])
		if before == "" {
			return "", requirements, visibleJDRequirementsOnly
		}
		responsibilities := strings.TrimSpace(visibleResponsibilitiesHeading.ReplaceAllString(before, ""))
		if responsibilities != "" && requirements != "" {
			return responsibilities, requirements, visibleJDExplicitSplit
		}
	}
	if heading := visibleResponsibilitiesHeading.FindStringIndex(fullJD); heading != nil {
		return strings.TrimSpace(fullJD[heading[1]:]), "", visibleJDResponsibilitiesOnly
	}
	return "", "", visibleJDUnstructured
}
