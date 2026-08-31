package discovery

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type resumeReader struct {
	content onlineresume.ResumeContent
}

func (r *resumeReader) Read(context.Context) (onlineresume.ResumeContent, error) {
	return r.content, nil
}

func discoveryResume(role string) onlineresume.ResumeContent {
	return onlineresume.ResumeContent{
		JobIntentions: []onlineresume.JobIntention{{
			Role: role, City: "福州", Salary: "20-30K", EmploymentType: "全职",
		}},
		WorkExperiences:    []string{"后端工程师"},
		ProjectExperiences: []string{},
		Educations:         []string{"计算机本科"},
		Skills:             []string{"Go"},
	}
}

func TestStartDependsOnlyOnTheCurrentSavedOnlineResume(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logs := runlog.Open(filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	reader := &resumeReader{content: discoveryResume("Go 后端工程师")}
	resumeVersions := onlineresume.New(db, reader, logs, time.Now)
	service := New(db, resumeVersions)
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

func TestActiveDiscoveryKeepsV1WhenTheCurrentOnlineResumeBecomesV2(t *testing.T) {
	t.Parallel()

	db, reader, versions := openDiscoveryResumeTest(t)
	first := refreshDiscoveryResume(t, versions)
	seedActiveDiscovery(t, db, first.Current.ID)

	reader.content = discoveryResume("Go 研发工程师")
	second := refreshDiscoveryResume(t, versions)
	if second.Current.Version != 2 {
		t.Fatalf("current online resume version = %d, want 2", second.Current.Version)
	}
	assertDiscoveryResumeIsolation(t, New(db, versions), versions)
}

func openDiscoveryResumeTest(t *testing.T) (*sql.DB, *resumeReader, *onlineresume.Versions) {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	reader := &resumeReader{content: discoveryResume("Go 后端工程师")}
	versions := onlineresume.New(db, reader, logs, time.Now)
	return db, reader, versions
}

func refreshDiscoveryResume(t *testing.T, versions *onlineresume.Versions) onlineresume.RefreshResult {
	t.Helper()
	result, err := versions.RefreshFromBoss(t.Context())
	if err != nil {
		t.Fatalf("refresh online resume: %v", err)
	}
	return result
}

func seedActiveDiscovery(t *testing.T, db *sql.DB, resumeVersionID int64) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO discovery_runs (
			resume_version_id, current_role, current_city, next_page,
			status, attempt_no, created_at, prepared_at, updated_at
		) VALUES (?, 'Go 后端工程师', '福州', 1, 'paused', 1, 1000, 1000, 1000)
	`, resumeVersionID); err != nil {
		t.Fatalf("seed active discovery using v1: %v", err)
	}
}

func assertDiscoveryResumeIsolation(t *testing.T, service *Service, versions *onlineresume.Versions) {
	t.Helper()
	active, err := service.GetActiveResumeUse(t.Context())
	if err != nil {
		t.Fatalf("get active discovery resume use: %v", err)
	}
	if active == nil {
		t.Fatal("active discovery resume use is nil")
	}
	if active.ResumeVersion != 1 {
		t.Errorf("active discovery resume version = %d, want 1", active.ResumeVersion)
	}
	current, err := versions.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current online resume: %v", err)
	}
	if current == nil || current.Version != 2 {
		t.Errorf("current online resume = %#v, want v2", current)
	}
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
