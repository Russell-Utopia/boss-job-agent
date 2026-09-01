package boss

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Russell-Utopia/boss-job-agent/internal/outreach"
)

const (
	outreachSession = "boss-job-agent-outreach"
	outreachGroup   = "BOSS Job Agent 首次打招呼"
)

//go:embed outreach_probe.js
var outreachProbeSource string

// Outreach is the production BOSS adapter for both the read-only contact
// check and the real first-contact action. Both operations share this one
// WebBridge session so a check and its subsequent send see the same tab state.
type Outreach struct {
	bridge *webBridge
}

func NewDefaultOutreach() *Outreach {
	return NewOutreach(defaultWebBridgeEndpoint, http.DefaultClient)
}

func NewOutreach(endpoint string, client *http.Client) *Outreach {
	return &Outreach{bridge: newWebBridge(endpoint, client, outreachSession)}
}

func (a *Outreach) Check(ctx context.Context, ref outreach.PlatformJobRef) (outreach.ContactStatus, error) {
	ref, err := validateOutreachRef(ref)
	if err != nil {
		return outreach.ContactStatus{}, classifyOutreachError(err)
	}
	if err := a.prepareOutreachTab(ctx, ref.CanonicalURL); err != nil {
		return outreach.ContactStatus{}, classifyOutreachError(err)
	}
	value, err := a.evaluateOutreach(ctx, "check", ref.PlatformJobID, "")
	if err != nil {
		return outreach.ContactStatus{}, classifyOutreachError(err)
	}
	var raw rawContactStatus
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return outreach.ContactStatus{}, classifyOutreachError(newAdapterFailure(
			adapterFailureInvalidProtocol, fmt.Errorf("decode BOSS contact status: %w", err),
		))
	}
	if raw.PlatformJobID != ref.PlatformJobID || !json.Valid(raw.Evidence) {
		return outreach.ContactStatus{}, classifyOutreachError(newAdapterFailure(
			adapterFailureInvalidResponse, errors.New("BOSS contact status did not prove the requested job"),
		))
	}
	return outreach.ContactStatus{
		Open: raw.Open, AlreadyContacted: raw.AlreadyContacted, Evidence: raw.Evidence,
	}, nil
}

func (a *Outreach) Send(ctx context.Context, request outreach.FirstContactRequest) (outreach.FirstContactResult, error) {
	ref, err := validateOutreachRef(outreach.PlatformJobRef{
		PlatformJobID: request.PlatformJobID, CanonicalURL: request.CanonicalURL,
	})
	if err != nil {
		return confirmedNoEffectResult(request.PlatformJobID), classifyOutreachError(err)
	}
	greeting := strings.Join(strings.Fields(request.GreetingText), " ")
	if greeting == "" {
		err := newAdapterFailure(adapterFailureInvalidResponse, errors.New("BOSS first contact requires a non-empty greeting"))
		return confirmedNoEffectResult(ref.PlatformJobID), classifyOutreachError(err)
	}
	if err := a.prepareOutreachTab(ctx, ref.CanonicalURL); err != nil {
		return confirmedNoEffectResult(ref.PlatformJobID), classifyOutreachError(err)
	}
	value, err := a.evaluateOutreach(ctx, "send", ref.PlatformJobID, greeting)
	if err != nil {
		return possiblyEffectiveResult(ref.PlatformJobID), classifyOutreachError(err)
	}
	var raw rawFirstContactResult
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		cause := newAdapterFailure(
			adapterFailureInvalidProtocol, fmt.Errorf("decode BOSS first contact result: %w", err),
		)
		return possiblyEffectiveResult(ref.PlatformJobID), classifyOutreachError(cause)
	}
	if raw.PlatformJobID != ref.PlatformJobID || !json.Valid(raw.Evidence) {
		cause := newAdapterFailure(
			adapterFailureInvalidResponse, errors.New("BOSS first contact result did not prove the requested job"),
		)
		return possiblyEffectiveResult(ref.PlatformJobID), classifyOutreachError(cause)
	}
	result := outreach.FirstContactResult{Effect: outreach.OutreachEffect(raw.Effect), Evidence: raw.Evidence}
	if !validOutreachEffect(result.Effect) {
		cause := newAdapterFailure(adapterFailureInvalidProtocol, fmt.Errorf("BOSS first contact returned invalid effect %q", raw.Effect))
		return possiblyEffectiveResult(ref.PlatformJobID), classifyOutreachError(cause)
	}
	return result, nil
}

func outreachEvidence(platformJobID, stage, effect string) json.RawMessage {
	evidence, _ := json.Marshal(map[string]string{
		"platformJobId": platformJobID, "stage": stage, "effect": effect,
	})
	return evidence
}

func confirmedNoEffectResult(platformJobID string) outreach.FirstContactResult {
	return outreach.FirstContactResult{
		Effect:   outreach.OutreachEffectConfirmedNoEffect,
		Evidence: outreachEvidence(platformJobID, "send_preflight", "confirmed_no_effect"),
	}
}

func possiblyEffectiveResult(platformJobID string) outreach.FirstContactResult {
	return outreach.FirstContactResult{
		Effect:   outreach.OutreachEffectPossiblyEffective,
		Evidence: outreachEvidence(platformJobID, "send_unconfirmed", "possibly_effective"),
	}
}

type rawContactStatus struct {
	PlatformJobID    string          `json:"platformJobId"`
	Open             bool            `json:"open"`
	AlreadyContacted bool            `json:"alreadyContacted"`
	Evidence         json.RawMessage `json:"evidence"`
}

type rawFirstContactResult struct {
	PlatformJobID string          `json:"platformJobId"`
	Effect        string          `json:"effect"`
	Evidence      json.RawMessage `json:"evidence"`
}

func (a *Outreach) prepareOutreachTab(ctx context.Context, target string) error {
	newTab, err := a.bridge.tabNeedsOpening(ctx, target)
	if err != nil {
		return err
	}
	args := map[string]any{"url": target, "newTab": newTab}
	if newTab {
		args["group_title"] = outreachGroup
	}
	var navigation navigationResult
	if err := a.bridge.command(ctx, "navigate", args, &navigation); err != nil {
		return fmt.Errorf("navigate to BOSS outreach job: %w", err)
	}
	if !navigation.Success {
		return newAdapterFailure(adapterFailureInvalidResponse, errors.New("navigate to BOSS outreach job returned unsuccessful result"))
	}
	return nil
}

func (a *Outreach) evaluateOutreach(ctx context.Context, mode, platformJobID, greeting string) (string, error) {
	input, err := json.Marshal(map[string]string{
		"mode": mode, "platformJobId": platformJobID, "greetingText": greeting,
	})
	if err != nil {
		return "", newAdapterFailure(adapterFailureInvalidProtocol, fmt.Errorf("encode BOSS outreach input: %w", err))
	}
	script := strings.Replace(outreachProbeSource, "__OUTREACH_INPUT__", string(input), 1)
	var evaluation evaluationResult
	if err := a.bridge.command(ctx, "evaluate", map[string]any{"code": script}, &evaluation); err != nil {
		return "", fmt.Errorf("evaluate BOSS outreach: %w", err)
	}
	if evaluation.Type != "string" || evaluation.Value == "" {
		return "", newAdapterFailure(adapterFailureInvalidProtocol, errors.New("BOSS outreach extraction returned a non-string result"))
	}
	return evaluation.Value, nil
}

func validateOutreachRef(ref outreach.PlatformJobRef) (outreach.PlatformJobRef, error) {
	ref.PlatformJobID = strings.TrimSpace(ref.PlatformJobID)
	ref.CanonicalURL = strings.TrimSpace(ref.CanonicalURL)
	if ref.PlatformJobID == "" || ref.CanonicalURL == "" {
		return outreach.PlatformJobRef{}, newAdapterFailure(adapterFailureInvalidResponse, errors.New("BOSS outreach requires a platform job ID and canonical URL"))
	}
	parsed, err := url.Parse(ref.CanonicalURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "www.zhipin.com" || !strings.Contains(parsed.Path, "/job_detail/") {
		return outreach.PlatformJobRef{}, newAdapterFailure(adapterFailureInvalidResponse, errors.New("BOSS outreach requires a canonical zhipin job URL"))
	}
	return ref, nil
}

func validOutreachEffect(effect outreach.OutreachEffect) bool {
	return effect == outreach.OutreachEffectConfirmedSent ||
		effect == outreach.OutreachEffectConfirmedNoEffect ||
		effect == outreach.OutreachEffectPossiblyEffective
}

func classifyOutreachError(cause error) error {
	if cause == nil {
		return nil
	}
	message := strings.ToLower(cause.Error())
	category := outreach.ErrorTransient
	switch {
	case strings.Contains(message, "boss_authentication_required"):
		category = outreach.ErrorAuthenticationExpired
	case strings.Contains(message, "boss_verification_required"):
		category = outreach.ErrorVerificationRequired
	case strings.Contains(message, "boss_platform_limited"):
		category = outreach.ErrorPlatformLimited
	case strings.Contains(message, "boss_outreach_unreliable"):
		category = outreach.ErrorInvalidResponse
	default:
		var failure *adapterFailure
		if errors.As(cause, &failure) {
			switch failure.kind {
			case adapterFailureInvalidResponse:
				category = outreach.ErrorInvalidResponse
			case adapterFailureInvalidProtocol:
				category = outreach.ErrorInvalidProtocol
			}
		}
	}
	return &outreach.ActionError{Category: category, Err: cause}
}
