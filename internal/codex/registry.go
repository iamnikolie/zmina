// Package codex bridges Codex App Server to zmina's long-lived role contract.
package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/iamnikolie/zmina/internal/registry"
)

// Record is an adapter-owned snapshot, replaced atomically after each lifecycle event.
type Record struct {
	registry.Session
	Turn registry.Turn `json:"turn"`
}

// Registry reads only this crew's Codex snapshots, never Codex's private database.
type Registry struct{ Dir string }

func (r *Registry) read(path string) (*Record, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ByPID finds the bridge process that owns a tmux pane.
func (r *Registry) ByPID(pid int) (*registry.Session, error) {
	rec, err := r.read(filepath.Join(r.Dir, strconv.Itoa(pid)+".json"))
	if err != nil || rec == nil {
		return nil, err
	}
	return &rec.Session, nil
}

// LiveByName finds an existing bridge with the seat's session name.
func (r *Registry) LiveByName(name string) *registry.Session {
	files, _ := filepath.Glob(filepath.Join(r.Dir, "*.json"))
	for _, f := range files {
		rec, err := r.read(f)
		if err == nil && rec != nil && rec.Name == name && syscall.Kill(rec.PID, 0) == nil {
			return &rec.Session
		}
	}
	return nil
}

// Last returns the structured state of a thread's latest turn.
func (r *Registry) Last(id string) (registry.Turn, error) {
	files, err := filepath.Glob(filepath.Join(r.Dir, "*.json"))
	if err != nil {
		return registry.Turn{}, err
	}
	for _, f := range files {
		rec, err := r.read(f)
		if err != nil {
			return registry.Turn{}, err
		}
		if rec != nil && rec.SessionID == id {
			return rec.Turn, nil
		}
	}
	return registry.Turn{}, nil
}

// Save publishes one complete snapshot; partial writes are never visible.
func (r *Registry) Save(rec Record) error {
	if err := os.MkdirAll(r.Dir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := filepath.Join(r.Dir, fmt.Sprint(rec.PID)+".json")
	if err := os.WriteFile(path+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
