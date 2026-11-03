package cmd

import (
	"io"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskRecordsAndEventsUseLocalDisplayTime(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("test", 3600)
	t.Cleanup(func() { time.Local = old })
	at := time.Date(2026, 1, 2, 23, 26, 0, 0, time.UTC)
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "task", "new", "dev", "Review")
	require.NoError(t, err)
	a := &app{crew: file, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		tk, err := s.Tasks.Get("1")
		if err != nil {
			return err
		}
		tk.Created = at
		r, err := s.Role("dev")
		if err != nil {
			return err
		}
		shape := s.Crew.HandoffFor(r)
		values := map[string]string{}
		for _, section := range shape.Sections {
			values[section.Name] = "Review complete"
		}
		for _, kind := range []string{"result", "checkpoint"} {
			ref, err := record.Save(s.Crew.StateDir(), record.Document{Version: 1, Kind: kind, Task: tk.ID, Role: tk.Role, At: at, Shape: shape, Values: values})
			if err != nil {
				return err
			}
			if kind == "result" {
				tk.Result = &ref
			} else {
				tk.Checkpoint = &ref
			}
		}
		if err := s.Tasks.Put(tk); err != nil {
			return err
		}
		return eventLog(s.Crew).Add(events.Event{TS: at, Role: "dev", Task: tk.ID, Event: "reviewed"})
	}))
	for _, args := range [][]string{{"task", "result", "1"}, {"task", "checkpoint", "1"}, {"task", "show", "1"}, {"log", "--task", "1"}} {
		t.Run(args[0]+"/"+args[1], func(t *testing.T) {
			out, _, err := zm(t, file, args...)
			require.NoError(t, err)
			assert.Contains(t, out, "2026-01-03 00:26:00")
			assert.NotContains(t, out, "2026-01-02 23:26:00")
		})
	}
	for _, args := range [][]string{{"task", "result", "1", "--json"}, {"task", "show", "1", "--json"}, {"log", "--task", "1", "--json"}} {
		out, _, err := zm(t, file, args...)
		require.NoError(t, err)
		assert.Contains(t, out, "2026-01-02T23:26:00Z", "JSON keeps stored timestamps")
	}
}
