package jobpool

import (
	"testing"
	"time"

	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestListEffectiveHumanReviewsReturnsOnlyCurrentJDLabels(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pool := New(db)

	current := mustObserve(t, pool, 1, observedJob("policy-sample-current"))
	if err := pool.Review(t.Context(), []ReviewDecision{{
		JobID: current.ID, ExpectedJDHash: current.JDHash, Verdict: HumanVerdictSuitable,
		Note: "当前 JD 的有效样本",
	}}); err != nil {
		t.Fatalf("save current human review: %v", err)
	}
	stale := mustObserve(t, pool, 1, observedJob("policy-sample-stale"))
	if err := pool.Review(t.Context(), []ReviewDecision{{
		JobID: stale.ID, ExpectedJDHash: stale.JDHash, Verdict: HumanVerdictUnsuitable,
	}}); err != nil {
		t.Fatalf("save stale fixture review: %v", err)
	}
	changed := observedJob("policy-sample-stale")
	changed.Responsibilities = "新的职责已经改变"
	changed.ObservedAt = time.UnixMilli(3_000)
	if _, err := pool.Observe(t.Context(), 1, changed); err != nil {
		t.Fatalf("change reviewed JD: %v", err)
	}

	samples, err := pool.ListEffectiveHumanReviews(t.Context())
	if err != nil {
		t.Fatalf("list effective human reviews: %v", err)
	}
	if len(samples) != 1 || samples[0].JobID != current.ID {
		t.Fatalf("effective samples = %#v, want only current job %d", samples, current.ID)
	}
	if samples[0].Verdict != HumanVerdictSuitable || samples[0].Responsibilities != current.Responsibilities {
		t.Errorf("effective sample = %#v, want current JD and human verdict", samples[0])
	}
}
