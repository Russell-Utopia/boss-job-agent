package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery/internal/sqlitedb"
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

type controlledPage struct {
	Observations []JobObservation
	HasMore      bool
}

type controlledJobDiscovery struct {
	pages      map[fetchCall]controlledPage
	errors     map[fetchCall]error
	calls      []fetchCall
	readCalls  []string
	activeJobs map[string]JobObservation
}

type blockingJobDiscovery struct {
	started chan struct{}
	release chan struct{}
	page    controlledPage
}

type checkpointJobDiscovery struct {
	page       JobPage
	jobs       map[string]JobObservation
	readErrors map[string]error
	listCalls  []fetchCall
	readCalls  []string
}

type leaseRenewalJobDiscovery struct {
	page        JobPage
	jobs        map[string]JobObservation
	now         *time.Time
	blockOnID   string
	readStarted chan struct{}
	releaseRead chan struct{}
}

func (d *checkpointJobDiscovery) ListPage(
	_ context.Context,
	searchRange SearchRange,
	page int,
) (JobPage, error) {
	d.listCalls = append(d.listCalls, fetchCall{SearchRange: searchRange, Page: page})
	return d.page, nil
}

func (d *checkpointJobDiscovery) ReadJob(
	_ context.Context,
	platformJobID string,
) (JobObservation, error) {
	d.readCalls = append(d.readCalls, platformJobID)
	if err := d.readErrors[platformJobID]; err != nil {
		return JobObservation{}, err
	}
	return d.jobs[platformJobID], nil
}

func (d *blockingJobDiscovery) ListPage(
	context.Context,
	SearchRange,
	int,
) (JobPage, error) {
	jobIDs := make([]string, len(d.page.Observations))
	for index, observation := range d.page.Observations {
		jobIDs[index] = observation.PlatformJobID
	}
	return JobPage{PlatformJobIDs: jobIDs, HasMore: d.page.HasMore}, nil
}

func (d *blockingJobDiscovery) ReadJob(ctx context.Context, platformJobID string) (JobObservation, error) {
	close(d.started)
	select {
	case <-ctx.Done():
		return JobObservation{}, ctx.Err()
	case <-d.release:
	}
	for _, observation := range d.page.Observations {
		if observation.PlatformJobID == platformJobID {
			return observation, nil
		}
	}
	return JobObservation{}, fmt.Errorf("unknown controlled job %q", platformJobID)
}

func (d *leaseRenewalJobDiscovery) ListPage(context.Context, SearchRange, int) (JobPage, error) {
	return d.page, nil
}

func (d *leaseRenewalJobDiscovery) ReadJob(
	ctx context.Context,
	platformJobID string,
) (JobObservation, error) {
	*d.now = d.now.Add(6 * time.Minute)
	if platformJobID == d.blockOnID {
		close(d.readStarted)
		select {
		case <-ctx.Done():
			return JobObservation{}, ctx.Err()
		case <-d.releaseRead:
			return JobObservation{}, &FetchError{
				Category: FetchErrorPlatformLimited,
				Cause:    errors.New("BOSS_PLATFORM_LIMITED"),
			}
		}
	}
	return d.jobs[platformJobID], nil
}

func (d *controlledJobDiscovery) ListPage(
	_ context.Context,
	searchRange SearchRange,
	page int,
) (JobPage, error) {
	call := fetchCall{SearchRange: searchRange, Page: page}
	d.calls = append(d.calls, call)
	if err := d.errors[call]; err != nil {
		return JobPage{}, err
	}
	result := d.pages[call]
	d.activeJobs = make(map[string]JobObservation, len(result.Observations))
	jobIDs := make([]string, len(result.Observations))
	for index, observation := range result.Observations {
		jobIDs[index] = observation.PlatformJobID
		d.activeJobs[observation.PlatformJobID] = observation
	}
	return JobPage{PlatformJobIDs: jobIDs, HasMore: result.HasMore}, nil
}

func (d *controlledJobDiscovery) ReadJob(
	_ context.Context,
	platformJobID string,
) (JobObservation, error) {
	d.readCalls = append(d.readCalls, platformJobID)
	observation, ok := d.activeJobs[platformJobID]
	if !ok {
		return JobObservation{}, fmt.Errorf("unknown controlled job %q", platformJobID)
	}
	return observation, nil
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

func TestDiscoveryPersistsEachReliableJobAndResumesFromTheFailedJob(t *testing.T) {
	t.Parallel()

	discoveryAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{"boss-job-1", "boss-job-2", "boss-job-3"}, HasMore: false},
		jobs: map[string]JobObservation{
			"boss-job-1": discoveredJob("boss-job-1"),
		},
		readErrors: map[string]error{
			"boss-job-2": &FetchError{
				Category: FetchErrorPlatformLimited,
				Cause:    errors.New("BOSS_PLATFORM_LIMITED"),
			},
		},
	}
	db, _, versions, pool, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
	refreshDiscoveryResume(t, versions)
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}

	err = service.runSchedulingCycle(t.Context(), time.UnixMilli(2000))
	if err == nil {
		t.Fatal("discovery cycle succeeded, want second job read to stop the run")
	}
	assertGlobalJobIDs(t, pool, "boss-job-1")
	assertRunCheckpoint(t, service, runID, StatusFailed, 1, 1)
	assertPersistedPageCheckpoint(t, db, runID, `["boss-job-1","boss-job-2","boss-job-3"]`, false, 1)
	if got, want := discoveryAdapter.readCalls, []string{"boss-job-1", "boss-job-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("read job calls = %#v, want %#v", got, want)
	}

	delete(discoveryAdapter.readErrors, "boss-job-2")
	discoveryAdapter.jobs["boss-job-2"] = discoveredJob("boss-job-2")
	discoveryAdapter.jobs["boss-job-3"] = discoveredJob("boss-job-3")
	if err := service.Continue(t.Context(), runID); err != nil {
		t.Fatalf("continue discovery: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("resume discovery: %v", err)
	}
	if got, want := discoveryAdapter.readCalls, []string{
		"boss-job-1", "boss-job-2", "boss-job-2", "boss-job-3",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("all read job calls = %#v, want failed job reread without earlier replay %#v", got, want)
	}
	assertGlobalJobIDs(t, pool, "boss-job-1", "boss-job-2", "boss-job-3")
}

func TestDiscoveryRestartRereadsAtMostTheJobObservedBeforeCheckpointAdvance(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	databasePath := filepath.Join(directory, "boss-job-agent.db")
	logPath := filepath.Join(directory, "boss-job-agent.jsonl")
	runID, reader := runUntilCheckpointAdvanceFailure(t, databasePath, logPath)

	restartedDB, err := storage.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("reopen discovery database: %v", err)
	}
	t.Cleanup(func() { _ = restartedDB.Close() })
	restartedLogs := runlog.Open(logPath)
	t.Cleanup(func() { _ = restartedLogs.Close() })
	restartedAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{"boss-job-1", "boss-job-2"}, HasMore: false},
		jobs: map[string]JobObservation{
			"boss-job-1": discoveredJob("boss-job-1"),
			"boss-job-2": discoveredJob("boss-job-2"),
		},
	}
	restartedService := New(
		restartedDB,
		onlineresume.New(restartedDB, reader, restartedLogs, func() time.Time { return time.UnixMilli(3000) }),
		jobpool.New(restartedDB),
		restartedAdapter,
		restartedLogs,
		func() time.Time { return time.UnixMilli(3000) },
	)
	if err := restartedService.Continue(t.Context(), runID); err != nil {
		t.Fatalf("continue restarted discovery: %v", err)
	}
	if err := restartedService.runSchedulingCycle(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("complete restarted discovery: %v", err)
	}
	if got, want := restartedAdapter.readCalls, []string{"boss-job-1", "boss-job-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("restarted read calls = %#v, want only current and later jobs %#v", got, want)
	}
	assertGlobalJobIDs(t, jobpool.New(restartedDB), "boss-job-1", "boss-job-2")
	assertCompletedRun(
		t,
		restartedService,
		1,
		SearchRange{Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职"},
		1,
		2,
	)
}

func TestDiscoveryRestartRereadsAJobInterruptedBeforeObserve(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	databasePath := filepath.Join(directory, "boss-job-agent.db")
	logPath := filepath.Join(directory, "boss-job-agent.jsonl")
	db := mustOpenDiscoveryDatabase(t, databasePath)
	logs := runlog.Open(logPath)
	reader := &resumeReader{content: discoveryResume("Go 后端工程师")}
	versions := onlineresume.New(db, reader, logs, func() time.Time { return time.UnixMilli(1000) })
	refreshDiscoveryResume(t, versions)
	blocked := &blockingJobDiscovery{
		started: make(chan struct{}),
		release: make(chan struct{}),
		page: controlledPage{
			Observations: []JobObservation{discoveredJob("boss-job-1")}, HasMore: false,
		},
	}
	service := New(db, versions, jobpool.New(db), blocked, logs, func() time.Time {
		return time.UnixMilli(2000)
	})
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	cycleResult := make(chan error, 1)
	go func() {
		cycleResult <- service.runSchedulingCycle(t.Context(), time.UnixMilli(2000))
	}()
	<-blocked.started
	assertPersistedPageCheckpoint(t, db, runID, `["boss-job-1"]`, false, 0)
	if err := db.Close(); err != nil {
		t.Fatalf("close database before observing the job: %v", err)
	}
	close(blocked.release)
	if err := <-cycleResult; err == nil {
		t.Fatal("interrupted discovery cycle succeeded after its database closed")
	}
	if err := logs.Close(); err != nil {
		t.Fatalf("close interrupted runlog: %v", err)
	}

	restartedDB := mustOpenDiscoveryDatabase(t, databasePath)
	t.Cleanup(func() { _ = restartedDB.Close() })
	restartedLogs := runlog.Open(logPath)
	t.Cleanup(func() { _ = restartedLogs.Close() })
	restartedAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{"boss-job-1"}, HasMore: false},
		jobs: map[string]JobObservation{"boss-job-1": discoveredJob("boss-job-1")},
	}
	restartedService := New(
		restartedDB,
		onlineresume.New(restartedDB, reader, restartedLogs, func() time.Time { return time.UnixMilli(700_000) }),
		jobpool.New(restartedDB),
		restartedAdapter,
		restartedLogs,
		func() time.Time { return time.UnixMilli(700_000) },
	)
	if err := restartedService.runSchedulingCycle(t.Context(), time.UnixMilli(700_000)); err != nil {
		t.Fatalf("expire interrupted discovery worker: %v", err)
	}
	if err := restartedService.Continue(t.Context(), runID); err != nil {
		t.Fatalf("continue interrupted discovery: %v", err)
	}
	if err := restartedService.runSchedulingCycle(t.Context(), time.UnixMilli(700_000)); err != nil {
		t.Fatalf("complete interrupted discovery: %v", err)
	}
	if got, want := restartedAdapter.readCalls, []string{"boss-job-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("restarted read calls = %#v, want interrupted job once %#v", got, want)
	}
	assertGlobalJobIDs(t, jobpool.New(restartedDB), "boss-job-1")
}

func TestDiscoveryRestartFinishesAPageBeforeSwitchingSearchRange(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	databasePath := filepath.Join(directory, "boss-job-agent.db")
	logPath := filepath.Join(directory, "boss-job-agent.jsonl")
	db := mustOpenDiscoveryDatabase(t, databasePath)
	logs := runlog.Open(logPath)
	reader := &resumeReader{content: discoveryResume("Go 后端工程师")}
	firstRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	secondRange := SearchRange{
		Role: "平台工程师", City: "厦门", Salary: "25-35K", EmploymentType: "全职",
	}
	reader.content.JobIntentions = []onlineresume.JobIntention{
		{Role: firstRange.Role, City: firstRange.City, Salary: firstRange.Salary, EmploymentType: firstRange.EmploymentType},
		{Role: secondRange.Role, City: secondRange.City, Salary: secondRange.Salary, EmploymentType: secondRange.EmploymentType},
	}
	adapter := discoveryAcrossRanges(firstRange, secondRange)
	versions := onlineresume.New(db, reader, logs, func() time.Time { return time.UnixMilli(1000) })
	refreshDiscoveryResume(t, versions)
	service := New(db, versions, jobpool.New(db), adapter, logs, func() time.Time {
		return time.UnixMilli(2000)
	})
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_discovery_range_switch
		BEFORE UPDATE OF current_role ON discovery_runs
		WHEN OLD.current_role = 'Go 后端工程师' AND NEW.current_role = '平台工程师'
		BEGIN
			SELECT RAISE(ABORT, 'simulated crash before range switch');
		END
	`); err != nil {
		t.Fatalf("create range switch failure trigger: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err == nil {
		t.Fatal("discovery cycle succeeded, want range switch failure")
	}
	assertPersistedPageCheckpoint(t, db, runID, `["boss-job-1"]`, false, 1)
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER fail_discovery_range_switch`); err != nil {
		t.Fatalf("drop range switch failure trigger: %v", err)
	}
	if err := logs.Close(); err != nil {
		t.Fatalf("close first runlog: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first discovery database: %v", err)
	}

	restartedDB := mustOpenDiscoveryDatabase(t, databasePath)
	t.Cleanup(func() { _ = restartedDB.Close() })
	restartedLogs := runlog.Open(logPath)
	t.Cleanup(func() { _ = restartedLogs.Close() })
	restartedAdapter := discoveryAcrossRanges(firstRange, secondRange)
	restartedService := New(
		restartedDB,
		onlineresume.New(restartedDB, reader, restartedLogs, func() time.Time { return time.UnixMilli(3000) }),
		jobpool.New(restartedDB),
		restartedAdapter,
		restartedLogs,
		func() time.Time { return time.UnixMilli(3000) },
	)
	if err := restartedService.Continue(t.Context(), runID); err != nil {
		t.Fatalf("continue discovery after range switch crash: %v", err)
	}
	if err := restartedService.runSchedulingCycle(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("complete discovery after range switch crash: %v", err)
	}
	if got, want := restartedAdapter.readCalls, []string{"boss-job-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("restarted reads = %#v, want only next range job %#v", got, want)
	}
	assertGlobalJobIDs(t, jobpool.New(restartedDB), "boss-job-1", "boss-job-2")
}

func discoveryAcrossRanges(firstRange, secondRange SearchRange) *controlledJobDiscovery {
	return &controlledJobDiscovery{pages: map[fetchCall]controlledPage{
		{SearchRange: firstRange, Page: 1}: {
			Observations: []JobObservation{discoveredJob("boss-job-1")}, HasMore: false,
		},
		{SearchRange: secondRange, Page: 1}: {
			Observations: []JobObservation{discoveredJob("boss-job-2")}, HasMore: false,
		},
	}}
}

func runUntilCheckpointAdvanceFailure(t *testing.T, databasePath, logPath string) (int64, *resumeReader) {
	t.Helper()

	db, err := storage.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("open discovery database: %v", err)
	}
	logs := runlog.Open(logPath)
	reader := &resumeReader{content: discoveryResume("Go 后端工程师")}
	versions := onlineresume.New(db, reader, logs, func() time.Time { return time.UnixMilli(1000) })
	refreshDiscoveryResume(t, versions)
	firstAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{"boss-job-1", "boss-job-2"}, HasMore: false},
		jobs: map[string]JobObservation{
			"boss-job-1": discoveredJob("boss-job-1"),
			"boss-job-2": discoveredJob("boss-job-2"),
		},
	}
	firstPool := jobpool.New(db)
	firstService := New(db, versions, firstPool, firstAdapter, logs, func() time.Time {
		return time.UnixMilli(2000)
	})
	runID, err := firstService.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_first_job_checkpoint_advance
		BEFORE UPDATE OF next_job_ordinal ON discovery_runs
		WHEN OLD.next_job_ordinal = 0 AND NEW.next_job_ordinal = 1
		BEGIN
			SELECT RAISE(ABORT, 'simulated crash before checkpoint advance');
		END
	`); err != nil {
		t.Fatalf("create checkpoint failure trigger: %v", err)
	}
	if err := firstService.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err == nil {
		t.Fatal("first discovery cycle succeeded, want checkpoint advance failure")
	}
	assertGlobalJobIDs(t, firstPool, "boss-job-1")
	assertPersistedPageCheckpoint(t, db, runID, `["boss-job-1","boss-job-2"]`, false, 0)
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER fail_first_job_checkpoint_advance`); err != nil {
		t.Fatalf("drop checkpoint failure trigger: %v", err)
	}
	if err := logs.Close(); err != nil {
		t.Fatalf("close first runlog: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first discovery process database: %v", err)
	}
	return runID, reader
}

func TestDiscoveryRejectsAChangedListBeforeResumingTheFrozenPage(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	databasePath := filepath.Join(directory, "boss-job-agent.db")
	logPath := filepath.Join(directory, "boss-job-agent.jsonl")
	runID, reader := runUntilSecondJobPlatformLimit(t, databasePath, logPath)

	changedAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{"boss-job-1", "boss-job-3"}, HasMore: false},
		jobs: map[string]JobObservation{"boss-job-3": discoveredJob("boss-job-3")},
	}
	db := mustOpenDiscoveryDatabase(t, databasePath)
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(logPath)
	t.Cleanup(func() { _ = logs.Close() })
	pool := jobpool.New(db)
	service := New(
		db,
		onlineresume.New(db, reader, logs, func() time.Time { return time.UnixMilli(3000) }),
		pool,
		changedAdapter,
		logs,
		func() time.Time { return time.UnixMilli(3000) },
	)
	if err := service.Continue(t.Context(), runID); err != nil {
		t.Fatalf("continue discovery: %v", err)
	}
	err := service.runSchedulingCycle(t.Context(), time.UnixMilli(3000))
	var fetchErr *FetchError
	if !errors.As(err, &fetchErr) || fetchErr.Category != FetchErrorInvalidResponse {
		t.Fatalf("changed list error = %v, want invalid_response", err)
	}
	if len(changedAdapter.readCalls) != 0 {
		t.Errorf("jobs read from changed list = %#v, want none", changedAdapter.readCalls)
	}
	assertGlobalJobIDs(t, pool, "boss-job-1")
	assertPersistedPageCheckpoint(t, db, runID, `["boss-job-1","boss-job-2"]`, false, 1)
	_ = service.runSchedulingCycle(t.Context(), time.UnixMilli(3_603_000))
	if len(changedAdapter.listCalls) != 1 {
		t.Errorf("changed list calls = %d, want no automatic retry after invalid_response", len(changedAdapter.listCalls))
	}
}

func runUntilSecondJobPlatformLimit(t *testing.T, databasePath, logPath string) (int64, *resumeReader) {
	t.Helper()
	db := mustOpenDiscoveryDatabase(t, databasePath)
	logs := runlog.Open(logPath)
	reader := &resumeReader{content: discoveryResume("Go 后端工程师")}
	versions := onlineresume.New(db, reader, logs, func() time.Time { return time.UnixMilli(1000) })
	firstAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{"boss-job-1", "boss-job-2"}, HasMore: false},
		jobs: map[string]JobObservation{"boss-job-1": discoveredJob("boss-job-1")},
		readErrors: map[string]error{
			"boss-job-2": &FetchError{Category: FetchErrorPlatformLimited, Cause: errors.New("BOSS_PLATFORM_LIMITED")},
		},
	}
	pool := jobpool.New(db)
	service := New(db, versions, pool, firstAdapter, logs, func() time.Time {
		return time.UnixMilli(2000)
	})
	refreshDiscoveryResume(t, versions)
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err == nil {
		t.Fatal("first cycle succeeded, want second job failure")
	}
	assertPersistedPageCheckpoint(t, db, runID, `["boss-job-1","boss-job-2"]`, false, 1)
	assertGlobalJobIDs(t, pool, "boss-job-1")
	if err := logs.Close(); err != nil {
		t.Fatalf("close first runlog: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first discovery database: %v", err)
	}
	return runID, reader
}

func TestDiscoveryRunlogSeparatesPageListsAndJobReadsWithoutStableIDs(t *testing.T) {
	t.Parallel()

	const platformJobID = "boss-sensitive-job-1"
	discoveryAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{platformJobID}, HasMore: false},
		jobs: map[string]JobObservation{platformJobID: discoveredJob(platformJobID)},
	}
	db, _, versions := openDiscoveryResumeTest(t)
	logPath := filepath.Join(t.TempDir(), "discovery.jsonl")
	logs := runlog.Open(logPath)
	t.Cleanup(func() { _ = logs.Close() })
	service := New(
		db,
		versions,
		jobpool.New(db),
		discoveryAdapter,
		logs,
		func() time.Time { return time.UnixMilli(2000) },
	)
	refreshDiscoveryResume(t, versions)
	if _, err := service.Start(t.Context()); err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err != nil {
		t.Fatalf("run discovery: %v", err)
	}
	records, err := os.ReadFile(logPath) // #nosec G304 -- the test owns this temporary runlog path.
	if err != nil {
		t.Fatalf("read discovery runlog: %v", err)
	}
	text := string(records)
	for _, want := range []string{
		`"operation":"list_page"`,
		`"operation":"read_job"`,
		`"page_no":1`,
		`"job_ordinal":1`,
		`"job_id_fingerprint":"9542806604c794eebc1517859836f31a3cf607ba0363d16be187120bb497c5fb"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("discovery runlog does not contain %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{platformJobID, `"search_role"`, `"search_city"`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("discovery runlog contains forbidden value %q: %s", forbidden, text)
		}
	}
}

func TestDiscoveryRunlogRedactsStableIDsFromFailedJobErrorTrees(t *testing.T) {
	t.Parallel()

	const requestedID = "boss-sensitive-requested-job"
	const returnedID = "boss-sensitive-returned-job"
	discoveryAdapter := &checkpointJobDiscovery{
		page: JobPage{PlatformJobIDs: []string{requestedID}, HasMore: false},
		jobs: map[string]JobObservation{requestedID: discoveredJob(returnedID)},
	}
	db, _, versions := openDiscoveryResumeTest(t)
	logPath := filepath.Join(t.TempDir(), "discovery-redaction.jsonl")
	logs := runlog.Open(logPath)
	t.Cleanup(func() { _ = logs.Close() })
	service := New(
		db,
		versions,
		jobpool.New(db),
		discoveryAdapter,
		logs,
		func() time.Time { return time.UnixMilli(2000) },
	)
	refreshDiscoveryResume(t, versions)
	if _, err := service.Start(t.Context()); err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err == nil {
		t.Fatal("run discovery succeeded with a mismatched stable ID")
	}
	records, err := os.ReadFile(logPath) // #nosec G304 -- the test owns this temporary runlog path.
	if err != nil {
		t.Fatalf("read discovery runlog: %v", err)
	}
	text := string(records)
	for _, forbidden := range []string{requestedID, returnedID} {
		if strings.Contains(text, forbidden) {
			t.Errorf("failed discovery runlog contains raw stable ID %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"job_id_fingerprint"`) || !strings.Contains(text, `"error_chain"`) {
		t.Errorf("failed discovery runlog lacks fingerprint or error chain: %s", text)
	}
}

func assertPersistedPageCheckpoint(
	t *testing.T,
	db *sql.DB,
	runID int64,
	wantJobIDs string,
	wantHasMore bool,
	wantOrdinal int,
) {
	t.Helper()
	var jobIDs string
	var hasMore int
	var ordinal int
	if err := db.QueryRowContext(t.Context(), `
		SELECT current_page_job_ids_json, current_page_has_more, next_job_ordinal
		FROM discovery_runs
		WHERE id = ?
	`, runID).Scan(&jobIDs, &hasMore, &ordinal); err != nil {
		t.Fatalf("read discovery page checkpoint: %v", err)
	}
	wantHasMoreInt := 0
	if wantHasMore {
		wantHasMoreInt = 1
	}
	if jobIDs != wantJobIDs || hasMore != wantHasMoreInt || ordinal != wantOrdinal {
		t.Errorf(
			"page checkpoint = %q/%d/%d, want %q/%d/%d",
			jobIDs,
			hasMore,
			ordinal,
			wantJobIDs,
			wantHasMoreInt,
			wantOrdinal,
		)
	}
}

func TestUnpreparedRunRemainsVisibleAndBlocksAnotherStart(t *testing.T) {
	t.Parallel()

	db, _, resumeVersions, _, service, _ := openDiscoveryServiceTest(t, &controlledJobDiscovery{})
	refreshDiscoveryResume(t, resumeVersions)
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO discovery_runs (status, attempt_no, created_at, updated_at)
		VALUES ('preparing', 0, 1000, 1000)
	`); err != nil {
		t.Fatalf("seed unprepared discovery: %v", err)
	}
	runID := assertUnpreparedRunVisibleAndBlocked(t, service)
	assertUnpreparedRunCanEnd(t, service, runID)
}

func assertUnpreparedRunVisibleAndBlocked(t *testing.T, service *Service) int64 {
	t.Helper()
	active, err := service.GetActiveResumeUse(t.Context())
	if err != nil {
		t.Fatalf("get active unprepared discovery: %v", err)
	}
	if active == nil || active.ResumeVersion != 0 {
		t.Fatalf("active unprepared discovery = %#v, want visible without resume version", active)
	}
	run, err := service.GetLatestRun(t.Context())
	if err != nil {
		t.Fatalf("get latest unprepared discovery: %v", err)
	}
	if run == nil || run.Status != StatusPreparing || run.TotalRanges != 0 {
		t.Fatalf("latest unprepared discovery = %#v", run)
	}
	availability, err := service.StartAvailability(t.Context())
	if err != nil {
		t.Fatalf("get start availability: %v", err)
	}
	assertUnavailable(t, availability, "unfinished_discovery_exists", "请先处理当前未结束的岗位发现运行")
	return run.ID
}

func assertUnpreparedRunCanEnd(t *testing.T, service *Service, runID int64) {
	t.Helper()
	if err := service.EndEarly(t.Context(), runID); err != nil {
		t.Fatalf("end unprepared discovery: %v", err)
	}
	ended, err := service.GetLatestRun(t.Context())
	if err != nil {
		t.Fatalf("get ended unprepared discovery: %v", err)
	}
	if ended == nil || ended.Status != StatusEndedEarly || ended.ResumeVersion != 0 || ended.TotalRanges != 0 {
		t.Fatalf("ended unprepared discovery = %#v", ended)
	}
}

func TestSchedulingCycleCoversEverySavedSearchRangeInOrderAndResetsThePage(t *testing.T) {
	t.Parallel()

	firstRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	secondRange := SearchRange{
		Role: "平台工程师", City: "厦门", Salary: "25-35K", EmploymentType: "全职",
	}
	discoveryAdapter := &controlledJobDiscovery{pages: map[fetchCall]controlledPage{
		{SearchRange: firstRange, Page: 1}: {
			Observations: []JobObservation{discoveredJob("boss-job-1")}, HasMore: true,
		},
		{SearchRange: firstRange, Page: 2}: {
			Observations: []JobObservation{discoveredJob("boss-job-1"), discoveredJob("boss-job-2")},
			HasMore:      false,
		},
		{SearchRange: secondRange, Page: 1}: {
			Observations: []JobObservation{discoveredJob("boss-job-3")}, HasMore: false,
		},
	}}
	_, reader, versions, pool, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
	reader.content.JobIntentions = []onlineresume.JobIntention{
		{Role: firstRange.Role, City: firstRange.City, Salary: firstRange.Salary, EmploymentType: firstRange.EmploymentType},
		{Role: secondRange.Role, City: secondRange.City, Salary: secondRange.Salary, EmploymentType: secondRange.EmploymentType},
	}
	refreshed := refreshDiscoveryResume(t, versions)

	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if runID <= 0 {
		t.Fatalf("discovery run ID = %d, want positive", runID)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err != nil {
		t.Fatalf("run discovery scheduling cycle: %v", err)
	}
	want := []fetchCall{
		{SearchRange: firstRange, Page: 1},
		{SearchRange: firstRange, Page: 2},
		{SearchRange: secondRange, Page: 1},
	}
	got := discoveryAdapter.calls
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fetch calls = %#v, want %#v", got, want)
	}
	assertCompletedRun(t, service, refreshed.Current.Version, secondRange, 1, 3)
	assertGlobalJobIDs(t, pool, "boss-job-1", "boss-job-2", "boss-job-3")
}

func TestPauseInvalidatesAnInFlightWorkerAndContinueResumesTheSameCheckpoint(t *testing.T) {
	t.Parallel()

	blocked := &blockingJobDiscovery{
		started: make(chan struct{}),
		release: make(chan struct{}),
		page: controlledPage{
			Observations: []JobObservation{discoveredJob("late-job")}, HasMore: false,
		},
	}
	_, _, versions, pool, service, _ := openDiscoveryServiceTest(t, blocked)
	refreshDiscoveryResume(t, versions)
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}

	cycleResult := make(chan error, 1)
	go func() {
		cycleResult <- service.runSchedulingCycle(t.Context(), time.UnixMilli(2000))
	}()
	<-blocked.started
	if err := service.Pause(t.Context(), runID); err != nil {
		t.Fatalf("pause discovery: %v", err)
	}
	close(blocked.release)
	if err := <-cycleResult; err != nil {
		t.Fatalf("finish invalidated worker: %v", err)
	}
	assertRunCheckpoint(t, service, runID, StatusPaused, 1, 0)
	assertGlobalJobIDs(t, pool)

	searchRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	service.discovery = &controlledJobDiscovery{pages: map[fetchCall]controlledPage{
		{SearchRange: searchRange, Page: 1}: {
			Observations: []JobObservation{discoveredJob("late-job")}, HasMore: false,
		},
	}}
	if err := service.Continue(t.Context(), runID); err != nil {
		t.Fatalf("continue discovery: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("run continued discovery: %v", err)
	}
	assertRunCheckpoint(t, service, runID, StatusCompleted, 1, 1)
	assertGlobalJobIDs(t, pool, "late-job")
}

func TestCrossProcessPauseCannotOvertakeCurrentJobObservation(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	databasePath := filepath.Join(directory, "boss-job-agent.db")
	db := mustOpenDiscoveryDatabase(t, databasePath)
	t.Cleanup(func() { _ = db.Close() })
	logs := runlog.Open(filepath.Join(directory, "first.jsonl"))
	t.Cleanup(func() { _ = logs.Close() })
	reader := &resumeReader{content: discoveryResume("Go 后端工程师")}
	versions := onlineresume.New(db, reader, logs, func() time.Time { return time.UnixMilli(1000) })
	refreshDiscoveryResume(t, versions)
	pool := jobpool.New(db)
	service := New(db, versions, pool, &checkpointJobDiscovery{}, logs, func() time.Time {
		return time.UnixMilli(2000)
	})
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	searchRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	worker := workerAttempt{
		runID: runID, attemptNo: 1, owner: service.workerOwner,
		currentSearchRange: searchRange, nextPage: 1,
	}
	checkpoint, err := service.freezePage(t.Context(), worker, JobPage{
		PlatformJobIDs: []string{"boss-job-1"}, HasMore: false,
	}, time.UnixMilli(2000))
	if err != nil {
		t.Fatalf("freeze discovery page: %v", err)
	}
	worker.pageCheckpoint = checkpoint
	secondDB := mustOpenDiscoveryDatabase(t, databasePath)
	t.Cleanup(func() { _ = secondDB.Close() })
	secondLogs := runlog.Open(filepath.Join(directory, "second.jsonl"))
	t.Cleanup(func() { _ = secondLogs.Close() })
	secondService := New(
		secondDB,
		onlineresume.New(secondDB, reader, secondLogs, func() time.Time { return time.UnixMilli(3000) }),
		jobpool.New(secondDB),
		&checkpointJobDiscovery{},
		secondLogs,
		func() time.Time { return time.UnixMilli(3000) },
	)

	transaction, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin current job observation: %v", err)
	}
	defer func() { _ = transaction.Rollback() }()
	transactionQueries := service.queries.WithTx(transaction)
	if _, err := transactionQueries.LockCurrentDiscoveryJob(
		t.Context(),
		lockCurrentJobParams(worker, "boss-job-1", 0),
	); err != nil {
		t.Fatalf("lock current discovery job: %v", err)
	}

	pauseContext, cancelPause := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancelPause()
	if err := secondService.Pause(pauseContext, runID); err == nil {
		t.Fatal("cross-process pause overtook a locked current job observation")
	}
	if _, err := pool.ObserveInTransaction(
		t.Context(), transaction, runID, toPoolObservation(discoveredJob("boss-job-1"), time.UnixMilli(2000)),
	); err != nil {
		t.Fatalf("observe locked current discovery job: %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit locked current discovery job: %v", err)
	}
	if err := secondService.Pause(t.Context(), runID); err != nil {
		t.Fatalf("pause after current job observation committed: %v", err)
	}
	_, err = service.queries.AdvanceDiscoveryJob(t.Context(), sqlitedb.AdvanceDiscoveryJobParams{
		NextJobOrdinal:    nullInt64(1),
		ProgressAt:        nullInt64(2000),
		WorkerLeaseUntil:  nullInt64(time.UnixMilli(2000).Add(discoveryWorkerLease).UnixMilli()),
		UpdatedAt:         2000,
		RunID:             runID,
		AttemptNo:         1,
		WorkerOwner:       nullString(worker.owner),
		CurrentRole:       nullString(searchRange.Role),
		CurrentCity:       nullString(searchRange.City),
		CurrentPage:       nullInt64(1),
		JobIdsJson:        nullString(checkpoint.encodedJobIDs),
		HasMore:           nullInt64(0),
		CurrentJobOrdinal: nullInt64(0),
		PlatformJobID:     nullString("boss-job-1"),
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old worker checkpoint advance error = %v, want stale no rows", err)
	}
	assertGlobalJobIDs(t, pool, "boss-job-1")
}

func lockCurrentJobParams(
	worker workerAttempt,
	platformJobID string,
	jobOrdinal int,
) sqlitedb.LockCurrentDiscoveryJobParams {
	return sqlitedb.LockCurrentDiscoveryJobParams{
		RunID:         worker.runID,
		AttemptNo:     worker.attemptNo,
		WorkerOwner:   nullString(worker.owner),
		CurrentRole:   nullString(worker.currentSearchRange.Role),
		CurrentCity:   nullString(worker.currentSearchRange.City),
		NextPage:      nullInt64(int64(worker.nextPage)),
		JobIdsJson:    nullString(worker.pageCheckpoint.encodedJobIDs),
		HasMore:       nullInt64(boolInt64(worker.pageCheckpoint.hasMore)),
		JobOrdinal:    nullInt64(int64(jobOrdinal)),
		PlatformJobID: nullString(platformJobID),
	}
}

func TestEachPersistedJobRenewsTheWorkerLeaseUsingFreshTime(t *testing.T) {
	t.Parallel()

	now := time.UnixMilli(2000)
	adapter := &leaseRenewalJobDiscovery{
		page: JobPage{
			PlatformJobIDs: []string{"boss-job-1", "boss-job-2", "boss-job-3"},
			HasMore:        false,
		},
		jobs: map[string]JobObservation{
			"boss-job-1": discoveredJob("boss-job-1"),
			"boss-job-2": discoveredJob("boss-job-2"),
		},
		now:         &now,
		blockOnID:   "boss-job-3",
		readStarted: make(chan struct{}),
		releaseRead: make(chan struct{}),
	}
	db, _, versions, _, service, _ := openDiscoveryServiceTest(t, adapter)
	service.now = func() time.Time { return now }
	refreshDiscoveryResume(t, versions)
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	cycleResult := make(chan error, 1)
	go func() {
		cycleResult <- service.runSchedulingCycle(t.Context(), time.UnixMilli(2000))
	}()
	<-adapter.readStarted

	var progressAt, leaseUntil int64
	if err := db.QueryRowContext(t.Context(), `
		SELECT last_progress_at, worker_lease_until
		FROM discovery_runs
		WHERE id = ?
	`, runID).Scan(&progressAt, &leaseUntil); err != nil {
		t.Fatalf("read renewed discovery lease: %v", err)
	}
	wantProgress := time.UnixMilli(2000).Add(12 * time.Minute)
	if progressAt != wantProgress.UnixMilli() || leaseUntil != wantProgress.Add(discoveryWorkerLease).UnixMilli() {
		t.Errorf(
			"progress/lease = %d/%d, want %d/%d",
			progressAt,
			leaseUntil,
			wantProgress.UnixMilli(),
			wantProgress.Add(discoveryWorkerLease).UnixMilli(),
		)
	}
	close(adapter.releaseRead)
	if err := <-cycleResult; err == nil {
		t.Fatal("discovery cycle succeeded after the blocked job returned platform_limited")
	}
}

func TestTransientFailureRetriesAtMostThreeTimesWithoutHumanIntervention(t *testing.T) {
	t.Parallel()

	searchRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	call := fetchCall{SearchRange: searchRange, Page: 1}
	discoveryAdapter := &controlledJobDiscovery{errors: map[fetchCall]error{
		call: &FetchError{Category: FetchErrorTransient, Cause: errors.New("temporary network failure")},
	}}
	_, _, versions, _, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
	refreshDiscoveryResume(t, versions)
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}

	cycleTimes := []time.Time{
		time.UnixMilli(2000),
		time.UnixMilli(61_000),
		time.UnixMilli(62_000),
		time.UnixMilli(122_000),
		time.UnixMilli(182_000),
	}
	wantCalls := []int{1, 1, 2, 3, 3}
	for index, now := range cycleTimes {
		_ = service.runSchedulingCycle(t.Context(), now)
		if got := len(discoveryAdapter.calls); got != wantCalls[index] {
			t.Fatalf("after cycle %d fetch calls = %d, want %d", index+1, got, wantCalls[index])
		}
	}
	assertRunCheckpoint(t, service, runID, StatusFailed, 1, 0)
}

func TestHumanActionErrorsDoNotRetryWithoutContinue(t *testing.T) {
	t.Parallel()

	for _, category := range []FetchErrorCategory{
		FetchErrorAuthenticationExpired,
		FetchErrorVerificationRequired,
		FetchErrorPlatformLimited,
	} {
		category := category
		t.Run(string(category), func(t *testing.T) {
			t.Parallel()
			discoveryAdapter := &checkpointJobDiscovery{
				page: JobPage{PlatformJobIDs: []string{"boss-job-1"}, HasMore: false},
				readErrors: map[string]error{
					"boss-job-1": &FetchError{Category: category, Cause: errors.New("human action required")},
				},
			}
			db, _, versions, _, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
			refreshDiscoveryResume(t, versions)
			runID, err := service.Start(t.Context())
			if err != nil {
				t.Fatalf("start discovery: %v", err)
			}
			_ = service.runSchedulingCycle(t.Context(), time.UnixMilli(2000))
			_ = service.runSchedulingCycle(t.Context(), time.UnixMilli(3_602_000))
			if got := len(discoveryAdapter.readCalls); got != 1 {
				t.Fatalf("read calls = %d, want one attempt before human Continue", got)
			}
			assertPersistedPageCheckpoint(t, db, runID, `["boss-job-1"]`, false, 0)
		})
	}
}

func TestRunLoopExecutesOneCycleImmediately(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cycles := 0
	runDiscoveryLoop(ctx, make(chan struct{}), func() {
		cycles++
		cancel()
	})
	if cycles != 1 {
		t.Fatalf("immediate scheduling cycles = %d, want 1", cycles)
	}
}

func TestSchedulingCycleMarksAnExpiredWorkerFailedWithoutStartingAnotherAttempt(t *testing.T) {
	t.Parallel()

	db, _, versions, _, service, _ := openDiscoveryServiceTest(t, &controlledJobDiscovery{})
	refreshed := refreshDiscoveryResume(t, versions)
	runID := seedRunningDiscovery(t, db, refreshed.Current.ID, "old-worker", 1000)

	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err != nil {
		t.Fatalf("scan expired discovery worker: %v", err)
	}
	assertRunCheckpoint(t, service, runID, StatusFailed, 1, 0)
}

func TestFailureToPersistFailedStateWritesTechnicalRunlog(t *testing.T) {
	t.Parallel()

	searchRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	discoveryAdapter := &controlledJobDiscovery{errors: map[fetchCall]error{
		{SearchRange: searchRange, Page: 1}: errors.New("fetch failed"),
	}}
	db, _, versions := openDiscoveryResumeTest(t)
	logPath := filepath.Join(t.TempDir(), "discovery.jsonl")
	logs := runlog.Open(logPath)
	t.Cleanup(func() { _ = logs.Close() })
	service := New(
		db,
		versions,
		jobpool.New(db),
		discoveryAdapter,
		logs,
		func() time.Time { return time.UnixMilli(2000) },
	)
	refreshDiscoveryResume(t, versions)
	if _, err := service.Start(t.Context()); err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_discovery_state_update
		BEFORE UPDATE OF status ON discovery_runs
		WHEN NEW.status = 'failed'
		BEGIN
			SELECT RAISE(ABORT, 'state write failed');
		END
	`); err != nil {
		t.Fatalf("create failing state trigger: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err == nil {
		t.Fatal("scheduling cycle error is nil, want failed state persistence error")
	}
	// #nosec G304 -- the test owns this temporary runlog path.
	records, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read discovery runlog: %v", err)
	}
	text := string(records)
	for _, want := range []string{`"event":"technical_error"`, `"stage":"mark_worker_failed"`, `"trace_id":"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("runlog does not contain %q: %s", want, text)
		}
	}
	traceMatches := regexp.MustCompile(`"trace_id":"([0-9a-f]{32})"`).FindAllStringSubmatch(text, -1)
	if len(traceMatches) < 3 {
		t.Fatalf("runlog trace records = %d, want external start, finish, and technical error: %s", len(traceMatches), text)
	}
	for _, match := range traceMatches[1:] {
		if match[1] != traceMatches[0][1] {
			t.Fatalf("related runlog trace IDs differ: %v", traceMatches)
		}
	}
}

func TestCommandStateWriteFailureWritesTechnicalRunlog(t *testing.T) {
	t.Parallel()

	db, _, versions := openDiscoveryResumeTest(t)
	logPath := filepath.Join(t.TempDir(), "discovery.jsonl")
	logs := runlog.Open(logPath)
	t.Cleanup(func() { _ = logs.Close() })
	service := New(
		db,
		versions,
		jobpool.New(db),
		&controlledJobDiscovery{},
		logs,
		func() time.Time { return time.UnixMilli(2000) },
	)
	refreshDiscoveryResume(t, versions)
	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_discovery_pause
		BEFORE UPDATE OF status ON discovery_runs
		WHEN NEW.status = 'paused'
		BEGIN
			SELECT RAISE(ABORT, 'pause write failed');
		END
	`); err != nil {
		t.Fatalf("create failing pause trigger: %v", err)
	}
	if err := service.Pause(t.Context(), runID); err == nil {
		t.Fatal("pause error is nil, want state persistence error")
	}
	// #nosec G304 -- the test owns this temporary runlog path.
	records, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read discovery runlog: %v", err)
	}
	text := string(records)
	for _, want := range []string{`"event":"technical_error"`, `"stage":"pause_run"`, fmt.Sprintf(`"discovery_run_id":%d`, runID)} {
		if !strings.Contains(text, want) {
			t.Fatalf("runlog does not contain %q: %s", want, text)
		}
	}
}

func assertRunCheckpoint(
	t *testing.T,
	service *Service,
	runID int64,
	status Status,
	page int,
	discoveredJobs int,
) {
	t.Helper()
	run, err := service.GetLatestRun(t.Context())
	if err != nil {
		t.Fatalf("get discovery run: %v", err)
	}
	if run == nil {
		t.Fatal("latest discovery run is nil")
	}
	if run.ID != runID || run.Status != status || run.NextPage != page || run.DiscoveredJobs != discoveredJobs {
		t.Errorf(
			"discovery run = %#v, want ID %d, status %q, page %d, jobs %d",
			run,
			runID,
			status,
			page,
			discoveredJobs,
		)
	}
}

func assertCompletedRun(
	t *testing.T,
	service *Service,
	resumeVersion int,
	searchRange SearchRange,
	page int,
	discoveredJobs int,
) {
	t.Helper()
	run, err := service.GetLatestRun(t.Context())
	if err != nil {
		t.Fatalf("get latest discovery run: %v", err)
	}
	if run == nil {
		t.Fatal("latest discovery run is nil")
	}
	if run.ResumeVersion != resumeVersion || run.CurrentRange != searchRange {
		t.Errorf("discovery inputs = %#v, want resume v%d and %#v", run, resumeVersion, searchRange)
	}
	if run.Status != StatusCompleted ||
		run.CompletedRanges != run.TotalRanges ||
		run.NextPage != page ||
		run.DiscoveredJobs != discoveredJobs {
		t.Errorf(
			"discovery progress = %#v, want completed on page %d with %d jobs",
			run,
			page,
			discoveredJobs,
		)
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
	matches := len(got) == len(wants)
	for index := 0; matches && index < len(got); index++ {
		matches = got[index] == wants[index]
	}
	if !matches {
		t.Errorf("global job IDs = %#v, want %#v", got, wants)
	}
}

func TestInvalidObservationKeepsEarlierReliableJobsAndStopsAtItsOwnCheckpoint(t *testing.T) {
	t.Parallel()

	invalid := discoveredJob("boss-job-invalid")
	invalid.Requirements = ""
	searchRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	discoveryAdapter := &controlledJobDiscovery{pages: map[fetchCall]controlledPage{
		{SearchRange: searchRange, Page: 1}: {
			Observations: []JobObservation{discoveredJob("boss-job-1"), invalid}, HasMore: false,
		},
	}}
	_, _, versions, pool, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
	refreshDiscoveryResume(t, versions)

	runID, err := service.Start(t.Context())
	if err != nil {
		t.Fatalf("start discovery: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err == nil {
		t.Fatal("start discovery succeeded, want unreliable page failure")
	}
	assertRunCheckpoint(t, service, runID, StatusFailed, 1, 1)
	assertGlobalJobIDs(t, pool, "boss-job-1")
}

func TestObservationWithoutReliableSalaryCanBeSaved(t *testing.T) {
	t.Parallel()

	job := discoveredJob("boss-job-without-salary")
	job.Salary = ""
	searchRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	discoveryAdapter := &controlledJobDiscovery{pages: map[fetchCall]controlledPage{
		{SearchRange: searchRange, Page: 1}: {Observations: []JobObservation{job}, HasMore: false},
	}}
	_, _, versions, pool, service, _ := openDiscoveryServiceTest(t, discoveryAdapter)
	refreshDiscoveryResume(t, versions)

	if _, err := service.Start(t.Context()); err != nil {
		t.Fatalf("start discovery without reliable job salary: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(2000)); err != nil {
		t.Fatalf("run discovery without reliable job salary: %v", err)
	}
	jobs, err := pool.ListJobs(t.Context())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Salary != "" {
		t.Errorf("jobs = %#v, want one saved job with unavailable salary", jobs)
	}
}

func TestRunlogFailureEvidencePreservesTheFirstFailedUpstreamRequest(t *testing.T) {
	t.Parallel()

	evidence := runlogFailureEvidence(&FetchError{
		Category: FetchErrorPlatformLimited,
		Evidence: &FetchFailureEvidence{
			RequestOrdinal: 7,
			Stage:          "job_detail",
			DetailOrdinal:  4,
			UpstreamCode:   "37",
		},
		Cause: errors.New("BOSS_PLATFORM_LIMITED"),
	})
	want := &runlog.ExternalFailureEvidence{
		RequestOrdinal: 7,
		Stage:          "job_detail",
		DetailOrdinal:  4,
		UpstreamCode:   "37",
	}
	if !reflect.DeepEqual(evidence, want) {
		t.Errorf("runlog failure evidence = %#v, want %#v", evidence, want)
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
	discoveryAdapter := &controlledJobDiscovery{}
	service := New(db, versions, pool, discoveryAdapter, logs, func() time.Time { return time.UnixMilli(2000) })
	assertDiscoveryResumeIsolation(t, service, versions)
	active, err := service.GetActiveResumeUse(t.Context())
	if err != nil || active == nil {
		t.Fatalf("get active resume use: active=%#v err=%v", active, err)
	}
	if err := service.Continue(t.Context(), active.DiscoveryRunID); err != nil {
		t.Fatalf("continue v1 discovery: %v", err)
	}
	if err := service.runSchedulingCycle(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("run v1 discovery after v2 refresh: %v", err)
	}
	wantRange := SearchRange{
		Role: "Go 后端工程师", City: "福州", Salary: "20-30K", EmploymentType: "全职",
	}
	if len(discoveryAdapter.calls) != 1 || discoveryAdapter.calls[0].SearchRange != wantRange {
		t.Errorf("continued discovery calls = %#v, want frozen v1 range %#v", discoveryAdapter.calls, wantRange)
	}
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
	versions := onlineresume.New(db, reader, logs, func() time.Time { return time.UnixMilli(1000) })
	return db, reader, versions
}

func mustOpenDiscoveryDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := storage.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open discovery database %q: %v", path, err)
	}
	return db
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

func seedRunningDiscovery(
	t *testing.T,
	db *sql.DB,
	resumeVersionID int64,
	workerOwner string,
	leaseUntil int64,
) int64 {
	t.Helper()
	result, err := db.ExecContext(t.Context(), `
		INSERT INTO discovery_runs (
			resume_version_id, current_role, current_city, next_page,
			status, attempt_no, worker_owner, worker_lease_until,
			created_at, prepared_at, updated_at
		) VALUES (?, 'Go 后端工程师', '福州', 1, 'running', 1, ?, ?, 1000, 1000, 1000)
	`, resumeVersionID, workerOwner, leaseUntil)
	if err != nil {
		t.Fatalf("seed running discovery: %v", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read seeded discovery run ID: %v", err)
	}
	return runID
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
