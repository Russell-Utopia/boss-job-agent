package onlineresume

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type controlledOnlineResume struct {
	content ResumeContent
	err     error
	calls   int
}

func (r *controlledOnlineResume) Read(context.Context) (ResumeContent, error) {
	r.calls++
	return r.content, r.err
}

func completeResume() ResumeContent {
	return ResumeContent{
		JobIntentions: []JobIntention{{
			Role:           "Go 后端工程师",
			City:           "福州",
			Salary:         "20-30K",
			EmploymentType: "全职",
		}},
		WorkExperiences:    []string{"某公司｜后端工程师｜负责 Go 服务"},
		ProjectExperiences: []string{"招聘助手｜负责状态机与 SQLite"},
		Educations:         []string{"某大学｜计算机科学｜本科"},
		Skills:             []string{"Go", "SQLite"},
	}
}

func openVersionsTest(t *testing.T, reader OnlineResume, now time.Time) *Versions {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	return New(db, reader, logs, func() time.Time { return now })
}

func TestRefreshFromBossCreatesTheFirstImmutableOnlineResumeVersion(t *testing.T) {
	t.Parallel()

	reader := &controlledOnlineResume{content: completeResume()}
	createdAt := time.Date(2026, time.August, 31, 10, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	versions := openVersionsTest(t, reader, createdAt)

	result, err := versions.RefreshFromBoss(t.Context())
	if err != nil {
		t.Fatalf("refresh online resume: %v", err)
	}
	if result.Status != RefreshCreated {
		t.Errorf("refresh status = %q, want %q", result.Status, RefreshCreated)
	}
	if result.Current.Version != 1 {
		t.Errorf("created version = %d, want 1", result.Current.Version)
	}
	if !result.Current.CreatedAt.Equal(createdAt) {
		t.Errorf("created at = %s, want %s", result.Current.CreatedAt, createdAt)
	}
	if reader.calls != 1 {
		t.Errorf("online resume reads = %d, want 1", reader.calls)
	}

	current, err := versions.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current online resume: %v", err)
	}
	if current == nil {
		t.Fatal("current online resume is nil")
	}
	if !reflect.DeepEqual(current.Content, completeResume()) {
		t.Errorf("current content = %#v, want %#v", current.Content, completeResume())
	}
}

func TestRefreshFromBossDoesNotCreateAVersionForEquivalentNormalizedContent(t *testing.T) {
	t.Parallel()

	reader := &controlledOnlineResume{content: completeResume()}
	versions := openVersionsTest(t, reader, time.UnixMilli(1000))
	first, err := versions.RefreshFromBoss(t.Context())
	if err != nil {
		t.Fatalf("create first online resume version: %v", err)
	}

	equivalent := completeResume()
	equivalent.JobIntentions[0].Role = "  Go 后端工程师  "
	equivalent.WorkExperiences[0] = "\r\n某公司｜后端工程师｜负责 Go 服务\r\n"
	equivalent.Skills[0] = " Go "
	reader.content = equivalent
	second, err := versions.RefreshFromBoss(t.Context())
	if err != nil {
		t.Fatalf("refresh equivalent online resume: %v", err)
	}

	if first.Status != RefreshCreated {
		t.Fatalf("first refresh status = %q, want %q", first.Status, RefreshCreated)
	}
	if second.Status != RefreshUnchanged {
		t.Errorf("second refresh status = %q, want %q", second.Status, RefreshUnchanged)
	}
	if second.Current.Version != 1 {
		t.Errorf("current version = %d, want 1", second.Current.Version)
	}
	if !reflect.DeepEqual(second.Current.Content, completeResume()) {
		t.Errorf("current content = %#v, want normalized %#v", second.Current.Content, completeResume())
	}
	if reader.calls != 2 {
		t.Errorf("online resume reads = %d, want 2", reader.calls)
	}
}

func TestRefreshFromBossRejectsPartialContentAndKeepsTheLastReliableVersion(t *testing.T) {
	t.Parallel()

	reader := &controlledOnlineResume{content: completeResume()}
	versions := openVersionsTest(t, reader, time.UnixMilli(1000))
	if _, err := versions.RefreshFromBoss(t.Context()); err != nil {
		t.Fatalf("create reliable online resume version: %v", err)
	}

	partial := completeResume()
	partial.ProjectExperiences = nil
	reader.content = partial
	_, err := versions.RefreshFromBoss(t.Context())
	if err == nil {
		t.Fatal("refresh partial online resume error is nil")
	}

	current, err := versions.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current online resume after partial read: %v", err)
	}
	if current == nil {
		t.Fatal("current online resume is nil after partial read")
	}
	if current.Version != 1 {
		t.Errorf("current version = %d, want 1", current.Version)
	}
	if !reflect.DeepEqual(current.Content, completeResume()) {
		t.Errorf("current content = %#v, want reliable %#v", current.Content, completeResume())
	}
}

func TestRefreshFromBossReportsAReadableFailureAndKeepsTheLastReliableVersion(t *testing.T) {
	t.Parallel()

	reader := &controlledOnlineResume{content: completeResume()}
	versions := openVersionsTest(t, reader, time.UnixMilli(1000))
	if _, err := versions.RefreshFromBoss(t.Context()); err != nil {
		t.Fatalf("create reliable online resume version: %v", err)
	}

	reader.err = errors.New("browser session disconnected: cookie=secret")
	_, err := versions.RefreshFromBoss(t.Context())
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("refresh error = %v, want business rejection", err)
	}
	if rejection.RejectionCode() != "online_resume_read_failed" {
		t.Errorf("rejection code = %q, want online_resume_read_failed", rejection.RejectionCode())
	}
	if rejection.RejectionReason() != "读取 BOSS 在线简历失败，已保留上一次可靠版本" {
		t.Errorf("rejection reason = %q", rejection.RejectionReason())
	}
	if strings.Contains(rejection.RejectionReason(), "cookie") {
		t.Errorf("rejection reason exposes the underlying error: %q", rejection.RejectionReason())
	}

	current, err := versions.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current online resume after failed read: %v", err)
	}
	if current == nil || current.Version != 1 {
		t.Fatalf("current online resume = %#v, want v1", current)
	}
}

func TestRefreshFromBossDoesNotReadBossWhenTheRunlogGateIsClosed(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reader := &controlledOnlineResume{content: completeResume()}
	logs := runlog.Open("relative-log-path.jsonl")
	t.Cleanup(func() { _ = logs.Close() })
	versions := New(db, reader, logs, time.Now)

	_, err = versions.RefreshFromBoss(t.Context())
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("refresh error = %v, want business rejection", err)
	}
	if rejection.RejectionCode() != "runlog_unavailable" {
		t.Errorf("rejection code = %q, want runlog_unavailable", rejection.RejectionCode())
	}
	if rejection.RejectionReason() != "运行日志不可用，恢复前不会访问 BOSS" {
		t.Errorf("rejection reason = %q", rejection.RejectionReason())
	}
	if reader.calls != 0 {
		t.Errorf("online resume reads = %d, want 0", reader.calls)
	}
	current, err := versions.GetCurrent(t.Context())
	if err != nil {
		t.Fatalf("get current online resume: %v", err)
	}
	if current != nil {
		t.Errorf("current online resume = %#v, want none", current)
	}
}

func TestRefreshFromBossPreservesTheProductionAdaptersReadableFailure(t *testing.T) {
	t.Parallel()

	readFailure := &ReadError{
		Category:   ReadErrorAuthenticationExpired,
		UserReason: "BOSS 登录已失效，请在 Chrome 重新登录后再刷新",
		Cause:      errors.New("browser cookie expired"),
	}
	reader := &controlledOnlineResume{err: readFailure}
	versions := openVersionsTest(t, reader, time.UnixMilli(1000))

	_, err := versions.RefreshFromBoss(t.Context())
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("refresh error = %v, want business rejection", err)
	}
	if rejection.RejectionReason() != readFailure.UserReason {
		t.Errorf("rejection reason = %q, want %q", rejection.RejectionReason(), readFailure.UserReason)
	}
}

func TestGetCurrentReturnsTheSavedOnlineResumeVersion(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reader := &controlledOnlineResume{err: errors.New("not used")}
	logs := runlog.Open(filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	versions := New(db, reader, logs, time.Now)
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
