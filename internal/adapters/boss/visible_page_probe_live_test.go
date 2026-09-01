//go:build live

package boss

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
)

func TestVisiblePageProbeLiveReadsAuthenticatedBossDOM(t *testing.T) {
	if os.Getenv("BOSS_VISIBLE_PAGE_PROBE_LIVE") != "1" {
		t.Skip("set BOSS_VISIBLE_PAGE_PROBE_LIVE=1 to read one authenticated BOSS page through visible DOM")
	}
	targetURL := os.Getenv("BOSS_VISIBLE_PAGE_URL")
	if targetURL == "" {
		t.Fatal("BOSS_VISIBLE_PAGE_URL must identify the explicitly authorized visible BOSS search page")
	}

	result, err := newVisiblePageProbe(defaultWebBridgeEndpoint, http.DefaultClient).read(
		t.Context(), targetURL, visiblePageProbeMaxJobs,
	)
	if err != nil {
		var fetchErr *discovery.FetchError
		if !errors.As(err, &fetchErr) {
			t.Errorf("visible DOM live failure has no stable discovery.FetchError classification: %v", err)
		}
		t.Fatalf("read authenticated BOSS visible job page once: %v", err)
	}
	if result.ExhaustionEvidence != visiblePageExhaustionUnavailable {
		t.Fatalf("visible DOM probe inferred exhaustion: %q", result.ExhaustionEvidence)
	}
	t.Logf(
		"BOSS visible DOM probe verified: jobs=%d scanned_cards=%d truncated=%t exhaustion_evidence=%s samples=%v",
		len(result.Jobs), result.ScannedCardCount, result.Truncated,
		result.ExhaustionEvidence, liveVisiblePageSamples(result),
	)
}

func liveVisiblePageSamples(result visiblePageProbeResult) []string {
	limit := min(3, len(result.Jobs))
	samples := make([]string, 0, limit)
	for _, job := range result.Jobs[:limit] {
		samples = append(samples, job.JobTitle+" / "+job.CompanyName+" / "+job.Salary+" / "+string(job.JDStructure))
	}
	return samples
}
