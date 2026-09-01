package assessment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

const minimumValidationSamples = 2

// PolicyAdvisor is the model seam used only by page-session policy
// optimization. It has no callback into JobPool and therefore cannot save a
// real platform-job assessment as a side effect.
type PolicyAdvisor interface {
	Generate(context.Context, PolicyGenerationRequest) (PolicyDraft, error)
	Validate(context.Context, PolicyValidationRequest) (PolicyValidationResult, error)
}

// PolicyAdvisorError preserves the stable category of an external policy
// attempt without exposing Pi or another provider's error type to the module.
type PolicyAdvisorError struct {
	Category PolicyAdvisorErrorCategory
	Err      error
}

func (e *PolicyAdvisorError) Error() string { return e.Err.Error() }

func (e *PolicyAdvisorError) Unwrap() error { return e.Err }

type PolicyAdvisorErrorCategory string

const (
	PolicyAdvisorErrorTransient       PolicyAdvisorErrorCategory = "transient"
	PolicyAdvisorErrorAuthentication  PolicyAdvisorErrorCategory = "authentication_expired"
	PolicyAdvisorErrorVerification    PolicyAdvisorErrorCategory = "verification_required"
	PolicyAdvisorErrorPlatformLimited PolicyAdvisorErrorCategory = "platform_limited"
	PolicyAdvisorErrorInvalidResponse PolicyAdvisorErrorCategory = "invalid_response"
	PolicyAdvisorErrorInvalidProtocol PolicyAdvisorErrorCategory = "invalid_protocol"
	PolicyAdvisorErrorUnknown         PolicyAdvisorErrorCategory = "unknown"
)

type PolicyGenerationRequest struct {
	Resume        onlineresume.ResumeContent  `json:"resume"`
	ResumeVersion int                         `json:"resumeVersion"`
	Policy        Policy                      `json:"policy"`
	Samples       []jobpool.HumanReviewSample `json:"samples"`
}

type PolicyValidationRequest struct {
	Resume              onlineresume.ResumeContent  `json:"resume"`
	ResumeVersion       int                         `json:"resumeVersion"`
	Policy              Policy                      `json:"policy"`
	Candidate           Policy                      `json:"candidate"`
	Samples             []jobpool.HumanReviewSample `json:"samples"`
	GenerationSampleIDs []int64                     `json:"generationSampleIds"`
}

type PolicyValidationResult struct {
	Results []PolicyValidationComparison `json:"results"`
}

type PolicyValidationComparison struct {
	JobID           int64                    `json:"jobId"`
	CurrentStatus   jobpool.AssessmentStatus `json:"currentStatus"`
	CandidateStatus jobpool.AssessmentStatus `json:"candidateStatus"`
}

type PolicyOptimizationView struct {
	CurrentResume *onlineresume.Version       `json:"currentResume"`
	ActivePolicy  Policy                      `json:"activePolicy"`
	Samples       []jobpool.HumanReviewSample `json:"samples"`
}

// PolicyDraft is intentionally serializable: the browser sends this metadata
// back for validation because the draft itself is page-session state, not a
// row in SQLite. Resume and policy IDs refer to immutable saved versions.
type PolicyDraft struct {
	Text                         string                    `json:"text"`
	ResumeVersionID              int64                     `json:"resumeVersionId"`
	ResumeVersion                int                       `json:"resumeVersion"`
	PolicyVersionID              int64                     `json:"policyVersionId"`
	PolicyVersion                int                       `json:"policyVersion"`
	Policy                       Policy                    `json:"policy"`
	GeneratedAt                  time.Time                 `json:"generatedAt"`
	GenerationJobIDs             []int64                   `json:"generationJobIds"`
	GenerationSampleFingerprints []PolicySampleFingerprint `json:"generationSampleFingerprints"`
	GenerationSampleCount        int                       `json:"generationSampleCount"`
	ValidationEnabled            bool                      `json:"validationEnabled"`
}

type PolicySampleFingerprint struct {
	JobID        int64                `json:"jobId"`
	JDHash       string               `json:"jdHash"`
	HumanVerdict jobpool.HumanVerdict `json:"humanVerdict"`
}

type PolicyValidationStatus string

const (
	PolicyValidationPassed       PolicyValidationStatus = "passed"
	PolicyValidationFailed       PolicyValidationStatus = "failed"
	PolicyValidationTradeoff     PolicyValidationStatus = "tradeoff"
	PolicyValidationInsufficient PolicyValidationStatus = "insufficient"
)

type PolicyValidationCase struct {
	JobID           int64                    `json:"jobId"`
	JobTitle        string                   `json:"jobTitle"`
	HumanVerdict    jobpool.HumanVerdict     `json:"humanVerdict"`
	CurrentStatus   jobpool.AssessmentStatus `json:"currentStatus"`
	CandidateStatus jobpool.AssessmentStatus `json:"candidateStatus"`
	InGenerationSet bool                     `json:"inGenerationSet"`
}

type PolicyValidationMetrics struct {
	FalsePositive         int `json:"falsePositive"`
	FalseNegative         int `json:"falseNegative"`
	NeedsUserConfirmation int `json:"needsUserConfirmation"`
}

type PolicyValidationReport struct {
	Status                PolicyValidationStatus  `json:"status"`
	Summary               string                  `json:"summary"`
	ResumeVersion         int                     `json:"resumeVersion"`
	PolicyVersion         int                     `json:"policyVersion"`
	GeneratedAt           time.Time               `json:"generatedAt"`
	GenerationSampleCount int                     `json:"generationSampleCount"`
	FullResults           []PolicyValidationCase  `json:"fullResults"`
	UngeneratedResults    []PolicyValidationCase  `json:"ungeneratedResults"`
	CurrentMetrics        PolicyValidationMetrics `json:"currentMetrics"`
	CandidateMetrics      PolicyValidationMetrics `json:"candidateMetrics"`
}

type Rejection struct {
	Code   string
	Reason string
	cause  error
}

func (r *Rejection) Error() string { return r.Reason }

func (r *Rejection) Unwrap() error { return r.cause }

func (r *Rejection) RejectionCode() string { return r.Code }

func (r *Rejection) RejectionReason() string { return r.Reason }

func (s *Service) GetPolicyOptimization(ctx context.Context) (PolicyOptimizationView, error) {
	if s.resumeVersions == nil || s.pool == nil {
		return PolicyOptimizationView{}, fmt.Errorf("get policy optimization: service dependencies are not configured")
	}
	resume, err := s.resumeVersions.GetCurrent(ctx)
	if err != nil {
		return PolicyOptimizationView{}, fmt.Errorf("get policy optimization resume: %w", err)
	}
	policy, err := s.GetActivePolicy(ctx)
	if err != nil {
		return PolicyOptimizationView{}, err
	}
	samples, err := s.pool.ListEffectiveHumanReviews(ctx)
	if err != nil {
		return PolicyOptimizationView{}, err
	}
	return PolicyOptimizationView{CurrentResume: resume, ActivePolicy: policy, Samples: samples}, nil
}

func (s *Service) GeneratePolicyDraft(ctx context.Context, generationJobIDs []int64) (PolicyDraft, error) {
	if s.advisor == nil {
		return PolicyDraft{}, &Rejection{Code: "policy_advisor_unavailable", Reason: "策略优化暂不可用，请稍后重试"}
	}
	view, err := s.GetPolicyOptimization(ctx)
	if err != nil {
		return PolicyDraft{}, err
	}
	if view.CurrentResume == nil {
		return PolicyDraft{}, &Rejection{Code: "online_resume_required", Reason: "请先刷新并保存在线简历，再生成策略候选稿"}
	}
	selected, err := selectPolicySamples(view.Samples, generationJobIDs)
	if err != nil {
		return PolicyDraft{}, err
	}
	startedAt := s.currentTime()
	trace, err := s.logs.Start(ctx, runlog.Attempt{
		Flow: runlog.FlowAssessment, Operation: runlog.OperationGeneratePolicy, AttemptNo: 1,
	})
	if err != nil {
		return PolicyDraft{}, fmt.Errorf("start policy generation trace: %w", err)
	}
	response, err := s.advisor.Generate(ctx, PolicyGenerationRequest{
		Resume: view.CurrentResume.Content, ResumeVersion: view.CurrentResume.Version,
		Policy: view.ActivePolicy, Samples: selected,
	})
	if err != nil {
		return PolicyDraft{}, finishPolicyAttempt(ctx, s.logs, trace, runlog.ErrorCategoryUnknown, err)
	}
	text := strings.TrimSpace(response.Text)
	if _, err := policyFromText(text); err != nil {
		finishErr := finishPolicyAttempt(ctx, s.logs, trace, runlog.ErrorCategoryInvalidProtocol, err)
		return PolicyDraft{}, &Rejection{Code: "policy_draft_invalid", Reason: "模型没有返回完整可采用的策略候选稿", cause: errors.Join(err, finishErr)}
	}
	if err := finishPolicyAttempt(ctx, s.logs, trace, "", nil); err != nil {
		return PolicyDraft{}, fmt.Errorf("finish policy generation trace: %w", err)
	}
	ids := make([]int64, 0, len(selected))
	fingerprints := make([]PolicySampleFingerprint, 0, len(selected))
	for _, sample := range selected {
		ids = append(ids, sample.JobID)
		fingerprints = append(fingerprints, PolicySampleFingerprint{
			JobID: sample.JobID, JDHash: sample.JDHash, HumanVerdict: sample.Verdict,
		})
	}
	return PolicyDraft{
		Text: text, ResumeVersionID: view.CurrentResume.ID, ResumeVersion: view.CurrentResume.Version,
		PolicyVersionID: view.ActivePolicy.ID, PolicyVersion: view.ActivePolicy.Version,
		Policy: view.ActivePolicy, GeneratedAt: startedAt, GenerationJobIDs: ids,
		GenerationSampleFingerprints: fingerprints,
		GenerationSampleCount:        len(selected),
	}, nil
}

func (s *Service) ValidatePolicyDraft(ctx context.Context, draft PolicyDraft) (PolicyValidationReport, error) {
	if s.advisor == nil {
		return PolicyValidationReport{}, &Rejection{Code: "policy_advisor_unavailable", Reason: "策略验收暂不可用，请稍后重试"}
	}
	if err := validatePolicyDraftMetadata(draft); err != nil {
		return PolicyValidationReport{}, err
	}
	resume, basePolicy, candidate, allSamples, err := s.loadPolicyValidationInput(ctx, draft)
	if err != nil {
		return PolicyValidationReport{}, err
	}
	if len(allSamples) < minimumValidationSamples {
		return insufficientValidationReport(draft, len(draft.GenerationJobIDs)), nil
	}
	trace, err := s.logs.Start(ctx, runlog.Attempt{
		Flow: runlog.FlowAssessment, Operation: runlog.OperationValidatePolicy, AttemptNo: 1,
	})
	if err != nil {
		return PolicyValidationReport{}, fmt.Errorf("start policy validation trace: %w", err)
	}
	response, err := s.advisor.Validate(ctx, PolicyValidationRequest{
		Resume: resume.Content, ResumeVersion: resume.Version, Policy: basePolicy,
		Candidate: candidate, Samples: allSamples,
		GenerationSampleIDs: append([]int64(nil), draft.GenerationJobIDs...),
	})
	if err != nil {
		return PolicyValidationReport{}, finishPolicyAttempt(ctx, s.logs, trace, runlog.ErrorCategoryUnknown, err)
	}
	comparisons, err := indexComparisons(allSamples, response.Results)
	if err != nil {
		finishErr := finishPolicyAttempt(ctx, s.logs, trace, runlog.ErrorCategoryInvalidProtocol, err)
		return PolicyValidationReport{}, errors.Join(err, finishErr)
	}
	if err := finishPolicyAttempt(ctx, s.logs, trace, "", nil); err != nil {
		return PolicyValidationReport{}, fmt.Errorf("finish policy validation trace: %w", err)
	}
	return buildValidationReport(draft, allSamples, comparisons), nil
}

func finishPolicyAttempt(ctx context.Context, logs *runlog.Log, trace runlog.Trace, category runlog.ErrorCategory, cause error) error {
	if cause == nil {
		return logs.Finish(ctx, trace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded})
	}
	finishErr := logs.Finish(ctx, trace, runlog.AttemptResult{
		Outcome: runlog.OutcomeFailed, ErrorCategory: policyAdvisorErrorCategory(cause, category), Err: cause,
	})
	return errors.Join(fmt.Errorf("policy advisor request: %w", cause), finishErr)
}

func policyAdvisorErrorCategory(cause error, fallback runlog.ErrorCategory) runlog.ErrorCategory {
	var classified *PolicyAdvisorError
	if errors.As(cause, &classified) {
		if category, ok := mapPolicyAdvisorErrorCategory(classified.Category); ok {
			return category
		}
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return runlog.ErrorCategoryTransient
	}
	return fallback
}

func mapPolicyAdvisorErrorCategory(category PolicyAdvisorErrorCategory) (runlog.ErrorCategory, bool) {
	switch category {
	case PolicyAdvisorErrorTransient:
		return runlog.ErrorCategoryTransient, true
	case PolicyAdvisorErrorInvalidProtocol:
		return runlog.ErrorCategoryInvalidProtocol, true
	case PolicyAdvisorErrorAuthentication:
		return runlog.ErrorCategoryAuthenticationExpired, true
	case PolicyAdvisorErrorVerification:
		return runlog.ErrorCategoryVerificationRequired, true
	case PolicyAdvisorErrorPlatformLimited:
		return runlog.ErrorCategoryPlatformLimited, true
	case PolicyAdvisorErrorInvalidResponse:
		return runlog.ErrorCategoryInvalidResponse, true
	case PolicyAdvisorErrorUnknown:
		return runlog.ErrorCategoryUnknown, true
	default:
		return "", false
	}
}

func validatePolicyDraftMetadata(draft PolicyDraft) error {
	if !draft.ValidationEnabled {
		return &Rejection{Code: "policy_validation_not_enabled", Reason: "本候选稿生成前没有开启策略验收，请重新生成"}
	}
	if draft.ResumeVersionID <= 0 || draft.PolicyVersionID <= 0 || draft.ResumeVersion <= 0 || draft.PolicyVersion <= 0 {
		return &Rejection{Code: "policy_draft_invalid", Reason: "候选稿版本信息无效，请重新生成"}
	}
	return nil
}

func (s *Service) loadPolicyValidationVersions(ctx context.Context, draft PolicyDraft) (*onlineresume.Version, Policy, error) {
	resume, err := s.resumeVersions.Get(ctx, draft.ResumeVersionID)
	if err != nil {
		return nil, Policy{}, err
	}
	if resume == nil || resume.Version != draft.ResumeVersion {
		return nil, Policy{}, &Rejection{Code: "policy_draft_expired", Reason: "在线简历版本已无法确认，请重新生成候选稿"}
	}
	basePolicy, err := s.getPolicyVersion(ctx, draft.PolicyVersionID)
	if err != nil {
		return nil, Policy{}, err
	}
	if basePolicy.Version != draft.PolicyVersion {
		return nil, Policy{}, &Rejection{Code: "policy_draft_expired", Reason: "正式策略版本已无法确认，请重新生成候选稿"}
	}
	return resume, basePolicy, nil
}

func (s *Service) loadPolicyValidationInput(
	ctx context.Context,
	draft PolicyDraft,
) (*onlineresume.Version, Policy, Policy, []jobpool.HumanReviewSample, error) {
	if s.resumeVersions == nil || s.pool == nil {
		return nil, Policy{}, Policy{}, nil, fmt.Errorf("validate policy draft: service dependencies are not configured")
	}
	resume, basePolicy, err := s.loadPolicyValidationVersions(ctx, draft)
	if err != nil {
		return nil, Policy{}, Policy{}, nil, err
	}
	candidate, err := policyFromText(draft.Text)
	if err != nil {
		return nil, Policy{}, Policy{}, nil, &Rejection{Code: "policy_draft_invalid", Reason: "候选稿不是完整可采用的策略文本", cause: err}
	}
	allSamples, err := s.pool.ListEffectiveHumanReviews(ctx)
	if err != nil {
		return nil, Policy{}, Policy{}, nil, err
	}
	if _, err := selectPolicySamples(allSamples, draft.GenerationJobIDs); err != nil {
		return nil, Policy{}, Policy{}, nil, &Rejection{Code: "policy_samples_changed", Reason: "人工复核样本已变化，请重新生成候选稿", cause: err}
	}
	if err := verifyPolicySampleFingerprints(allSamples, draft); err != nil {
		return nil, Policy{}, Policy{}, nil, err
	}
	return resume, basePolicy, candidate, allSamples, nil
}

func (s *Service) CreatePolicyVersion(ctx context.Context, rules []string, changeNote string) (int64, error) {
	return s.createPolicyVersion(ctx, rules, changeNote, 0)
}

// CreatePolicyVersionIfCurrent adopts a page-session draft only when its
// immutable base policy is still the active version. This makes a lost adopt
// response safe to retry without persisting page-session state or history.
func (s *Service) CreatePolicyVersionIfCurrent(ctx context.Context, rules []string, changeNote string, expectedPolicyID int64) (int64, error) {
	if expectedPolicyID <= 0 {
		return 0, &Rejection{Code: "policy_draft_invalid", Reason: "候选稿缺少正式策略版本，请重新生成"}
	}
	return s.createPolicyVersion(ctx, rules, changeNote, expectedPolicyID)
}

func (s *Service) createPolicyVersion(ctx context.Context, rules []string, changeNote string, expectedPolicyID int64) (int64, error) {
	cleanRules, err := normalizePolicyRules(rules)
	if err != nil {
		return 0, &Rejection{Code: "policy_rules_required", Reason: "策略必须包含至少一条完整规则", cause: err}
	}
	if s.now == nil {
		return 0, fmt.Errorf("create policy version: current time is not configured")
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("create policy version: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := verifyExpectedActivePolicy(ctx, queries, expectedPolicyID); err != nil {
		return 0, err
	}
	next, err := queries.GetNextPolicyVersionNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("create policy version: get next version: %w", err)
	}
	if err := queries.DeactivatePolicies(ctx); err != nil {
		return 0, fmt.Errorf("create policy version: deactivate current policy: %w", err)
	}
	rulesJSON, err := json.Marshal(struct {
		Rules []string `json:"rules"`
	}{Rules: cleanRules})
	if err != nil {
		return 0, fmt.Errorf("create policy version: encode rules: %w", err)
	}
	created, err := queries.CreatePolicyVersion(ctx, sqlitedb.CreatePolicyVersionParams{
		VersionNo: next, RulesJson: string(rulesJSON), ChangeNote: nullablePolicyText(changeNote),
		CreatedAt: s.now().UnixMilli(),
	})
	if err != nil {
		return 0, fmt.Errorf("create policy version: insert version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("create policy version: commit: %w", err)
	}
	return created.ID, nil
}

func verifyExpectedActivePolicy(ctx context.Context, queries *sqlitedb.Queries, expectedPolicyID int64) error {
	if expectedPolicyID <= 0 {
		return nil
	}
	active, err := queries.GetActivePolicy(ctx)
	if err != nil {
		return fmt.Errorf("create policy version: get current policy: %w", err)
	}
	if active.ID != expectedPolicyID {
		return &Rejection{Code: "policy_draft_expired", Reason: "正式策略已经变化，请重新生成候选稿"}
	}
	return nil
}

func (s *Service) getPolicyVersion(ctx context.Context, id int64) (Policy, error) {
	row, err := s.queries.GetPolicyVersion(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, &Rejection{Code: "policy_draft_expired", Reason: "正式策略版本不存在，请重新生成候选稿"}
	}
	if err != nil {
		return Policy{}, fmt.Errorf("query policy version %d: %w", id, err)
	}
	return policyFromRow(row.ID, row.VersionNo, row.RulesJson, row.ChangeNote)
}

func policyFromRow(id, version int64, rulesJSON string, changeNote sql.NullString) (Policy, error) {
	var document struct {
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal([]byte(rulesJSON), &document); err != nil {
		return Policy{}, fmt.Errorf("decode assessment policy: %w", err)
	}
	name := fmt.Sprintf("策略 v%d", version)
	if version == 1 && changeNote.String == "系统默认策略" {
		name = "默认策略 v1"
	}
	return Policy{ID: id, Version: int(version), Name: name, Rules: document.Rules}, nil
}

func selectPolicySamples(all []jobpool.HumanReviewSample, ids []int64) ([]jobpool.HumanReviewSample, error) {
	if len(ids) == 0 {
		return nil, &Rejection{Code: "policy_samples_required", Reason: "至少需要一条有效人工复核作为策略生成样本"}
	}
	byID := make(map[int64]jobpool.HumanReviewSample, len(all))
	for _, sample := range all {
		byID[sample.JobID] = sample
	}
	selected := make([]jobpool.HumanReviewSample, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, &Rejection{Code: "policy_samples_invalid", Reason: "策略生成样本编号无效"}
		}
		if _, ok := seen[id]; ok {
			return nil, &Rejection{Code: "policy_samples_invalid", Reason: "策略生成样本不能重复选择"}
		}
		seen[id] = struct{}{}
		sample, ok := byID[id]
		if !ok {
			return nil, &Rejection{Code: "policy_samples_changed", Reason: "人工复核样本已变化，请重新生成候选稿"}
		}
		selected = append(selected, sample)
	}
	return selected, nil
}

func verifyPolicySampleFingerprints(all []jobpool.HumanReviewSample, draft PolicyDraft) error {
	if draft.GenerationSampleCount != len(draft.GenerationJobIDs) || len(draft.GenerationSampleFingerprints) != len(draft.GenerationJobIDs) {
		return &Rejection{Code: "policy_samples_changed", Reason: "候选稿缺少生成时的样本依据，请重新生成"}
	}
	generationIDs := make(map[int64]struct{}, len(draft.GenerationJobIDs))
	for _, id := range draft.GenerationJobIDs {
		generationIDs[id] = struct{}{}
	}
	byID := make(map[int64]jobpool.HumanReviewSample, len(all))
	for _, sample := range all {
		byID[sample.JobID] = sample
	}
	seen := make(map[int64]struct{}, len(draft.GenerationSampleFingerprints))
	for _, fingerprint := range draft.GenerationSampleFingerprints {
		if err := verifyPolicySampleFingerprint(fingerprint, generationIDs, seen, byID); err != nil {
			return err
		}
	}
	if len(seen) != len(generationIDs) {
		return &Rejection{Code: "policy_samples_changed", Reason: "候选稿遗漏生成样本依据，请重新生成"}
	}
	return nil
}

func verifyPolicySampleFingerprint(
	fingerprint PolicySampleFingerprint,
	generationIDs, seen map[int64]struct{},
	byID map[int64]jobpool.HumanReviewSample,
) error {
	if _, expected := generationIDs[fingerprint.JobID]; !expected {
		return &Rejection{Code: "policy_samples_changed", Reason: "候选稿生成样本依据无效，请重新生成"}
	}
	if _, duplicate := seen[fingerprint.JobID]; duplicate {
		return &Rejection{Code: "policy_samples_changed", Reason: "候选稿生成样本依据无效，请重新生成"}
	}
	seen[fingerprint.JobID] = struct{}{}
	current, ok := byID[fingerprint.JobID]
	if !ok || current.JDHash != fingerprint.JDHash || current.Verdict != fingerprint.HumanVerdict {
		return &Rejection{Code: "policy_samples_changed", Reason: "生成样本的 JD 或人工结论已变化，请重新生成候选稿"}
	}
	return nil
}

func policyFromText(text string) (Policy, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Policy{}, errors.New("policy text is empty")
	}
	if strings.HasPrefix(text, "{") {
		var document struct {
			Name  string   `json:"name"`
			Rules []string `json:"rules"`
		}
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&document); err != nil {
			return Policy{}, fmt.Errorf("decode policy text: %w", err)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return Policy{}, fmt.Errorf("decode policy text: %w", err)
		}
		rules, err := normalizePolicyRules(document.Rules)
		if err != nil {
			return Policy{}, err
		}
		return Policy{Name: document.Name, Rules: rules}, nil
	}
	rules, err := normalizePolicyRules(strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"))
	if err != nil {
		return Policy{}, err
	}
	return Policy{Rules: rules}, nil
}

func normalizePolicyRules(rules []string) ([]string, error) {
	clean := make([]string, 0, len(rules))
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule != "" {
			clean = append(clean, rule)
		}
	}
	if len(clean) == 0 {
		return nil, errors.New("policy must contain at least one rule")
	}
	return clean, nil
}

// ParsePolicyRules converts editable page-session text into the complete rule
// list accepted by the immutable policy-version store.
func ParsePolicyRules(text string) ([]string, error) {
	policy, err := policyFromText(text)
	if err != nil {
		return nil, err
	}
	return policy.Rules, nil
}

func insufficientValidationReport(draft PolicyDraft, sampleCount int) PolicyValidationReport {
	return PolicyValidationReport{
		Status: PolicyValidationInsufficient, Summary: "有效人工复核样本不足，无法可靠验收",
		ResumeVersion: draft.ResumeVersion, PolicyVersion: draft.PolicyVersion,
		GeneratedAt: draft.GeneratedAt, GenerationSampleCount: sampleCount,
	}
}

func indexComparisons(samples []jobpool.HumanReviewSample, results []PolicyValidationComparison) (map[int64]PolicyValidationComparison, error) {
	if len(results) != len(samples) {
		return nil, &Rejection{Code: "policy_validation_invalid", Reason: "策略验收没有返回全部样本结果"}
	}
	byID := make(map[int64]PolicyValidationComparison, len(results))
	for _, result := range results {
		if _, exists := byID[result.JobID]; exists {
			return nil, &Rejection{Code: "policy_validation_invalid", Reason: "策略验收返回了重复岗位结果"}
		}
		if !validValidationStatus(result.CurrentStatus) || !validValidationStatus(result.CandidateStatus) {
			return nil, &Rejection{Code: "policy_validation_invalid", Reason: "策略验收返回了无效鉴定状态"}
		}
		byID[result.JobID] = result
	}
	for _, sample := range samples {
		if _, ok := byID[sample.JobID]; !ok {
			return nil, &Rejection{Code: "policy_validation_invalid", Reason: "策略验收遗漏了有效人工复核"}
		}
	}
	return byID, nil
}

func buildValidationReport(draft PolicyDraft, samples []jobpool.HumanReviewSample, comparisons map[int64]PolicyValidationComparison) PolicyValidationReport {
	set := make(map[int64]struct{}, len(draft.GenerationJobIDs))
	for _, id := range draft.GenerationJobIDs {
		set[id] = struct{}{}
	}
	report := PolicyValidationReport{
		ResumeVersion: draft.ResumeVersion, PolicyVersion: draft.PolicyVersion,
		GeneratedAt: draft.GeneratedAt, GenerationSampleCount: len(draft.GenerationJobIDs),
		FullResults: make([]PolicyValidationCase, 0, len(samples)),
	}
	for _, sample := range samples {
		comparison := comparisons[sample.JobID]
		item := PolicyValidationCase{
			JobID: sample.JobID, JobTitle: sample.JobTitle, HumanVerdict: sample.Verdict,
			CurrentStatus: comparison.CurrentStatus, CandidateStatus: comparison.CandidateStatus,
		}
		_, item.InGenerationSet = set[sample.JobID]
		report.FullResults = append(report.FullResults, item)
		if !item.InGenerationSet {
			report.UngeneratedResults = append(report.UngeneratedResults, item)
		}
	}
	report.CurrentMetrics = validationMetrics(report.FullResults, false)
	report.CandidateMetrics = validationMetrics(report.FullResults, true)
	report.Status, report.Summary = validationStatus(report.CurrentMetrics, report.CandidateMetrics)
	return report
}

func validationMetrics(results []PolicyValidationCase, candidate bool) PolicyValidationMetrics {
	metrics := PolicyValidationMetrics{}
	for _, result := range results {
		status := result.CurrentStatus
		if candidate {
			status = result.CandidateStatus
		}
		if result.HumanVerdict == jobpool.HumanVerdictUnsuitable && status == jobpool.AssessmentStatusSuitable {
			metrics.FalsePositive++
		}
		if result.HumanVerdict == jobpool.HumanVerdictSuitable && status == jobpool.AssessmentStatusUnsuitable {
			metrics.FalseNegative++
		}
		if status == jobpool.AssessmentStatusNeedsUserConfirmation {
			metrics.NeedsUserConfirmation++
		}
	}
	return metrics
}

func validationStatus(current, candidate PolicyValidationMetrics) (PolicyValidationStatus, string) {
	if validationHasTradeoff(current, candidate) {
		return PolicyValidationTradeoff, "候选策略有改善也有退步，结果有取舍，需要人工判断"
	}
	if validationImprovesWithoutNewErrors(current, candidate) {
		return PolicyValidationPassed, "候选策略降低了错误或人工确认需求，验收通过"
	}
	return PolicyValidationFailed, "候选策略没有在不增加错误的前提下改善判断，验收未通过"
}

func validationHasTradeoff(current, candidate PolicyValidationMetrics) bool {
	falsePositiveReduced := candidate.FalsePositive < current.FalsePositive
	falseNegativeReduced := candidate.FalseNegative < current.FalseNegative
	falsePositiveWorse := candidate.FalsePositive > current.FalsePositive
	falseNegativeWorse := candidate.FalseNegative > current.FalseNegative
	return (falsePositiveReduced && falseNegativeWorse) || (falseNegativeReduced && falsePositiveWorse)
}

func validationImprovesWithoutNewErrors(current, candidate PolicyValidationMetrics) bool {
	falsePositiveWorse := candidate.FalsePositive > current.FalsePositive
	falseNegativeWorse := candidate.FalseNegative > current.FalseNegative
	if falsePositiveWorse || falseNegativeWorse {
		return false
	}
	falsePositiveReduced := candidate.FalsePositive < current.FalsePositive
	falseNegativeReduced := candidate.FalseNegative < current.FalseNegative
	confirmationReduced := candidate.NeedsUserConfirmation < current.NeedsUserConfirmation
	return falsePositiveReduced || falseNegativeReduced ||
		(!falsePositiveReduced && !falseNegativeReduced && confirmationReduced)
}

func validValidationStatus(status jobpool.AssessmentStatus) bool {
	switch status {
	case jobpool.AssessmentStatusSuitable, jobpool.AssessmentStatusUnsuitable, jobpool.AssessmentStatusNeedsUserConfirmation:
		return true
	default:
		return false
	}
}

func (s *Service) currentTime() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func nullablePolicyText(value string) sql.NullString {
	value = strings.TrimSpace(value)
	return sql.NullString{String: value, Valid: value != ""}
}
