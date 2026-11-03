// Package recovery keeps explicit dispatcher follow-ups and disposable recovery projections.
package recovery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Followup struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Task     string    `json:"task,omitempty"`
	Source   string    `json:"source,omitempty"`
	At       time.Time `json:"at"`
	Resolved time.Time `json:"resolved,omitzero"`
}
type State struct {
	Version     int                 `json:"version"`
	Followups   map[string]Followup `json:"followups,omitempty"`
	ReadResults map[string]bool     `json:"read_results,omitempty"`
}
type Entry struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Ref  string `json:"ref,omitempty"`
	Data any    `json:"data,omitempty"`
}
type Pack struct {
	Version int                `json:"version"`
	At      time.Time          `json:"at"`
	Entries map[string][]Entry `json:"entries"`
	Counts  map[string]int     `json:"counts"`
	Index   string             `json:"full_index,omitempty"`
}

func Load(dir string) (State, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "dispatcher.json"))
	if os.IsNotExist(err) {
		return State{Version: 1}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err = json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.Version != 1 {
		return s, fmt.Errorf("unsupported dispatcher state version %d", s.Version)
	}
	return s, nil
}
func Save(dir string, s State) error {
	s.Version = 1
	_, err := write(filepath.Join(dir, "dispatcher.json"), s)
	return err
}
func Publish(dir, kind string, p Pack) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return write(filepath.Join(dir, "context", "dispatcher", fmt.Sprintf("%s-%x.json", kind, sha256.Sum256(raw))), p)
}
func write(path string, v any) (string, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".recovery-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return filepath.Abs(path)
}
