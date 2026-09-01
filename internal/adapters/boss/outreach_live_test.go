//go:build live

package boss

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/outreach"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

// This entry is explicitly gated: an actual target URL, ID, and greeting are
// always required, and a real send additionally requires BOSS_OUTREACH_SEND_LIVE=1.
func TestOutreachLiveUsesTheAuthenticatedBossSession(t *testing.T) {
	if testing.Short() || getenv("BOSS_OUTREACH_LIVE") != "1" {
		t.Skip("set BOSS_OUTREACH_LIVE=1 to run the explicit BOSS outreach probe")
	}

	jobID := getenv("BOSS_OUTREACH_PLATFORM_JOB_ID")
	canonicalURL := getenv("BOSS_OUTREACH_CANONICAL_URL")
	greeting := getenv("BOSS_OUTREACH_GREETING")
	if jobID == "" || canonicalURL == "" || greeting == "" {
		t.Skip("set BOSS_OUTREACH_PLATFORM_JOB_ID, BOSS_OUTREACH_CANONICAL_URL, and BOSS_OUTREACH_GREETING")
	}

	adapter := NewDefaultOutreach()
	logs := runlog.Open(filepath.Join(t.TempDir(), "outreach-live.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	ref := outreach.PlatformJobRef{PlatformJobID: jobID, CanonicalURL: canonicalURL}
	checkAttempt := runlog.Attempt{
		Flow: runlog.FlowOutreach, Operation: runlog.OperationCheckContactStatus,
		PlatformJobID: jobID, AttemptNo: 1,
	}
	checkTrace, err := logs.Start(t.Context(), checkAttempt)
	if err != nil {
		t.Fatal(err)
	}
	status, err := adapter.Check(t.Context(), ref)
	if err != nil {
		_ = logs.Finish(t.Context(), checkTrace, runlog.AttemptResult{Outcome: runlog.OutcomeFailed, Err: err})
		t.Fatal(err)
	}
	if err := logs.Finish(t.Context(), checkTrace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
	t.Logf("live BOSS contact check: job_id=%s evidence=%s trace_id=%s", jobID, status.Evidence, checkTrace.ID())
	if status.AlreadyContacted || !status.Open {
		t.Skip("the supplied BOSS job is already contacted or no longer open")
	}
	if getenv("BOSS_OUTREACH_SEND_LIVE") != "1" {
		t.Skip("set BOSS_OUTREACH_SEND_LIVE=1 to authorize the real first-contact action")
	}

	sendAttempt := runlog.Attempt{
		Flow: runlog.FlowOutreach, Operation: runlog.OperationSendFirstContact,
		PlatformJobID: jobID, AttemptNo: 1,
	}
	sendTrace, err := logs.StartLinked(t.Context(), checkTrace.ID(), sendAttempt)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Send(t.Context(), outreach.FirstContactRequest{
		PlatformJobID: jobID, CanonicalURL: canonicalURL, GreetingText: greeting,
	})
	if err != nil {
		_ = logs.Finish(t.Context(), sendTrace, runlog.AttemptResult{
			Outcome: runlog.OutcomeFailed, Err: err, OutreachEffect: runlog.OutreachEffectPossiblyEffective,
		})
		t.Fatal(err)
	}
	effect := runlog.OutreachEffect(result.Effect)
	if err := logs.Finish(t.Context(), sendTrace, runlog.AttemptResult{
		Outcome: runlog.OutcomeSucceeded, OutreachEffect: effect,
	}); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(result.Evidence) {
		t.Fatal("live BOSS first-contact result has invalid evidence")
	}
	t.Logf("live BOSS first contact: job_id=%s greeting=%q effect=%s evidence=%s trace_id=%s", jobID, greeting, result.Effect, result.Evidence, sendTrace.ID())
	if result.Effect != outreach.OutreachEffectConfirmedSent && result.Effect != outreach.OutreachEffectConfirmedNoEffect {
		t.Fatalf("live outreach effect = %q, want confirmed result", result.Effect)
	}
	if result.Effect == outreach.OutreachEffectConfirmedSent {
		followUpAttempt := runlog.Attempt{
			Flow: runlog.FlowOutreach, Operation: runlog.OperationCheckContactStatus,
			PlatformJobID: jobID, AttemptNo: 2,
		}
		followUpTrace, err := logs.StartLinked(t.Context(), checkTrace.ID(), followUpAttempt)
		if err != nil {
			t.Fatal(err)
		}
		status, err := adapter.Check(t.Context(), ref)
		if err != nil {
			_ = logs.Finish(t.Context(), followUpTrace, runlog.AttemptResult{Outcome: runlog.OutcomeFailed, Err: err})
			t.Fatal(err)
		}
		if err := logs.Finish(t.Context(), followUpTrace, runlog.AttemptResult{Outcome: runlog.OutcomeSucceeded}); err != nil {
			t.Fatal(err)
		}
		if !status.AlreadyContacted {
			t.Fatal("BOSS did not report the sent job as already contacted on the follow-up check")
		}
		t.Logf("live BOSS follow-up contact check: job_id=%s evidence=%s trace_id=%s", jobID, status.Evidence, followUpTrace.ID())
	}
}

func getenv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
