package onlineresume

import (
	"testing"
	"time"

	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestGetCurrentReturnsTheSavedOnlineResumeVersion(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	versions := New(db)
	current, err := versions.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current online resume before save: %v", err)
	}
	if current != nil {
		t.Fatalf("current online resume = %#v, want none", current)
	}

	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO online_resume_versions (
			version_no, resume_json, resume_hash, is_current, created_at
		) VALUES (1, '{"jobIntentions":[]}', 'resume-hash', 1, 1788139800000)
	`); err != nil {
		t.Fatalf("save current online resume fixture: %v", err)
	}

	current, err = versions.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current online resume: %v", err)
	}
	if current == nil {
		t.Fatal("current online resume is nil")
	}
	if current.Version != 1 {
		t.Errorf("current online resume version = %d, want 1", current.Version)
	}
	wantCreatedAt := time.UnixMilli(1788139800000)
	if !current.CreatedAt.Equal(wantCreatedAt) {
		t.Errorf("current online resume created at = %s, want %s", current.CreatedAt, wantCreatedAt)
	}
}
