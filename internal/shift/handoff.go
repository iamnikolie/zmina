package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/task"
	"path/filepath"
	"slices"
	"strings"
)

// TaskPacket renders current task state and the assigned role's reporting shape.
func (s *Shift) TaskPacket(name string, t task.Task) (handoff.Packet, error) {
	r, err := s.Role(name)
	if err != nil {
		return handoff.Packet{}, err
	}
	shape := s.Crew.HandoffFor(r)
	t.Delivery = nil // Receipt metadata must not recursively change the packet identity.
	return handoff.Packet{Version: 1, Role: name, Task: t, Shape: shape, Instructions: fmt.Sprintf("Read repository instructions. Run zmina --crew %s task show %s and confirm this task is still assigned to %s; use the latest note before working.\n", shellWord(s.Crew.Path), t.ID, name) + shape.Request("checkpoint or result")}, nil
}

// HandTask queues only a packet locator, never the task's body.
func (s *Shift) HandTask(name string, t task.Task) (string, error) {
	p, err := s.TaskPacket(name, t)
	if err != nil {
		return "", err
	}
	path, err := handoff.Write(s.Crew.StateDir(), p)
	if err != nil {
		return "", err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	line := fmt.Sprintf("[zmina %s] Read handoff packet %q; before work run: zmina --crew %s task accept %s --handoff %s --role %s", t.ID, path, shellWord(s.Crew.Path), t.ID, id, shellWord(name))
	current, err := s.Tasks.Get(t.ID)
	if err != nil {
		return "", err
	}
	renew := current.Delivery != nil && s.State.Role(name).Started.After(current.Delivery.QueuedAt)
	if current.Delivery != nil && current.Delivery.ID == id && !current.Delivery.AcceptedAt.IsZero() {
		live := s.session(name)
		if !renew && live != nil && live.SessionID == current.Delivery.Session {
			return path, nil
		}
		renew = true
	}
	if renew || current.Delivery == nil || current.Delivery.ID != id || current.Delivery.Role != name {
		r, _ := s.Role(name)
		runtime, home := s.runtimeScope(r)
		current.Delivery = &task.Delivery{ID: id, Role: name, Runtime: runtime, Home: home, Line: line, TaskUpdated: current.Updated, QueuedAt: s.Now().UTC()}
		if err := s.Tasks.Put(current); err != nil {
			return "", err
		}
	}

	prefix := "[zmina " + t.ID + "] Read handoff packet "
	for _, rs := range s.State.Roles {
		rs.Inbox = slices.DeleteFunc(rs.Inbox, func(old string) bool { return strings.HasPrefix(old, prefix) && old != line })
	}
	if !slices.Contains(s.State.Role(name).Inbox, line) {
		if err := s.Queue(name, line); err != nil {
			return "", err
		}
	}
	return path, nil
}

func shellWord(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
