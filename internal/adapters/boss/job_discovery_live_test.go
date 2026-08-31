//go:build live

package boss

import (
	"errors"
	"os"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery"
)

func TestJobDiscoveryLiveReadsAuthenticatedBossPages(t *testing.T) {
	if os.Getenv("BOSS_JOB_DISCOVERY_LIVE") != "1" {
		t.Skip("set BOSS_JOB_DISCOVERY_LIVE=1 to read authenticated BOSS search pages")
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
	adapter := NewDefaultJobDiscovery()
	seen := make(map[string]struct{})
	for pageNo := 1; pageNo <= 3; pageNo++ {
		page, err := adapter.FetchPage(t.Context(), searchRange, pageNo)
		if err != nil {
			var fetchErr *discovery.FetchError
			if !errors.As(err, &fetchErr) {
				t.Errorf("live failure has no stable discovery.FetchError classification: %v", err)
			}
			t.Fatalf("fetch authenticated BOSS job page %d: %v", pageNo, err)
		}
		if err := discovery.ValidatePage(page); err != nil {
			t.Fatalf("live page %d contract: %v", pageNo, err)
		}
		newJobs := 0
		for _, observation := range page.Observations {
			if _, exists := seen[observation.PlatformJobID]; !exists {
				seen[observation.PlatformJobID] = struct{}{}
				newJobs++
			}
		}
		t.Logf(
			"BOSS discovery page %d verified: jobs=%d new_stable_ids=%d total_stable_ids=%d has_more=%t samples=%v",
			pageNo, len(page.Observations), newJobs, len(seen), page.HasMore, liveJobSamples(page),
		)
		if !page.HasMore {
			return
		}
	}
}

func liveJobSamples(page discovery.DiscoveryPage) []string {
	limit := min(3, len(page.Observations))
	samples := make([]string, 0, limit)
	for _, observation := range page.Observations[:limit] {
		samples = append(samples, observation.JobTitle+" / "+observation.CompanyName+" / "+observation.Salary)
	}
	return samples
}
