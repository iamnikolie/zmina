package task

import "time"

// Delivery distinguishes a handed task from one explicitly accepted by its live worker.
type Delivery struct {
	ID          string    `json:"id"`
	Role        string    `json:"role"`
	Runtime     string    `json:"runtime"`
	Home        string    `json:"home"`
	Session     string    `json:"session,omitempty"`
	Line        string    `json:"line"`
	TaskUpdated time.Time `json:"task_updated"`
	QueuedAt    time.Time `json:"queued_at"`
	SentAt      time.Time `json:"sent_at,omitzero"`
	AcceptedAt  time.Time `json:"accepted_at,omitzero"`
	AlertedAt   time.Time `json:"alerted_at,omitzero"`
}

func (d Delivery) Status() string {
	if !d.AcceptedAt.IsZero() {
		return "accepted"
	}
	if !d.SentAt.IsZero() {
		return "sent, awaiting acceptance"
	}
	return "queued, awaiting acceptance"
}
