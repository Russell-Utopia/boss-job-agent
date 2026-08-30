package discovery

import (
	"context"

	"github.com/Russell-Utopia/boss-job-agent/internal/onlineresume"
)

// Service owns job discovery runs.
type Service struct {
	resumeVersions *onlineresume.Versions
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

func New(resumeVersions *onlineresume.Versions) *Service {
	return &Service{resumeVersions: resumeVersions}
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
