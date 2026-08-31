package runlog

import "time"

// Health is the current persistent logging health projected to callers and Web.
type Health struct {
	Healthy                bool      `json:"healthy"`
	Code                   string    `json:"code"`
	Message                string    `json:"message"`
	CheckedAt              time.Time `json:"checkedAt"`
	ConfirmationRequired   bool      `json:"confirmationRequired"`
	PendingTerminalRecords int       `json:"pendingTerminalRecords"`
}
