package jobpool

import (
	"context"
)

// Pool owns the global platform job state machine.
type Pool struct{}

type OutreachAuthorization struct {
	GreetingText    string
	TimeDescription string
}

type ActionAvailability struct {
	Allowed bool
	Code    string
	Reason  string
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

func New() *Pool {
	return &Pool{}
}

func (p *Pool) OutreachAvailability(_ context.Context) ActionAvailability {
	return ActionAvailability{
		Code:   "outreach_unavailable",
		Reason: "当前没有可真实打招呼的岗位",
	}
}

func (p *Pool) QueueAuthorizedOutreach(
	ctx context.Context,
	_ []int64,
	_ OutreachAuthorization,
) error {
	availability := p.OutreachAvailability(ctx)
	return &Rejection{Code: availability.Code, Reason: availability.Reason}
}
