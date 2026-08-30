package assessment

import (
	"database/sql"
	"testing"
	"time"

	storage "github.com/Russell-Utopia/boss-job-agent/internal/sqlite"
)

func openTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db, err := storage.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), db
}

func TestDefaultPolicyIsReadyForTheFirstAssessment(t *testing.T) {
	t.Parallel()

	service, _ := openTestService(t)
	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure default policy: %v", err)
	}

	policy, err := service.GetActivePolicy(t.Context())
	if err != nil {
		t.Fatalf("get default policy: %v", err)
	}
	if policy.Version != 1 {
		t.Errorf("default policy version = %d, want 1", policy.Version)
	}
	if policy.Name != "默认策略 v1" {
		t.Errorf("default policy name = %q, want 默认策略 v1", policy.Name)
	}
	if len(policy.Rules) != 4 {
		t.Errorf("default policy rule count = %d, want 4", len(policy.Rules))
	}
}

func TestDefaultPolicyInitializationPreservesTheActiveSavedPolicy(t *testing.T) {
	t.Parallel()

	service, db := openTestService(t)
	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(1000)); err != nil {
		t.Fatalf("ensure default policy: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE assessment_policy_versions SET is_active = 0`); err != nil {
		t.Fatalf("deactivate default policy fixture: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (2, '{"rules":["用户保存的策略"]}', 1, '用户采用', 2000)
	`); err != nil {
		t.Fatalf("save policy fixture: %v", err)
	}

	if err := service.EnsureDefaultPolicy(t.Context(), time.UnixMilli(3000)); err != nil {
		t.Fatalf("ensure default policy after save: %v", err)
	}
	policy, err := service.GetActivePolicy(t.Context())
	if err != nil {
		t.Fatalf("get saved policy: %v", err)
	}
	if policy.Version != 2 {
		t.Errorf("active policy version = %d, want 2", policy.Version)
	}
	if policy.Name != "策略 v2" {
		t.Errorf("active policy name = %q, want 策略 v2", policy.Name)
	}
	if len(policy.Rules) != 1 || policy.Rules[0] != "用户保存的策略" {
		t.Errorf("active policy rules = %#v, want saved rules", policy.Rules)
	}
}
