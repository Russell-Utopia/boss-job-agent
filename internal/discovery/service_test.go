package discovery

import (
	"errors"
	"testing"

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestStartDependsOnlyOnTheCurrentSavedOnlineResume(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	service := New(onlineresume.New(db))
	availability, err := service.StartAvailability(t.Context())
	if err != nil {
		t.Fatalf("get start availability: %v", err)
	}
	assertUnavailable(t, availability, "online_resume_required", "请先刷新在线简历，再开始岗位发现")
	assertRejectionCode(t, service.Start(t.Context()), "online_resume_required")

	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (1, '{"jobIntentions":[]}', 'resume-hash', 1, 1000)
	`); err != nil {
		t.Fatalf("save current online resume fixture: %v", err)
	}

	availability, err = service.StartAvailability(t.Context())
	if err != nil {
		t.Fatalf("get start availability with current resume: %v", err)
	}
	assertUnavailable(t, availability, "discovery_unavailable", "当前版本尚未开放岗位发现")
	assertRejectionCode(t, service.Start(t.Context()), "discovery_unavailable")
}

func assertUnavailable(t *testing.T, availability ActionAvailability, code, reason string) {
	t.Helper()
	if availability.Allowed {
		t.Fatal("action is allowed, want unavailable")
	}
	if availability.Code != code {
		t.Errorf("availability code = %q, want %q", availability.Code, code)
	}
	if availability.Reason != reason {
		t.Errorf("availability reason = %q, want %q", availability.Reason, reason)
	}
}

func assertRejectionCode(t *testing.T, err error, code string) {
	t.Helper()
	var rejection *Rejection
	ok := errors.As(err, &rejection)
	if !ok {
		t.Fatalf("start error = %v, want business rejection", err)
	}
	if rejection.Code != code {
		t.Errorf("rejection code = %q, want %q", rejection.Code, code)
	}
}
