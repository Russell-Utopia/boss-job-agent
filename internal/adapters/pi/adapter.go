package pi

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
)

//go:embed confirm_assessment_results.ts
var confirmationExtension []byte

type Confirmer func(context.Context, assessment.ConfirmationBatch) (assessment.ConfirmationReceipt, error)

type Config struct {
	Executable string
	Arguments  []string
	// RuntimeDir stores only this worker's private process identity and
	// per-process markers. It must be an absolute path when provided.
	RuntimeDir string
	// Inspector is injectable only so controlled tests can model PID reuse or
	// an unverifiable process. Production uses the operating-system inspector.
	Inspector ProcessInspector
}

type Adapter struct {
	mu           sync.Mutex
	config       Config
	confirmer    Confirmer
	inspector    ProcessInspector
	runtimeDir   string
	instanceID   string
	runtimeReady bool
	runtimeErr   error
	active       *activeSubmission
	closed       bool
}

func New(confirmer Confirmer) *Adapter {
	return NewWithConfig(Config{Executable: "pi"}, confirmer)
}

func NewWithConfig(config Config, confirmer Confirmer) *Adapter {
	if config.Executable == "" {
		config.Executable = "pi"
	}
	inspector := config.Inspector
	if inspector == nil {
		inspector = systemProcessInspector{}
	}
	return &Adapter{config: config, confirmer: confirmer, inspector: inspector}
}

func (a *Adapter) Submit(ctx context.Context, request assessment.AssessmentRequest) error {
	childContext, finish, err := a.begin(ctx)
	if err != nil {
		return submissionError(classifyBeginError(err), err)
	}
	defer finish()
	if a.confirmer == nil {
		return submissionError(
			assessment.SubmissionErrorInvalidProtocol,
			fmt.Errorf("submit assessment through Pi: confirmation handler is required"),
		)
	}
	extensionPath, err := writeConfirmationExtension()
	if err != nil {
		return submissionError(assessment.SubmissionErrorTransient, err)
	}
	defer func() { _ = os.Remove(extensionPath) }()
	token, err := newCallbackToken()
	if err != nil {
		return submissionError(
			assessment.SubmissionErrorTransient,
			fmt.Errorf("create Pi confirmation token: %w", err),
		)
	}
	callback, err := startConfirmationServer(
		childContext,
		token,
		request.TraceID,
		confirmationAttempts(request.Jobs),
		a.confirmer,
	)
	if err != nil {
		return submissionError(assessment.SubmissionErrorTransient, err)
	}
	defer callback.close()
	return a.runRPC(childContext, request, extensionPath, callback, token)
}

func confirmationAttempts(jobs []assessment.AssessmentJobInput) []assessment.ConfirmationAttempt {
	attempts := make([]assessment.ConfirmationAttempt, 0, len(jobs))
	for _, job := range jobs {
		attempts = append(attempts, assessment.ConfirmationAttempt{
			JobID: job.JobID, AttemptNo: job.AttemptNo,
		})
	}
	return attempts
}

func classifyBeginError(err error) assessment.SubmissionErrorCategory {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return assessment.SubmissionErrorTransient
	}
	return assessment.SubmissionErrorInvalidProtocol
}

func (a *Adapter) begin(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := a.Recover(ctx); err != nil {
		return nil, nil, err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, nil, fmt.Errorf("submit assessment through Pi: Adapter is closed")
	}
	if a.active != nil {
		a.mu.Unlock()
		return nil, nil, fmt.Errorf("submit assessment through Pi: another request is still active")
	}
	childContext, cancel := context.WithCancel(ctx)
	active := &activeSubmission{cancel: cancel, done: make(chan struct{})}
	a.active = active
	a.mu.Unlock()
	finish := func() {
		cancel()
		a.mu.Lock()
		if a.active == active {
			a.active = nil
			close(active.done)
		}
		a.mu.Unlock()
	}
	return childContext, finish, nil
}

func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	active := a.active
	if active != nil {
		active.cancel()
	}
	a.mu.Unlock()
	if active != nil {
		select {
		case <-active.done:
		case <-ctx.Done():
			return fmt.Errorf("close Pi: wait for active request: %w", ctx.Err())
		}
	}
	return a.Recover(ctx)
}

func (a *Adapter) runRPC(
	ctx context.Context,
	request assessment.AssessmentRequest,
	extensionPath string,
	callback *confirmationServer,
	token string,
) error {
	directory, instanceID, err := a.ensureRuntime()
	if err != nil {
		return submissionError(assessment.SubmissionErrorInvalidProtocol, err)
	}
	runToken, err := randomHex(processRunTokenBytes)
	if err != nil {
		return submissionError(assessment.SubmissionErrorInvalidProtocol, fmt.Errorf("create Pi run token: %w", err))
	}
	arguments := append([]string(nil), a.config.Arguments...)
	arguments = append(arguments, "--mode", "rpc", "--no-session", "--no-tools", "-e", extensionPath)
	// New fixes the executable to pi; configurable execution exists only for controlled adapter tests.
	// A never-canceled context keeps CommandContext from bypassing the
	// ownership-checked shutdown below.
	command := exec.CommandContext(context.Background(), a.config.Executable, arguments...) //nolint:gosec // Test configuration must inject a local fake Pi executable.
	stdin, err := command.StdinPipe()
	if err != nil {
		return submissionError(assessment.SubmissionErrorTransient, fmt.Errorf("open Pi RPC input: %w", err))
	}
	var stdout bytes.Buffer
	command.Stdout = &stdout
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Env = callbackEnvironment(os.Environ(), callback.url(), token)
	if err := command.Start(); err != nil {
		return submissionError(assessment.SubmissionErrorTransient, fmt.Errorf("start Pi RPC: %w", err))
	}
	waitResult := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		waitResult <- command.Wait()
		close(waitDone)
	}()
	managed, err := a.attachProcess(command.Process, stdin, waitResult, directory, instanceID, runToken)
	if err != nil {
		// The child has started, but its identity could not be proven. Do not
		// signal or kill it; the absence of a verified owner is a reportable
		// safety stop.
		return submissionError(assessment.SubmissionErrorInvalidProtocol, err)
	}
	prompt, err := assessmentPrompt(request)
	if err != nil {
		_, _, _ = a.shutdownManagedProcess(managed)
		return submissionError(assessment.SubmissionErrorInvalidProtocol, err)
	}
	if err := json.NewEncoder(stdin).Encode(map[string]any{
		"id": "assessment-submit", "type": "prompt", "message": prompt,
	}); err != nil {
		_, _, _ = a.shutdownManagedProcess(managed)
		return submissionError(assessment.SubmissionErrorTransient, fmt.Errorf("send Pi assessment prompt: %w", err))
	}
	scanResult := make(chan rpcScanResult, 1)
	go func() {
		<-waitDone
		scanResult <- scanRPC(strings.NewReader(stdout.String()))
	}()
	select {
	case result := <-scanResult:
		return a.finishRPC(ctx, callback, result, managed, &stderr)
	case <-ctx.Done():
		return a.interruptRPC(managed, ctx.Err())
	}
}

func (a *Adapter) finishRPC(
	ctx context.Context,
	callback *confirmationServer,
	result rpcScanResult,
	managed *managedProcess,
	stderr *bytes.Buffer,
) error {
	waitErr, shutdownErr, forced := a.shutdownManagedProcess(managed)
	if shutdownErr != nil {
		return submissionError(assessment.SubmissionErrorInvalidProtocol, shutdownErr)
	}
	stderrText := stderr.String()
	if err := validateRPCExecution(ctx, result, waitErr, forced, stderrText); err != nil {
		return err
	}
	if !result.accepted {
		return submissionError(assessment.SubmissionErrorInvalidProtocol, fmt.Errorf("pi RPC did not accept the assessment prompt: %s", strings.TrimSpace(stderrText)))
	}
	if err := callback.result(); err != nil {
		if callback.called() {
			return submissionError(assessment.SubmissionErrorTransient, err)
		}
		return submissionError(assessment.SubmissionErrorInvalidProtocol, err)
	}
	return nil
}

func validateRPCExecution(
	ctx context.Context,
	result rpcScanResult,
	waitErr error,
	forced bool,
	stderr string,
) error {
	if result.err != nil {
		return submissionError(assessment.SubmissionErrorInvalidProtocol, result.err)
	}
	if ctx.Err() != nil {
		return submissionError(assessment.SubmissionErrorTransient, fmt.Errorf("pi RPC interrupted: %w", ctx.Err()))
	}
	if waitErr != nil && (!forced || !result.completed) {
		return submissionError(assessment.SubmissionErrorTransient, fmt.Errorf("pi RPC exited: %w: %s", waitErr, strings.TrimSpace(stderr)))
	}
	if !result.completed {
		return submissionError(assessment.SubmissionErrorTransient, fmt.Errorf("pi RPC ended before agent_end: %s", strings.TrimSpace(stderr)))
	}
	return nil
}

func (a *Adapter) interruptRPC(managed *managedProcess, cause error) error {
	_, shutdownErr, _ := a.shutdownManagedProcess(managed)
	if shutdownErr != nil {
		return submissionError(assessment.SubmissionErrorInvalidProtocol, shutdownErr)
	}
	return submissionError(assessment.SubmissionErrorTransient, fmt.Errorf("pi RPC interrupted: %w", cause))
}

func submissionError(category assessment.SubmissionErrorCategory, err error) error {
	return &assessment.SubmissionError{Category: category, Err: err}
}

type rpcScanResult struct {
	accepted  bool
	completed bool
	err       error
}

func scanRPC(stdout io.Reader) rpcScanResult {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	result := rpcScanResult{}
	for scanner.Scan() {
		var event struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Command string `json:"command"`
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			result.err = fmt.Errorf("decode Pi RPC event: %w", err)
			return result
		}
		if event.Type == "response" && event.ID == "assessment-submit" && event.Command == "prompt" {
			if !event.Success {
				result.err = fmt.Errorf("pi RPC rejected assessment prompt: %s", event.Error)
				return result
			}
			result.accepted = true
		}
		if event.Type == "agent_end" {
			result.completed = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		result.err = fmt.Errorf("read Pi RPC event: %w", err)
	}
	return result
}

func assessmentPrompt(request assessment.AssessmentRequest) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode assessment request: %w", err)
	}
	return "你是 BOSS Job Agent 的岗位鉴定器。只依据下面请求中的完整在线简历、完整岗位鉴定策略和完整岗位输入逐项判断。" +
		"每项只能给出 suitable、unsuitable 或 needs_user_confirmation；必须包含中文理由和结构化证据。" +
		"必须且只能调用一次 confirm_assessment_results，提交全部岗位的 jobId、attemptNo、status、reason、evidence；" +
		"普通文本不会保存任何结论。请求 JSON：\n" + string(encoded), nil
}

func writeConfirmationExtension() (string, error) {
	file, err := os.CreateTemp("", "boss-job-agent-confirm-assessment-*.ts")
	if err != nil {
		return "", fmt.Errorf("create Pi confirmation extension: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("protect Pi confirmation extension: %w", err)
	}
	if _, err := file.Write(confirmationExtension); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write Pi confirmation extension: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close Pi confirmation extension: %w", err)
	}
	return path, nil
}
