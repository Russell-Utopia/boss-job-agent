package boss

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxWebBridgeResponse = 4 << 20

type webBridge struct {
	endpoint string
	client   *http.Client
	session  string
}

func newWebBridge(endpoint string, client *http.Client, session string) *webBridge {
	if client == nil {
		client = http.DefaultClient
	}
	return &webBridge{
		endpoint: strings.TrimRight(endpoint, "/"),
		client:   client,
		session:  session,
	}
}

func (b *webBridge) tabNeedsOpening(ctx context.Context, url string) (bool, error) {
	var found navigationResult
	err := b.command(ctx, "find_tab", map[string]any{"url": url, "active": false}, &found)
	if err == nil {
		if !found.Success {
			return false, newAdapterFailure(
				adapterFailureInvalidResponse,
				errors.New("find BOSS tab returned unsuccessful result"),
			)
		}
		return false, nil
	}
	var bridgeErr *webBridgeError
	if errors.As(err, &bridgeErr) && strings.Contains(bridgeErr.Message, "no tab matching") {
		return true, nil
	}
	return false, fmt.Errorf("find BOSS tab: %w", err)
}

type webBridgeCommand struct {
	Action  string `json:"action"`
	Args    any    `json:"args"`
	Session string `json:"session"`
}

type webBridgeResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *webBridgeError `json:"error"`
}

type webBridgeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *webBridgeError) Error() string {
	return e.Code + ": " + e.Message
}

type navigationResult struct {
	Success bool   `json:"success"`
	URL     string `json:"url"`
	TabID   int64  `json:"tabId"`
}

type evaluationResult struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (b *webBridge) command(ctx context.Context, action string, args, result any) error {
	payload, err := json.Marshal(webBridgeCommand{Action: action, Args: args, Session: b.session})
	if err != nil {
		return newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("encode WebBridge %s command: %w", action, err),
		)
	}
	body, err := b.postCommand(ctx, action, payload)
	if err != nil {
		return err
	}
	return decodeCommandResponse(action, body, result)
}

func (b *webBridge) postCommand(ctx context.Context, action string, payload []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint+"/command", bytes.NewReader(payload))
	if err != nil {
		return nil, newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("create WebBridge %s request: %w", action, err),
		)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := b.client.Do(request)
	if err != nil {
		return nil, newAdapterFailure(
			adapterFailureTransient,
			fmt.Errorf("call WebBridge %s: %w", action, err),
		)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWebBridgeResponse+1))
	if err != nil {
		return nil, newAdapterFailure(
			adapterFailureTransient,
			fmt.Errorf("read WebBridge %s response: %w", action, err),
		)
	}
	if len(body) > maxWebBridgeResponse {
		return nil, newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("WebBridge %s response exceeds %d bytes", action, maxWebBridgeResponse),
		)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, newAdapterFailure(
			adapterFailureTransient,
			fmt.Errorf("WebBridge %s returned HTTP %d", action, response.StatusCode),
		)
	}
	return body, nil
}

func decodeCommandResponse(action string, body []byte, result any) error {
	var envelope webBridgeResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("decode WebBridge %s response: %w", action, err),
		)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return newAdapterFailure(adapterFailureTransient, envelope.Error)
		}
		return newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("WebBridge %s returned an unspecified error", action),
		)
	}
	if result == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("WebBridge %s response has no data", action),
		)
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return newAdapterFailure(
			adapterFailureInvalidProtocol,
			fmt.Errorf("decode WebBridge %s data: %w", action, err),
		)
	}
	return nil
}
