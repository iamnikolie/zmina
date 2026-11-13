// Package handoff stores deterministic task packets outside the runtime transcript.
package handoff

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/task"
)

// The directories under the crew's state directory the packets are written to: a task's packets each in a directory
// of the task's id, release and restart requests as files of their own.
const (
	PacketsDir  = "handoffs"
	ReleasesDir = "releases"
	RestartsDir = "restarts"
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
	return publish(filepath.Join(dir, PacketsDir, p.Task.ID), p)
}

// Release writes one short-locator request containing all role-shaped checkpoint packets.
func Release(dir, role string, packets []Packet) (string, error) {
	return publish(filepath.Join(dir, ReleasesDir), struct {
		Role         string   `json:"role"`
		Tasks        []Packet `json:"tasks"`
		Instructions string   `json:"instructions"`
	}{role, packets, "Write each requested checkpoint, then park each task with what remains. Do not invent missing information."})
}

// Checkpoint writes one short-locator request for the role's checkpoints while its plan's window fills: the same request a
// restart's is, in the same directory (the retention pass keeps it clean), with the words that say no restart follows.
func Checkpoint(dir, role string, packets []Packet) (string, error) {
	return publish(filepath.Join(dir, RestartsDir), struct {
		Role         string   `json:"role"`
		Tasks        []Packet `json:"tasks"`
		Instructions string   `json:"instructions"`
	}{role, packets, "Write each requested checkpoint as a note a fresh session could start from: what is done, what is left, " +
		"where things are, open questions. Then stop: no restart follows and your next line is work as usual. " +
		"Do not park the tasks or start new work. Do not invent missing information."})
}

// Restart writes one short-locator request for the role's handoff notes before zmina restarts it fresh.
func Restart(dir, role string, packets []Packet) (string, error) {
	return publish(filepath.Join(dir, RestartsDir), struct {
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
	if err := fsx.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256(raw))))
	if err != nil {
		return "", err
	}
	if err := fsx.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
