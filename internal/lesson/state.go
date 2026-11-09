package lesson

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Open is the lessons merge request that waits on the owner; while one is open, new lessons wait.
type Open struct {
	Branch string    `json:"branch"`
	Target string    `json:"target"`
	URL    string    `json:"url,omitempty"`
	Base   string    `json:"base"`
	Tip    string    `json:"tip"`
	IDs    []string  `json:"ids"`
	Todo   string    `json:"todo,omitempty"`
	Opened time.Time `json:"opened"`
	// Waiting is the count of new lessons the todo last said; the todo is rewritten when it changes.
	Waiting int `json:"waiting,omitempty"`
}

// RunState is the triage's own record, next to the lessons: the open merge request, the last run and its cost.
type RunState struct {
	Open    *Open     `json:"open,omitempty"`
	LastRun time.Time `json:"last_run,omitzero"`
	// LastError is why the last run failed, empty when it did not.
	LastError string  `json:"last_error,omitempty"`
	LastCost  float64 `json:"last_cost,omitempty"`
	// Running is when a run began; a run older than an hour is taken as gone.
	Running time.Time `json:"running,omitzero"`
	// Checked is when the open merge request was last looked at on the remote.
	Checked time.Time `json:"checked,omitzero"`
	// Catchup is the command the owner was given to bring the roles' checkout up to the merge, until it has it.
	Catchup string `json:"catchup,omitempty"`
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// LoadState reads the record; none yet is an empty one.
func LoadState(dir string) (RunState, error) {
	var st RunState

	raw, err := os.ReadFile(statePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}

	if err != nil {
		return st, fmt.Errorf("lessons state: %w", err)
	}

	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("lessons state %s: %w", statePath(dir), err)
	}

	return st, nil
}

// SaveState writes the record whole.
func SaveState(dir string, st RunState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lessons state: %w", err)
	}

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("lessons state: %w", err)
	}

	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("lessons state: %w", err)
	}

	if err := os.Rename(tmp, statePath(dir)); err != nil {
		return fmt.Errorf("lessons state: %w", err)
	}

	return nil
}
