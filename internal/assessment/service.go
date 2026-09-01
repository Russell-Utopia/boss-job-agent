package assessment

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Russell-Utopia/boss-job-agent/internal/assessment/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/automationsettings"
	"github.com/Russell-Utopia/boss-job-agent/internal/jobpool"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
	"github.com/Russell-Utopia/boss-job-agent/internal/runlog"
)

const defaultPolicyJSON = `{"rules":["只依据本次实际采用的在线简历和 JD，不猜测未提供的经历","有明确且重要的不匹配证据时判为不适合","有明确匹配证据时判为适合","信息不足或证据冲突时需要人工确认"]}`

// Service owns assessment policies.
type Service struct {
	queries        *sqlitedb.Queries
	resumeVersions *onlineresume.Versions
	pool           *jobpool.Pool
	settings       *automationsettings.Settings
	submitter      AssessmentSubmitter
	logs           *runlog.Log
	now            func() time.Time
	cycleMu        sync.Mutex
}

type Policy struct {
	ID      int64    `json:"-"`
	Version int      `json:"version"`
	Name    string   `json:"name"`
	Rules   []string `json:"rules"`
}

func New(
	db *sql.DB,
	resumeVersions *onlineresume.Versions,
	pool *jobpool.Pool,
	settings *automationsettings.Settings,
	submitter AssessmentSubmitter,
	logs *runlog.Log,
	now func() time.Time,
) *Service {
	return &Service{
		queries:        sqlitedb.New(db),
		resumeVersions: resumeVersions,
		pool:           pool,
		settings:       settings,
		submitter:      submitter,
		logs:           logs,
		now:            now,
	}
}

func (s *Service) EnsureDefaultPolicy(ctx context.Context, now time.Time) error {
	err := s.queries.EnsureDefaultPolicy(ctx, sqlitedb.EnsureDefaultPolicyParams{
		RulesJson:  defaultPolicyJSON,
		ChangeNote: sql.NullString{String: "系统默认策略", Valid: true},
		CreatedAt:  now.UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("create default assessment policy: %w", err)
	}
	return nil
}

func (s *Service) GetActivePolicy(ctx context.Context) (Policy, error) {
	row, err := s.queries.GetActivePolicy(ctx)
	if err != nil {
		return Policy{}, fmt.Errorf("query active assessment policy: %w", err)
	}
	var document struct {
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal([]byte(row.RulesJson), &document); err != nil {
		return Policy{}, fmt.Errorf("decode active assessment policy: %w", err)
	}
	version := int(row.VersionNo)
	name := fmt.Sprintf("策略 v%d", version)
	if version == 1 && row.ChangeNote.String == "系统默认策略" {
		name = "默认策略 v1"
	}
	return Policy{ID: row.ID, Version: version, Name: name, Rules: document.Rules}, nil
}
