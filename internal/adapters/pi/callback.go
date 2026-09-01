package pi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment"
)

func newCallbackToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type confirmationServer struct {
	listener net.Listener
	server   *http.Server
	mu       sync.Mutex
	calls    int
	err      error
}

func startConfirmationServer(
	ctx context.Context,
	token string,
	traceID string,
	expectedAttempts []assessment.ConfirmationAttempt,
	confirmer Confirmer,
) (*confirmationServer, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for Pi confirmations: %w", err)
	}
	callback := &confirmationServer{listener: listener}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /confirm", callback.confirmHandler(token, traceID, expectedAttempts, confirmer))
	callback.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		serveErr := callback.server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			callback.mu.Lock()
			callback.err = fmt.Errorf("serve Pi confirmation callback: %w", serveErr)
			callback.mu.Unlock()
		}
	}()
	return callback, nil
}

func (s *confirmationServer) confirmHandler(
	token string,
	traceID string,
	expectedAttempts []assessment.ConfirmationAttempt,
	confirmer Confirmer,
) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "confirmation token is invalid", http.StatusUnauthorized)
			return
		}
		var batch assessment.ConfirmationBatch
		decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&batch); err != nil {
			http.Error(w, "confirmation body is invalid", http.StatusBadRequest)
			return
		}
		var trailing json.RawMessage
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			http.Error(w, "confirmation body is invalid", http.StatusBadRequest)
			return
		}
		batch.TraceID = traceID
		batch.ExpectedAttempts = append([]assessment.ConfirmationAttempt(nil), expectedAttempts...)
		receipt, err := confirmer(request.Context(), batch)
		s.mu.Lock()
		s.calls++
		if err != nil {
			s.err = err
		}
		s.mu.Unlock()
		if err != nil {
			http.Error(w, "confirmation failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(receipt)
	}
}

func (s *confirmationServer) url() string {
	return "http://" + s.listener.Addr().String() + "/confirm"
}

func (s *confirmationServer) result() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return fmt.Errorf("confirm Pi assessment results: %w", s.err)
	}
	if s.calls == 0 {
		return fmt.Errorf("pi assessment ended without calling confirm_assessment_results")
	}
	return nil
}

func (s *confirmationServer) called() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls > 0
}

func (s *confirmationServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
}

func callbackEnvironment(environment []string, url, token string) []string {
	filtered := make([]string, 0, len(environment)+2)
	for _, value := range environment {
		if strings.HasPrefix(value, "BOSS_JOB_AGENT_CONFIRM_URL=") ||
			strings.HasPrefix(value, "BOSS_JOB_AGENT_CONFIRM_TOKEN=") {
			continue
		}
		filtered = append(filtered, value)
	}
	return append(filtered,
		"BOSS_JOB_AGENT_CONFIRM_URL="+url,
		"BOSS_JOB_AGENT_CONFIRM_TOKEN="+token,
	)
}
