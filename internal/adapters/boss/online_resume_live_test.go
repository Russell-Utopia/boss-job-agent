//go:build live

package boss

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	"github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestOnlineResumeLiveReadsTheAuthenticatedBossResume(t *testing.T) {
	if os.Getenv("BOSS_ONLINE_RESUME_LIVE") != "1" {
		t.Skip("set BOSS_ONLINE_RESUME_LIVE=1 to read the authenticated BOSS online resume")
	}

	db, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open live test SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logPath := filepath.Join(t.TempDir(), "boss-job-agent.jsonl")
	logs := runlog.Open(logPath)
	t.Cleanup(func() { _ = logs.Close() })
	versions := onlineresume.New(db, NewDefaultOnlineResume(), logs, time.Now)

	result, err := versions.RefreshFromBoss(t.Context())
	if err != nil {
		assertLiveReadErrorClassification(t, err)
		t.Fatalf("refresh authenticated BOSS online resume: %v", err)
	}
	content := result.Current.Content
	if len(content.JobIntentions) == 0 {
		t.Fatal("BOSS online resume returned no job intentions")
	}
	if content.WorkExperiences == nil || content.ProjectExperiences == nil || content.Educations == nil || content.Skills == nil {
		t.Fatal("BOSS online resume omitted a required section")
	}
	assertLiveTrace(t, logPath)
	t.Logf(
		"BOSS online resume contract and trace verified: intentions=%d work=%d projects=%d educations=%d skills=%d",
		len(content.JobIntentions),
		len(content.WorkExperiences),
		len(content.ProjectExperiences),
		len(content.Educations),
		len(content.Skills),
	)
}

func assertLiveReadErrorClassification(t *testing.T, err error) {
	t.Helper()
	var readErr *onlineresume.ReadError
	if !errors.As(err, &readErr) {
		t.Errorf("live failure has no stable onlineresume.ReadError classification: %v", err)
		return
	}
	if readErr.Category == "" || readErr.Category == onlineresume.ReadErrorUnknown {
		t.Errorf("live failure classification = %q, want a stable category", readErr.Category)
	}
}

func assertLiveTrace(t *testing.T, logPath string) {
	t.Helper()
	report, err := runlog.Find(t.Context(), logPath, runlog.Query{
		Flow:      runlog.FlowOnlineResume,
		Operation: runlog.OperationReadOnlineResume,
		AttemptNo: 1,
	})
	if err != nil {
		t.Fatalf("find live online-resume trace: %v", err)
	}
	if len(report.Events) != 2 {
		t.Fatalf("live trace events = %d, want start and terminal events", len(report.Events))
	}
	type traceEvent struct {
		Event   string `json:"event"`
		TraceID string `json:"trace_id"`
	}
	events := make([]traceEvent, len(report.Events))
	for index, raw := range report.Events {
		if err := json.Unmarshal(raw, &events[index]); err != nil {
			t.Fatalf("decode live trace event %d: %v", index, err)
		}
	}
	if events[0].Event != "external_attempt_started" || events[1].Event != "external_attempt_finished" {
		t.Errorf("live trace events = %#v", events)
	}
	if events[0].TraceID == "" || events[0].TraceID != events[1].TraceID {
		t.Errorf("live trace IDs = %q and %q", events[0].TraceID, events[1].TraceID)
	}
}
