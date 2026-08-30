package jobpool

import (
	"errors"
	"testing"
)

func TestQueueAuthorizedOutreachRejectsWhenNoJobIsAvailable(t *testing.T) {
	t.Parallel()

	pool := New()
	err := pool.QueueAuthorizedOutreach(t.Context(), nil, OutreachAuthorization{})
	var rejection *Rejection
	ok := errors.As(err, &rejection)
	if !ok {
		t.Fatalf("queue authorized outreach error = %v, want business rejection", err)
	}
	if rejection.Code != "outreach_unavailable" {
		t.Errorf("rejection code = %q, want outreach_unavailable", rejection.Code)
	}
	if rejection.Reason != "当前没有可真实打招呼的岗位" {
		t.Errorf("rejection reason = %q, want current availability reason", rejection.Reason)
	}
}
