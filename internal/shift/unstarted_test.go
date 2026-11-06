package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
)

// unstartedNotes counts the dispatcher's notes about a task left on a down role.
func unstartedNotes(s *Shift) []string {
	var out []string

	for _, n := range s.State.Notes {
		if strings.Contains(n, "has not taken") {
			out = append(out, n)
		}
	}

	return out
}

func TestTaskOnADownRoleIsToldOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	tk, err := f.tk.Add(task.Task{Role: "designer", Asked: "look"}, *f.now)
	require.NoError(t, err)

	f.s.Tick()
	assert.Empty(t, unstartedNotes(f.s), "not before five minutes")

	*f.now = f.now.Add(unstartedFor)
	f.s.Tick()
	f.s.Tick()

	notes := unstartedNotes(f.s)
	require.Len(t, notes, 1, "once per task")
	assert.Equal(t, "designer is down and has not taken "+tk.ID+"; zmina up designer --task "+tk.ID+" starts it on it", notes[0])
}

func TestTaskOnADownRoleQuiet(t *testing.T) {
	cases := map[string]func(f fix, id string){
		"role up": func(f fix, _ string) { require.NoError(t, f.s.Up("designer")) },
		"accepted before it went down": func(f fix, id string) {
			for i := range f.tk.list {
				if f.tk.list[i].ID == id {
					f.tk.list[i].Delivery = &task.Delivery{AcceptedAt: *f.now}
				}
			}
		},
		"shift stopped": func(f fix, _ string) { f.s.State.Stopped = *f.now },
		"waiting":       func(f fix, _ string) { f.tk.list[0].State = task.Waiting },
	}

	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			tk, err := f.tk.Add(task.Task{Role: "designer", Asked: "look"}, *f.now)
			require.NoError(t, err)
			set(f, tk.ID)

			*f.now = f.now.Add(unstartedFor + time.Minute)
			f.s.Tick()

			assert.Empty(t, unstartedNotes(f.s))
		})
	}
}
