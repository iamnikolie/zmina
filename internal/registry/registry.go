// Package registry reads Claude Code's own session registry: <namespace>/sessions/<pid>.json, one file per running
// claude, with its session id, name and idle/busy status.
package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Session is one entry of the registry.
type Session struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Name            string `json:"name"`
	Cwd             string `json:"cwd"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
}

// StatusSince is when the status last changed.
func (s *Session) StatusSince() time.Time { return time.UnixMilli(s.StatusUpdatedAt) }

// Registry reads one namespace's registry.
type Registry struct {
	// Namespace is the CLAUDE_CONFIG_DIR.
	Namespace string
}

// ByPID returns the entry of a running claude, or nil when it has not written one yet.
func (r *Registry) ByPID(pid int) (*Session, error) {
	s, err := read(filepath.Join(r.Namespace, "sessions", strconv.Itoa(pid)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	return s, err
}

// LiveByName returns a running session with that name, or nil. Entries of dead processes are skipped.
func (r *Registry) LiveByName(name string) *Session {
	files, _ := filepath.Glob(filepath.Join(r.Namespace, "sessions", "*.json"))
	for _, f := range files {
		s, err := read(f)
		if err != nil || s.Name != name || !alive(s.PID) {
			continue
		}

		return s
	}

	return nil
}

func read(path string) (*Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}

	return &s, nil
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}
