package task

import "time"

// Delivery distinguishes a handed task from one explicitly accepted by its live worker.
type Delivery struct {
	ID string `json:"id"`
	// Gen counts the hand-outs of the task: each new delivery is one more than the last (NextGen), 0 for a file made before
	// it was kept. A task whose delivery has one tells the session that holds it from one that held it before
	// (ledger.Current); a delivery put back by a rollback or an abort puts its number back too.
	Gen         int       `json:"gen,omitempty"`
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
