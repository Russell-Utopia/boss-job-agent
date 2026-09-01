package pi

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
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
}

type Adapter struct {
	mu        sync.Mutex
	config    Config
	confirmer Confirmer
	active    bool
	closed    bool
	cancel    context.CancelFunc
}

func New(confirmer Confirmer) *Adapter {
	return NewWithConfig(Config{Executable: "pi"}, confirmer)
}

func NewWithConfig(config Config, confirmer Confirmer) *Adapter {
	if config.Executable == "" {
		config.Executable = "pi"
	}
	return &Adapter{config: config, confirmer: confirmer}
}

func (a *Adapter) Submit(ctx context.Context, request assessment.AssessmentRequest) error {
	childContext, finish, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if a.confirmer == nil {
		return fmt.Errorf("submit assessment through Pi: confirmation handler is required")
	}
	extensionPath, err := writeConfirmationExtension()
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(extensionPath) }()
	token, err := newCallbackToken()
	if err != nil {
		return fmt.Errorf("create Pi confirmation token: %w", err)
	}
	callback, err := startConfirmationServer(
		childContext,
		token,
		request.TraceID,
		confirmationAttempts(request.Jobs),
		a.confirmer,
	)
	if err != nil {
		return err
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

func (a *Adapter) begin(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, nil, fmt.Errorf("submit assessment through Pi: Adapter is closed")
	}
	if a.active {
		return nil, nil, fmt.Errorf("submit assessment through Pi: another request is still active")
	}
	childContext, cancel := context.WithCancel(ctx)
	a.active = true
	a.cancel = cancel
	finish := func() {
		cancel()
		a.mu.Lock()
		a.active = false
		a.cancel = nil
		a.mu.Unlock()
	}
	return childContext, finish, nil
}

func (a *Adapter) Close(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	if a.cancel != nil {
		a.cancel()
	}
	return nil
}

func (a *Adapter) runRPC(
	ctx context.Context,
	request assessment.AssessmentRequest,
	extensionPath string,
	callback *confirmationServer,
	token string,
) error {
	arguments := append([]string(nil), a.config.Arguments...)
	arguments = append(arguments, "--mode", "rpc", "--no-session", "--no-tools", "-e", extensionPath)
	// New fixes the executable to pi; configurable execution exists only for controlled adapter tests.
	command := exec.CommandContext(ctx, a.config.Executable, arguments...) //nolint:gosec // Test configuration must inject a local fake Pi executable.
	stdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Pi RPC input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Pi RPC output: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Env = callbackEnvironment(os.Environ(), callback.url(), token)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Pi RPC: %w", err)
	}
	prompt, err := assessmentPrompt(request)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	if err := json.NewEncoder(stdin).Encode(map[string]any{
		"id": "assessment-submit", "type": "prompt", "message": prompt,
	}); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("send Pi assessment prompt: %w", err)
	}
	accepted, scanErr := scanRPC(stdout)
	_ = stdin.Close()
	waitErr := command.Wait()
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return fmt.Errorf("pi RPC exited: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if !accepted {
		return fmt.Errorf("pi RPC did not accept the assessment prompt: %s", strings.TrimSpace(stderr.String()))
	}
	if err := callback.result(); err != nil {
		return err
	}
	return nil
}

func scanRPC(stdout io.Reader) (bool, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	accepted := false
	for scanner.Scan() {
		var event struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Command string `json:"command"`
			Success bool   `json:"success"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return false, fmt.Errorf("decode Pi RPC event: %w", err)
		}
		if event.Type == "response" && event.ID == "assessment-submit" && event.Command == "prompt" {
			if !event.Success {
				return false, fmt.Errorf("pi RPC rejected assessment prompt: %s", event.Error)
			}
			accepted = true
		}
		if event.Type == "agent_end" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read Pi RPC event: %w", err)
	}
	return accepted, nil
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
