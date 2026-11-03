package task

import "time"

// Conversation is a task-scoped runtime locator, not a copy of the transcript.
type Conversation struct {
	Node             string    `json:"node,omitempty"`
	Crew             string    `json:"crew"`
	CrewPath         string    `json:"crew_path"`
	Role             string    `json:"role"`
	Runtime          string    `json:"runtime"`
	Home             string    `json:"home"`
	Session          string    `json:"session"`
	Cwd              string    `json:"cwd"`
	LastObservedWork time.Time `json:"last_observed_work"`
	Context          int       `json:"context,omitempty"`
}
