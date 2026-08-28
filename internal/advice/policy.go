package advice

import (
	"context"
	"database/sql"
	"fmt"
)

const defaultPolicyJSON = `{"rules":["只依据本次实际采用的在线简历和 JD，不猜测未提供的经历","有明确且重要的不匹配证据时判为不适合","有明确匹配证据时判为适合","信息不足或证据冲突时需要人工确认"]}`

// EnsureDefaultPolicy creates the immutable default policy only when the
// instance has never had a policy version.
func EnsureDefaultPolicy(ctx context.Context, db *sql.DB, nowMillis int64) error {
	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO assessment_policy_versions (
			version_no, rules_json, is_active, change_note, created_at
		) VALUES (1, ?, 1, '系统默认策略', ?)
	`, defaultPolicyJSON, nowMillis); err != nil {
		return fmt.Errorf("create default assessment policy: %w", err)
	}
	return nil
}
