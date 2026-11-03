package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

func (s *Shift) releaseCheckpointRequest(name string, tasks []task.Task) (string, error) {
	packets := make([]handoff.Packet, 0, len(tasks))
	for _, t := range tasks {
		p, err := s.TaskPacket(name, t)
		if err != nil {
			return "", err
		}
		p.Instructions = fmt.Sprintf("Before release, write zmina --crew %s task checkpoint %s --file <json-file>, then park the task with what remains.\n", shellWord(s.Crew.Path), t.ID) + p.Shape.Request("checkpoint")
		packets = append(packets, p)
	}
	path, err := handoff.Release(s.Crew.StateDir(), name, packets)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("[zmina release] Read checkpoint request %q; write each checkpoint, park each task, then stop.", path), nil
}
func (s *Shift) checkpointCurrent(t task.Task) bool {
	if t.Checkpoint == nil {
		return false
	}
	if !t.CheckpointTaskUpdated.IsZero() {
		if !t.CheckpointTaskUpdated.Equal(t.Updated) {
			return false
		}
	} else if t.Checkpoint.At.Before(t.Updated) { // Legacy checkpoint references.
		return false
	}
	id := t.Checkpoint.Task
	if id == "" {
		id = t.ID
	}
	_, err := record.Load(s.Crew.StateDir(), "checkpoint", id, t.Checkpoint.Revision)
	return err == nil
}
