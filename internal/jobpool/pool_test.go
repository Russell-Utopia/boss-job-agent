package jobpool

import (
	"errors"
	"testing"
	"time"

	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func TestObserveKeepsOneGlobalJobPerStablePlatformID(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pool := New(db)

	first := observedJob("boss-job-1")
	if _, err := pool.Observe(t.Context(), 1, first); err != nil {
		t.Fatalf("observe first platform job: %v", err)
	}
	first.ObservedAt = first.ObservedAt.Add(time.Minute)
	if _, err := pool.Observe(t.Context(), 2, first); err != nil {
		t.Fatalf("observe same platform job again: %v", err)
	}
	if _, err := pool.Observe(t.Context(), 2, observedJob("boss-job-2")); err != nil {
		t.Fatalf("observe similar job with new platform ID: %v", err)
	}

	jobs, err := pool.ListJobs(t.Context())
	if err != nil {
		t.Fatalf("list global jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("global jobs = %d, want 2", len(jobs))
	}
	if jobs[0].PlatformJobID != "boss-job-1" || jobs[1].PlatformJobID != "boss-job-2" {
		t.Errorf("platform job IDs = %q, %q", jobs[0].PlatformJobID, jobs[1].PlatformJobID)
	}
}

func observedJob(platformJobID string) Observation {
	return Observation{
		PlatformJobID:    platformJobID,
		CanonicalURL:     "https://www.zhipin.com/job_detail/" + platformJobID + ".html",
		JobTitle:         "Go 后端工程师",
		CompanyName:      "示例科技",
		City:             "福州",
		Salary:           "20-30K",
		Responsibilities: "负责 Go 服务开发",
		Requirements:     "熟悉 Go 与 SQLite",
		PlatformStatus:   PlatformStatusOpen,
		ObservedAt:       time.UnixMilli(1000),
	}
}

func TestObserveKeepsJobWhenReliableSalaryIsUnavailable(t *testing.T) {
	t.Parallel()

	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pool := New(db)

	job := observedJob("boss-job-without-salary")
	job.Salary = ""
	got, err := pool.Observe(t.Context(), 1, job)
	if err != nil {
		t.Fatalf("observe platform job without reliable salary: %v", err)
	}
	if got.Salary != "" {
		t.Errorf("observed salary = %q, want unavailable empty value", got.Salary)
	}

	jobs, err := pool.ListJobs(t.Context())
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Salary != "" {
		t.Errorf("listed jobs = %#v, want one job with unavailable salary", jobs)
	}
}

func TestReliableSalaryAppearingChangesJudgmentHash(t *testing.T) {
	t.Parallel()

	withoutSalary := observedJob("boss-job-salary-change")
	withoutSalary.Salary = ""
	_, withoutHash, err := encodeJudgmentContent(withoutSalary)
	if err != nil {
		t.Fatalf("encode job without reliable salary: %v", err)
	}
	_, withHash, err := encodeJudgmentContent(observedJob("boss-job-salary-change"))
	if err != nil {
		t.Fatalf("encode job with reliable salary: %v", err)
	}
	if withoutHash == withHash {
		t.Fatal("judgment hash did not change when reliable salary appeared")
	}
}

func TestQueueAuthorizedOutreachRejectsWhenNoJobIsAvailable(t *testing.T) {
	t.Parallel()

	pool := &Pool{}
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
