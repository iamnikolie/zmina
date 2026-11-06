package shift

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/task"
)

// unstartedNotes counts the dispatcher's notes about a task left on a down role.
func unstartedNotes(s *Shift) []string {
	var out []string

	for _, n := range s.State.Notes {
		if strings.Contains(n, unstartedWord) {
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
	assert.Equal(t, "designer: down, has not taken "+tk.ID+"; zmina --crew "+shellWord(f.s.Crew.Path)+
		" up 'designer' --task "+tk.ID+" starts it on it", notes[0])
	assert.Equal(t, "unstarted", f.s.State.NoticeMeta[notes[0]].Kind)
}

// A notice still waiting for the dispatcher is dropped once it is no longer true: the role started, or the task
// was taken.
func TestUnstartedNoticeIsCheckedBeforeDelivery(t *testing.T) {
	cases := map[string]func(f fix){
		"role started":  func(f fix) { require.NoError(t, f.s.Up("designer")) },
		"task accepted": func(f fix) { f.tk.list[0].Delivery = &task.Delivery{AcceptedAt: *f.now} },
		"task done":     func(f fix) { f.tk.list[0].State = task.Done },
		"role asleep": func(f fix) {
			f.s.Alarms = &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}
			_, _, err := f.s.Sleep("designer", f.now.Add(time.Hour), "+1h", "", DownOptions{})
			require.NoError(t, err)
		},
	}

	for name, resolve := range cases {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			_, err := f.tk.Add(task.Task{Role: "designer", Asked: "look"}, *f.now)
			require.NoError(t, err)

			*f.now = f.now.Add(unstartedFor)
			f.s.Tick() // the dispatcher is down: the notice waits
			require.Len(t, unstartedNotes(f.s), 1)
			current := func() []string {
				return slices.DeleteFunc(f.s.currentNotes(), func(n string) bool { return !strings.Contains(n, unstartedWord) })
			}
			assert.Len(t, current(), 1, "still true: it would be delivered")

			resolve(f)
			assert.Empty(t, current(), "no longer true: not delivered")
			assert.Empty(t, unstartedNotes(f.s))
		})
	}
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
