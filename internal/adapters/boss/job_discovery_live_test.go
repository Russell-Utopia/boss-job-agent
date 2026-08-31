//go:build live

package boss

import (
	"errors"
	"os"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
)

func TestJobDiscoveryLiveReadsOneAuthenticatedBossPage(t *testing.T) {
	if os.Getenv("BOSS_JOB_DISCOVERY_LIVE") != "1" {
		t.Skip("set BOSS_JOB_DISCOVERY_LIVE=1 to read one authenticated BOSS search page")
	}
	searchRange := discovery.SearchRange{
		Role:           os.Getenv("BOSS_JOB_DISCOVERY_ROLE"),
		City:           os.Getenv("BOSS_JOB_DISCOVERY_CITY"),
		Salary:         os.Getenv("BOSS_JOB_DISCOVERY_SALARY"),
		EmploymentType: os.Getenv("BOSS_JOB_DISCOVERY_EMPLOYMENT_TYPE"),
	}
	if err := validateSearchInput(searchRange, 1); err != nil {
		t.Fatalf("live search input: %v", err)
	}
	page, err := NewDefaultJobDiscovery().FetchPage(t.Context(), searchRange, 1)
	if err != nil {
		var fetchErr *discovery.FetchError
		if !errors.As(err, &fetchErr) {
			t.Errorf("live failure has no stable discovery.FetchError classification: %v", err)
		}
		t.Fatalf("fetch authenticated BOSS job page: %v", err)
	}
	if err := discovery.ValidatePage(page); err != nil {
		t.Fatalf("live page contract: %v", err)
	}
	t.Logf("BOSS discovery page contract verified: jobs=%d has_more=%t", len(page.Observations), page.HasMore)
}
