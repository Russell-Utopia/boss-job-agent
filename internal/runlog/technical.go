package runlog

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// TechnicalError is one operational failure that is not itself an external
// attempt result. Business tables keep only the state required for recovery.
type TechnicalError struct {
	Flow            Flow
	Stage           string
	TraceID         string
	DiscoveryRunID  int64
	PlatformJobID   string
	AttemptNo       int64
	Err             error
	ErrorRedactions []string
}

func (l *Log) RecordTechnicalError(ctx context.Context, failure TechnicalError) error {
	if err := validateTechnicalError(failure); err != nil {
		return err
	}
	traceID := failure.TraceID
	if traceID == "" {
		var err error
		traceID, err = newTraceID()
		if err != nil {
			return fmt.Errorf("generate technical error trace ID: %w", err)
		}
	}
	chain, truncated := snapshotErrorTree(failure.Err, failure.ErrorRedactions...)
	record := slog.NewRecord(l.now().UTC(), slog.LevelError, "technical error", 0)
	attrs := []slog.Attr{
		slog.Int("schema_version", 1),
		slog.String("event", "technical_error"),
		slog.String("trace_id", traceID),
		slog.String("flow", string(failure.Flow)),
		slog.String("stage", failure.Stage),
		slog.Any("error_chain", chain),
	}
	if truncated {
		attrs = append(attrs, slog.Bool("error_chain_truncated", true))
	}
	if failure.DiscoveryRunID > 0 {
		attrs = append(attrs, slog.Int64("discovery_run_id", failure.DiscoveryRunID))
	}
	if failure.PlatformJobID != "" {
		attrs = append(attrs, slog.String("platform_job_id", failure.PlatformJobID))
	}
	if failure.AttemptNo > 0 {
		attrs = append(attrs, slog.Int64("attempt_no", failure.AttemptNo))
	}
	record.AddAttrs(attrs...)

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.health.Healthy {
		return fmt.Errorf("%w: %s", ErrUnavailable, l.health.Message)
	}
	return l.handleLocked(ctx, record)
}

func validateTechnicalError(failure TechnicalError) error {
	switch failure.Flow {
	case FlowOnlineResume, FlowDiscovery, FlowAssessment, FlowOutreach:
	default:
		return fmt.Errorf("technical error requires a supported flow")
	}
	if failure.Stage == "" || failure.Err == nil {
		return fmt.Errorf("technical error requires stage and error")
	}
	if failure.DiscoveryRunID < 0 || failure.AttemptNo < 0 {
		return fmt.Errorf("technical error identifiers cannot be negative")
	}
	if failure.TraceID != "" && !validTechnicalTraceID(failure.TraceID) {
		return fmt.Errorf("technical error trace ID must be non-zero 32 lowercase hex characters")
	}
	return nil
}

func validTechnicalTraceID(value string) bool {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != value {
		return false
	}
	for _, part := range decoded {
		if part != 0 {
			return true
		}
	}
	return false
}
