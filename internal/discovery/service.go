package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Russell-Utopia/boss-job-agent/internal/discovery/internal/sqlitedb"
	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
)

// Service owns job discovery runs.
type Service struct {
	resumeVersions *onlineresume.Versions
	queries        *sqlitedb.Queries
}

type ActiveResumeUse struct {
	DiscoveryRunID int64 `json:"-"`
	ResumeVersion  int   `json:"resumeVersion"`
}

type ActionAvailability struct {
	Allowed bool   `json:"allowed"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type Rejection struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func (r *Rejection) Error() string {
	return r.Reason
}

func (r *Rejection) RejectionCode() string {
	return r.Code
}

func (r *Rejection) RejectionReason() string {
	return r.Reason
}

func New(db *sql.DB, resumeVersions *onlineresume.Versions) *Service {
	return &Service{resumeVersions: resumeVersions, queries: sqlitedb.New(db)}
}

func (s *Service) GetActiveResumeUse(ctx context.Context) (*ActiveResumeUse, error) {
	row, err := s.queries.GetActiveDiscoveryResumeUse(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query active discovery resume use: %w", err)
	}
	return &ActiveResumeUse{
		DiscoveryRunID: row.DiscoveryRunID,
		ResumeVersion:  int(row.ResumeVersionNo),
	}, nil
}

func (s *Service) StartAvailability(ctx context.Context) (ActionAvailability, error) {
	current, err := s.resumeVersions.GetCurrent(ctx)
	if err != nil {
		return ActionAvailability{}, err
	}
	if current == nil {
		return ActionAvailability{
			Code:   "online_resume_required",
			Reason: "请先刷新在线简历，再开始岗位发现",
		}, nil
	}
	return ActionAvailability{
		Code:   "discovery_unavailable",
		Reason: "当前版本尚未开放岗位发现",
	}, nil
}

func (s *Service) Start(ctx context.Context) error {
	availability, err := s.StartAvailability(ctx)
	if err != nil {
		return err
	}
	if availability.Allowed {
		return nil
	}
	return &Rejection{Code: availability.Code, Reason: availability.Reason}
}
