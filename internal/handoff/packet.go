// Package handoff stores deterministic task packets outside the runtime transcript.
package handoff

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/task"
)

// Packet preserves the full task, including multiline requests and the latest note.
type Packet struct {
	Shape        crew.HandoffShape `json:"shape"`
	Version      int               `json:"version"`
	Role         string            `json:"role"`
	Task         task.Task         `json:"task"`
	Instructions string            `json:"instructions"`
}

// Write publishes an immutable content-addressed snapshot and returns its absolute path.
func Write(dir string, p Packet) (string, error) {
	if task.Normalize(p.Task.ID) != p.Task.ID || filepath.Base(p.Task.ID) != p.Task.ID {
		return "", fmt.Errorf("invalid task ID %q", p.Task.ID)
	}
	return publish(filepath.Join(dir, "handoffs", p.Task.ID), p)
}

// Release writes one short-locator request containing all role-shaped checkpoint packets.
func Release(dir, role string, packets []Packet) (string, error) {
	return publish(filepath.Join(dir, "releases"), struct {
		Role         string   `json:"role"`
		Tasks        []Packet `json:"tasks"`
		Instructions string   `json:"instructions"`
	}{role, packets, "Write each requested checkpoint, then park each task with what remains. Do not invent missing information."})
}

// Restart writes one short-locator request for the role's handoff notes before zmina restarts it fresh.
func Restart(dir, role string, packets []Packet) (string, error) {
	return publish(filepath.Join(dir, "restarts"), struct {
		Role         string   `json:"role"`
		Tasks        []Packet `json:"tasks"`
		Instructions string   `json:"instructions"`
	}{role, packets, "Write each requested checkpoint as the note your fresh self starts from: what is done, what is left, " +
		"where things are, open questions. Then stop: zmina restarts you fresh with each task's packet and its note. " +
		"Do not park the tasks or start new work. Do not invent missing information."})
}

func publish(dir string, value any) (string, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256(raw))))
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".packet-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
