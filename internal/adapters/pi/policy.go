package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
)

// Generate implements assessment.PolicyAdvisor. Policy work uses ordinary
// Pi output and deliberately has no assessment confirmation callback.
func (a *Adapter) Generate(ctx context.Context, request assessment.PolicyGenerationRequest) (assessment.PolicyDraft, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return assessment.PolicyDraft{}, fmt.Errorf("encode policy generation request: %w", err)
	}
	stdout, err := a.runPolicyRPC(ctx, "policy-generate", "你是 BOSS Job Agent 的策略顾问。请根据完整在线简历、当前完整岗位鉴定策略和人工复核样本，返回一份完整、可直接采用的策略文本。不要返回零碎建议。请求 JSON：\n"+string(encoded))
	if err != nil {
		return assessment.PolicyDraft{}, wrapPolicyAdvisorError(ctx, err)
	}
	var response assessment.PolicyDraft
	if err := decodePolicyPayload(stdout, &response); err != nil {
		var text string
		if textErr := json.Unmarshal(stdout, &text); textErr == nil && strings.TrimSpace(text) != "" {
			return assessment.PolicyDraft{Text: text}, nil
		}
		if text := strings.TrimSpace(string(stdout)); text != "" {
			return assessment.PolicyDraft{Text: text}, nil
		}
		return assessment.PolicyDraft{}, newPolicyAdvisorError(assessment.PolicyAdvisorErrorInvalidProtocol, fmt.Errorf("decode policy generation response: %w", err))
	}
	return response, nil
}

// Validate implements assessment.PolicyAdvisor. It asks Pi for paired
// current/candidate classifications and never calls assessment.Service.Confirm.
func (a *Adapter) Validate(ctx context.Context, request assessment.PolicyValidationRequest) (assessment.PolicyValidationResult, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return assessment.PolicyValidationResult{}, fmt.Errorf("encode policy validation request: %w", err)
	}
	stdout, err := a.runPolicyRPC(ctx, "policy-validate", "你是 BOSS Job Agent 的策略验收器。请使用同一份完整在线简历，分别按当前策略和候选策略判断全部人工复核样本，只返回每个 jobId 的 currentStatus 与 candidateStatus。请求 JSON：\n"+string(encoded))
	if err != nil {
		return assessment.PolicyValidationResult{}, wrapPolicyAdvisorError(ctx, err)
	}
	var response assessment.PolicyValidationResult
	if err := decodePolicyPayload(stdout, &response); err != nil {
		return assessment.PolicyValidationResult{}, newPolicyAdvisorError(assessment.PolicyAdvisorErrorInvalidProtocol, fmt.Errorf("decode policy validation response: %w", err))
	}
	return response, nil
}

func wrapPolicyAdvisorError(ctx context.Context, err error) error {
	var classified *assessment.PolicyAdvisorError
	if errors.As(err, &classified) {
		return err
	}
	category := assessment.PolicyAdvisorErrorTransient
	var providerErr *policyRPCProviderError
	if errors.As(err, &providerErr) {
		return newPolicyAdvisorError(classifyPolicyProviderError(providerErr.message), err)
	}
	var protocolErr *policyRPCProtocolError
	if errors.As(err, &protocolErr) {
		return newPolicyAdvisorError(assessment.PolicyAdvisorErrorInvalidProtocol, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return newPolicyAdvisorError(category, err)
	}
	return newPolicyAdvisorError(category, err)
}

func newPolicyAdvisorError(category assessment.PolicyAdvisorErrorCategory, err error) error {
	return &assessment.PolicyAdvisorError{Category: category, Err: err}
}

func classifyPolicyProviderError(message string) assessment.PolicyAdvisorErrorCategory {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "login"), strings.Contains(message, "auth"), strings.Contains(message, "登录"), strings.Contains(message, "认证"):
		return assessment.PolicyAdvisorErrorAuthentication
	case strings.Contains(message, "captcha"), strings.Contains(message, "verification"), strings.Contains(message, "验证码"), strings.Contains(message, "验证"):
		return assessment.PolicyAdvisorErrorVerification
	case strings.Contains(message, "rate limit"), strings.Contains(message, "too many"), strings.Contains(message, "限流"), strings.Contains(message, "平台限制"):
		return assessment.PolicyAdvisorErrorPlatformLimited
	case strings.Contains(message, "invalid response"), strings.Contains(message, "响应无效"):
		return assessment.PolicyAdvisorErrorInvalidResponse
	default:
		return assessment.PolicyAdvisorErrorUnknown
	}
}

func (a *Adapter) runPolicyRPC(ctx context.Context, requestID, prompt string) ([]byte, error) {
	childContext, finish, err := a.begin(ctx)
	if err != nil {
		return nil, classifyPolicyBeginError(ctx, err)
	}
	defer finish()
	process, err := a.startPolicyRPCProcess()
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(process.stdin).Encode(map[string]any{
		"id": requestID, "type": "prompt", "message": prompt,
	}); err != nil {
		_, shutdownErr, _ := a.shutdownManagedProcess(process.managed)
		return nil, errors.Join(fmt.Errorf("send pi policy prompt: %w", err), shutdownErr)
	}
	return a.waitPolicyRPC(childContext, process, requestID)
}

type policyRPCProcess struct {
	managed  *managedProcess
	stdin    io.WriteCloser
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	waitDone <-chan struct{}
}

func (a *Adapter) startPolicyRPCProcess() (*policyRPCProcess, error) {
	directory, instanceID, err := a.ensureRuntime()
	if err != nil {
		return nil, fmt.Errorf("prepare pi policy runtime: %w", err)
	}
	runToken, err := randomHex(processRunTokenBytes)
	if err != nil {
		return nil, fmt.Errorf("create pi policy run token: %w", err)
	}
	arguments := append([]string(nil), a.config.Arguments...)
	arguments = append(arguments, "--mode", "rpc", "--no-session", "--no-tools")
	command := exec.CommandContext(context.Background(), a.config.Executable, arguments...) //nolint:gosec // Executable is fixed in production and injectable only in adapter tests.
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open pi policy input: %w", err)
	}
	var stdout bytes.Buffer
	command.Stdout = &stdout
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start pi policy process: %w", err)
	}
	waitResult := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		waitResult <- command.Wait()
		close(waitDone)
	}()
	managed, err := a.attachProcess(command.Process, stdin, waitResult, directory, instanceID, runToken)
	if err != nil {
		return nil, &policyRPCProtocolError{err: err}
	}
	return &policyRPCProcess{managed: managed, stdin: stdin, stdout: &stdout, stderr: &stderr, waitDone: waitDone}, nil
}

func (a *Adapter) waitPolicyRPC(ctx context.Context, process *policyRPCProcess, requestID string) ([]byte, error) {
	scanResult := make(chan policyRPCScanResult, 1)
	go func() {
		<-process.waitDone
		scanResult <- scanPolicyRPC(strings.NewReader(process.stdout.String()), requestID)
	}()
	select {
	case result := <-scanResult:
		return a.finishPolicyRPC(ctx, process, result)
	case <-ctx.Done():
		return a.interruptPolicyRPC(process, ctx.Err())
	}
}

func (a *Adapter) finishPolicyRPC(ctx context.Context, process *policyRPCProcess, result policyRPCScanResult) ([]byte, error) {
	waitErr, shutdownErr, forced := a.shutdownManagedProcess(process.managed)
	if shutdownErr != nil {
		return nil, &policyRPCProtocolError{err: shutdownErr}
	}
	if result.err != nil {
		return nil, result.err
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("pi policy request interrupted: %w", ctx.Err())
	}
	if waitErr != nil && (!forced || !result.completed) {
		return nil, fmt.Errorf("pi policy process exited: %w: %s", waitErr, strings.TrimSpace(process.stderr.String()))
	}
	if !result.completed || !result.accepted {
		return nil, &policyRPCProtocolError{err: fmt.Errorf("pi policy request was not completed: %s", strings.TrimSpace(process.stderr.String()))}
	}
	return result.payload, nil
}

func (a *Adapter) interruptPolicyRPC(process *policyRPCProcess, cause error) ([]byte, error) {
	_, shutdownErr, _ := a.shutdownManagedProcess(process.managed)
	return nil, errors.Join(fmt.Errorf("pi policy request interrupted: %w", cause), shutdownErr)
}

func classifyPolicyBeginError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	return &policyRPCProtocolError{err: fmt.Errorf("start pi policy request: %w", err)}
}

type policyRPCScanResult struct {
	accepted  bool
	completed bool
	payload   []byte
	err       error
}

type policyRPCProtocolError struct {
	err error
}

func (e *policyRPCProtocolError) Error() string { return e.err.Error() }

func (e *policyRPCProtocolError) Unwrap() error { return e.err }

type policyRPCProviderError struct {
	message string
}

func (e *policyRPCProviderError) Error() string { return e.message }

type policyRPCEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Text    string          `json:"text"`
	Data    json.RawMessage `json:"data"`
	Result  json.RawMessage `json:"result"`
	Message struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

func scanPolicyRPC(stdout io.Reader, requestID string) policyRPCScanResult {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	result := policyRPCScanResult{}
	for scanner.Scan() {
		event, err := decodePolicyRPCEvent(scanner.Bytes())
		if err != nil {
			result.err = &policyRPCProtocolError{err: fmt.Errorf("decode pi policy RPC event: %w", err)}
			return result
		}
		if err := recordPolicyRPCEvent(&result, event, requestID); err != nil {
			return result
		}
		if event.Type == "agent_end" {
			result.completed = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		result.err = fmt.Errorf("read pi policy RPC event: %w", err)
	}
	if result.err == nil && len(result.payload) == 0 {
		result.err = &policyRPCProtocolError{err: errors.New("pi policy RPC returned no policy payload")}
	}
	return result
}

func decodePolicyRPCEvent(data []byte) (policyRPCEvent, error) {
	var event policyRPCEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return policyRPCEvent{}, err
	}
	return event, nil
}

func recordPolicyRPCEvent(result *policyRPCScanResult, event policyRPCEvent, requestID string) error {
	if event.Type == "response" && event.ID == requestID && event.Command == "prompt" {
		if !event.Success {
			result.err = &policyRPCProviderError{message: fmt.Sprintf("pi policy prompt rejected: %s", event.Error)}
			return result.err
		}
		result.accepted = true
	}
	if payload := policyRPCEventPayload(event); len(payload) > 0 {
		result.payload = payload
	}
	return nil
}

func policyRPCEventPayload(event policyRPCEvent) []byte {
	var payload []byte
	if event.Text != "" {
		payload = []byte(event.Text)
	}
	if len(event.Data) > 0 && string(event.Data) != "null" {
		payload = append([]byte(nil), event.Data...)
	}
	if len(event.Result) > 0 && string(event.Result) != "null" {
		payload = append([]byte(nil), event.Result...)
	}
	for _, part := range event.Message.Content {
		if part.Text != "" {
			payload = []byte(part.Text)
		}
	}
	return payload
}

func decodePolicyPayload(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("policy payload has trailing JSON")
		}
		return err
	}
	return nil
}
