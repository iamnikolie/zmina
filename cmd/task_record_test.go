package cmd

import (
	"encoding/json"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoleResultSavedShownAndNotifiedOnce(t *testing.T) {
	file := testCrew(t, "  support:\n    handoff:\n      sections:\n        - name: draft\n          guidance: Proposed response\n")
	_, _, err := zm(t, file, "task", "new", "support", "Answer the question")
	require.NoError(t, err)
	body := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"draft":"Reply in Russian: ответ.\nSecond line"}`), 0600))
	for i := 0; i < 2; i++ {
		_, _, err = zm(t, file, "task", "result", "1", "--file", body)
		require.NoError(t, err)
	}
	out, _, err := zm(t, file, "task", "show", "1", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "draft:\nReply in Russian: ответ.")
	out, _, err = zm(t, file, "task", "show", "1")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^result: [0-9a-f]{12} · .* · Reply in Russian: ответ\. Second line$`, out, "compact: the result in one line")
	assert.Contains(t, out, "zmina task result T-1")
	out, _, err = zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out, "T-1 result ready (worker-reported): Reply in Russian: ответ. (zmina task result T-1 --revision "))
	require.NoError(t, os.WriteFile(body, []byte(`{"draft":"Revised response"}`), 0600))
	_, _, err = zm(t, file, "task", "done", "1", "--result", body)
	require.NoError(t, err)
	out, _, err = zm(t, file, "task", "result", "1", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "Revised response")
}
func TestUITaskIncludesLatestRoleShapedResult(t *testing.T) {
	uiHome(t)
	file := crewPathOf(t, "default")
	_, _, err := zm(t, file, "task", "new", "dev", "Review")
	require.NoError(t, err)
	c, err := crew.Load(file)
	require.NoError(t, err)
	shape := c.HandoffFor(c.Roles["dev"])
	values := map[string]string{}
	for _, s := range shape.Sections {
		values[s.Name] = "Completed section"
	}
	ref, err := record.Save(c.StateDir(), record.Document{Version: 1, Kind: "result", Task: "T-1", Role: "dev", Shape: shape, Values: values})
	require.NoError(t, err)
	ts := taskStore(c)
	tk, err := ts.Get("1")
	require.NoError(t, err)
	tk.Result = &ref
	require.NoError(t, ts.Put(tk))
	srv, _ := uiServer(t, true, "")
	resp, err := http.Get(srv.URL + "/api/task?crew=default&id=T-1")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Task   task.Task       `json:"task"`
		Result record.Document `json:"result"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, values, got.Result.Values)
}

func TestCheckpointRemainsReadableWhenParkedWorkGetsNewTaskID(t *testing.T) {
	file := testCrew(t, "  support:\n    handoff:\n      sections:\n        - name: draft\n          guidance: Draft and open question\n")
	_, _, err := zm(t, file, "task", "new", "support", "Prepare reply")
	require.NoError(t, err)
	body := filepath.Join(t.TempDir(), "checkpoint.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"draft":"Reply needs one answer"}`), 0600))
	_, _, err = zm(t, file, "task", "checkpoint", "1", "--file", body)
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "park", "1", "--note", "One question remains")
	require.NoError(t, err)
	_, _, err = zm(t, file, "later", "take", "1", "support")
	require.NoError(t, err)
	out, _, err := zm(t, file, "task", "checkpoint", "2", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"task": "T-1"`)
	assert.Contains(t, out, "Reply needs one answer")
	out, _, err = zm(t, file, "task", "show", "2", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "source task: T-1")
	out, _, err = zm(t, file, "task", "show", "2")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^checkpoint: [0-9a-f]{12} · .* · from T-1 · Reply needs one answer$`, out)
}

func TestCheckpointFreshnessUsesTaskSnapshotAndExplicitRevalidation(t *testing.T) {
	file := testCrew(t, "  support:\n    handoff:\n      sections:\n        - name: draft\n")
	_, _, err := zm(t, file, "task", "new", "support", "Prepare reply")
	require.NoError(t, err)
	body := filepath.Join(t.TempDir(), "checkpoint.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"draft":"Ready to continue"}`), 0600))
	c, err := crew.Load(file)
	require.NoError(t, err)
	a := &app{crew: file, out: io.Discard, err: io.Discard}
	var first record.Ref
	for i := 0; i < 2; i++ {
		_, _, err = zm(t, file, "task", "checkpoint", "1", "--file", body)
		require.NoError(t, err)
		tk, err := taskStore(c).Get("1")
		require.NoError(t, err)
		if i == 0 {
			first = *tk.Checkpoint
		} else {
			assert.Equal(t, first, *tk.Checkpoint, "identical content keeps immutable record/time")
		}
		require.NoError(t, a.withState(func(s *shift.Shift) error {
			s.State.Role("support").Releasing = time.Now()
			_, err := s.Park(tk, "Remaining work")
			return err
		}))
		parked, err := taskStore(c).Get("1")
		require.NoError(t, err)
		require.False(t, parked.HandoffIncomplete, "a just-saved checkpoint is valid even with a real advancing CLI clock")
		if i == 0 {
			// Change the task after its first checkpoint, then explicitly revalidate identical content.
			parked.State = task.Open
			parked.Updated = time.Now()
			parked.Note = "New task context"
			parked.HandoffIncomplete = true
			require.NoError(t, taskStore(c).Put(parked))
		}
	}
}

// A task's result is told beside a role's own line about the task (zmina note): the line names the task but need
// not be about the result. The notice waits as an info line, with no id; with info_lines off it waits with its id as
// before.
func TestResultNoticeIsMadeBesideTheRolesNote(t *testing.T) {
	const handoff = "  support:\n    handoff:\n      sections:\n        - name: draft\n          guidance: Proposed response\n"

	for _, c := range []struct {
		name, crew string
		note       bool
		results    int
	}{
		{"the role's note first", handoff, true, 1},
		{"no note", handoff, false, 1},
		{"info_lines off", handoff + "info_lines: false\n", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := testCrew(t, c.crew)
			_, _, err := zm(t, file, "task", "new", "support", "Answer the question")
			require.NoError(t, err)

			if c.note {
				t.Setenv("ZMINA_ROLE", "support")
				_, _, err = zm(t, file, "note", "T-1 is answered, the draft is in the result")
				require.NoError(t, err)
				t.Setenv("ZMINA_ROLE", "")
			}

			body := filepath.Join(t.TempDir(), "result.json")
			require.NoError(t, os.WriteFile(body, []byte(`{"draft":"Reply"}`), 0600))
			_, _, err = zm(t, file, "task", "result", "1", "--file", body)
			require.NoError(t, err)

			out, _, err := zm(t, file, "queue", "ls")
			require.NoError(t, err)
			assert.Equal(t, c.results, strings.Count(out, "T-1 result ready (worker-reported)"), out)

			if c.note {
				assert.Contains(t, out, "support: T-1 is answered, the draft is in the result")
			}

			_, _, err = zm(t, file, "ack", "S-99")
			assert.NoError(t, err, "an id that waits for no ack is a warning, not an error")
		})
	}
}
