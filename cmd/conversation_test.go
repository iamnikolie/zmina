package cmd

import (
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"testing"
)

func TestTaskUpSelectsSavedConversationAndExplicitFresh(t *testing.T) {
	for _, mode := range []string{"default", "--fresh", "--continue"} {
		t.Run(mode, func(t *testing.T) {
			file := testCrew(t, "  dev: {}\n")
			_, _, err := zm(t, file, "task", "new", "dev", "Review")
			require.NoError(t, err)
			a := &app{crew: file, out: io.Discard, err: io.Discard}
			require.NoError(t, a.withState(func(s *shift.Shift) error {
				r, _ := s.Role("dev")
				t, _ := s.Tasks.Get("1")
				t.Conversations = []task.Conversation{{Node: s.Node, Crew: s.Crew.Name, CrewPath: s.Crew.Path, Role: "dev#2", Runtime: "claude", Home: s.Crew.Namespace, Session: "saved-task-session", Cwd: r.Dir, LastObservedWork: s.Now()}}
				return s.Tasks.Put(t)
			}))
			args := []string{"up", "dev", "--task", "1"}
			if mode != "default" {
				args = append(args, mode)
			}
			out, _, err := zm(t, file, args...)
			require.NoError(t, err)
			if mode == "--fresh" {
				assert.Contains(t, out, "fresh (requested)")
			} else {
				assert.Contains(t, out, "continue saved-task-session")
			}
		})
	}
}
