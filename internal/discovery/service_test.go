package discovery

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

type resumeReader struct {
	content onlineresume.ResumeContent
}

type fetchCall struct {
	SearchRange SearchRange
	Page        int
}

type controlledJobDiscovery struct {
	pages  map[int]DiscoveryPage
	errors map[int]error
	calls  []fetchCall
}

func (d *controlledJobDiscovery) FetchPage(
	_ context.Context,
	searchRange SearchRange,
	page int,
) (DiscoveryPage, error) {
	d.calls = append(d.calls, fetchCall{SearchRange: searchRange, Page: page})
	if err := d.errors[page]; err != nil {
		return DiscoveryPage{}, err
	}
	return d.pages[page], nil
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

	_, _, resumeVersions, _, service, _ := openDiscoveryServiceTest(t, &controlledJobDiscovery{})
	availability, err := service.StartAvailability(t.Context())
	if err != nil {
		t.Fatalf("get start availability: %v", err)
	}
	assertUnavailable(t, availability, "online_resume_required", "请先刷新在线简历，再开始岗位发现")
	_, startErr := service.Start(t.Context())
	assertRejectionCode(t, startErr, "online_resume_required")

	refreshDiscoveryResume(t, resumeVersions)
	availability, err = service.StartAvailability(t.Context())
	if err != nil {
		t.Fatalf("get start availability with current resume: %v", err)
	}
	if !availability.Allowed {
		t.Errorf("start availability = %#v, want allowed", availability)
	}
}

func TestStartCompletesOneSearchRangeWithAllSavedInputsAndGlobalIDDeduplication(t *testing.T) {
	t.Parallel()

	discoveryAdapter := &controlledJobDiscovery{pages: map[int]DiscoveryPage{
		1: {Observations: []JobObservation{discoveredJob("boss-job-1")}, HasMore: true},
		2: {
			Observations: []JobObservation{discoveredJob("boss-job-1"), discoveredJob("boss-job-2")},
			HasMore:      false,
		},
	}}
	_, _, versions, pool, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
	refreshed := refreshDiscoveryResume(t, versions)

	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if runID <= 0 {
		t.Fatalf("discovery run ID = %d, want positive", runID)
	}
	wantRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	assertFetchCalls(t, discoveryAdapter.calls, wantRange)
	assertCompletedRun(t, service, refreshed.Current.Version, wantRange)
	assertGlobalJobIDs(t, pool, "boss-job-1", "boss-job-2")
}

func assertFetchCalls(t *testing.T, got []fetchCall, searchRange SearchRange) {
	t.Helper()
	want := []fetchCall{{SearchRange: searchRange, Page: 1}, {SearchRange: searchRange, Page: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fetch calls = %#v, want %#v", got, want)
	}
}

func assertCompletedRun(t *testing.T, service *Service, resumeVersion int, searchRange SearchRange) {
	t.Helper()
	run, err := service.GetLatestRun(t.Context())
	if err != nil {
		t.Fatalf("get latest discovery run: %v", err)
	}
	if run == nil {
		t.Fatal("latest discovery run is nil")
	}
	if run.ResumeVersion != resumeVersion || run.SearchRange != searchRange {
		t.Errorf("discovery inputs = %#v, want resume v%d and %#v", run, resumeVersion, searchRange)
	}
	if run.Status != StatusCompleted || run.CurrentPage != 2 || run.DiscoveredJobs != 2 {
		t.Errorf("discovery progress = %#v, want completed on page 2 with 2 jobs", run)
	}
}

func assertGlobalJobIDs(t *testing.T, pool *jobpool.Pool, wants ...string) {
	t.Helper()
	jobs, err := pool.ListJobs(t.Context())
	if err != nil {
		t.Fatalf("list global jobs: %v", err)
	}
	got := make([]string, len(jobs))
	for index, job := range jobs {
		got[index] = job.PlatformJobID
	}
	if !reflect.DeepEqual(got, wants) {
		t.Errorf("global job IDs = %#v, want %#v", got, wants)
	}
}

func TestInvalidObservationFailsTheWholePageWithoutAdvancingOrSavingJobs(t *testing.T) {
	t.Parallel()

	invalid := discoveredJob("boss-job-invalid")
	invalid.Requirements = ""
	discoveryAdapter := &controlledJobDiscovery{pages: map[int]DiscoveryPage{
		1: {Observations: []JobObservation{discoveredJob("boss-job-1"), invalid}, HasMore: false},
	}}
	_, _, versions, pool, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
	refreshDiscoveryResume(t, versions)

	runID, err := service.Start(t.Context())
	if err == nil {
		t.Fatal("start discovery succeeded, want unreliable page failure")
	}
	if runID <= 0 {
		t.Fatalf("failed discovery run ID = %d, want positive", runID)
	}
	run, viewErr := service.GetLatestRun(t.Context())
	if viewErr != nil {
		t.Fatalf("get failed discovery run: %v", viewErr)
	}
	if run == nil || run.Status != StatusFailed || run.CurrentPage != 1 {
		t.Errorf("failed discovery progress = %#v, want failed at page 1", run)
	}
	jobs, listErr := pool.ListJobs(t.Context())
	if listErr != nil {
		t.Fatalf("list jobs after invalid page: %v", listErr)
	}
	if len(jobs) != 0 {
		t.Errorf("jobs saved from invalid page = %d, want 0", len(jobs))
	}
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
	logs := runlog.Open(filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	pool := jobpool.New(db)
	service := New(db, versions, pool, &controlledJobDiscovery{}, logs, time.Now)
	assertDiscoveryResumeIsolation(t, service, versions)
}

func discoveredJob(platformJobID string) JobObservation {
	return JobObservation{
		PlatformJobID:    platformJobID,
		CanonicalURL:     "https://www.zhipin.com/job_detail/" + platformJobID + ".html",
		JobTitle:         "Go 后端工程师",
		CompanyName:      "示例科技",
		City:             "福州",
		Salary:           "20-30K",
		Responsibilities: "负责 Go 服务开发",
		Requirements:     "熟悉 Go 与 SQLite",
		PlatformStatus:   PlatformStatusOpen,
	}
}

func openDiscoveryServiceTest(
	t *testing.T,
	discoveryAdapter JobDiscovery,
) (*sql.DB, *resumeReader, *onlineresume.Versions, *jobpool.Pool, *Service, *runlog.Log) {
	t.Helper()
	db, reader, versions := openDiscoveryResumeTest(t)
	logs := runlog.Open(filepath.Join(t.TempDir(), "boss-job-agent.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	pool := jobpool.New(db)
	now := func() time.Time { return time.UnixMilli(2000) }
	service := New(db, versions, pool, discoveryAdapter, logs, now)
	return db, reader, versions, pool, service, logs
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
