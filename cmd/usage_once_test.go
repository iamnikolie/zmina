package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

// usageCrew is a crew with one role, alpha; tail is added to its crew file after the roles (a timezone). It returns
// the crew file, the namespace's projects directory and the name alpha's sessions run under.
func usageCrew(t *testing.T, tail string) (crewFile, projects, session string) {
	t.Helper()

	crewFile = testCrew(t, "  alpha:\n"+tail)
	name := strings.TrimPrefix(strings.SplitN(string(mustRead(t, crewFile)), "\n", 2)[0], "name: ")

	return crewFile, filepath.Join(filepath.Dir(crewFile), "ns", "projects"), name + "-alpha"
}

// usageTurn is one reply of a session at a time: $0.080016 of opus-5.5, and a Bash call and its result.
func usageTurn(id string, at time.Time) string {
	ts := at.UTC().Format(time.RFC3339)

	return `{"type":"assistant","timestamp":"` + ts + `","cwd":"/work","message":{"id":"` + id +
		`","model":"claude-opus-5-5","usage":{"input_tokens":4,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":200000,"output_tokens":2000},"content":[{"type":"tool_use","id":"call-` + id +
		`","name":"Bash","input":{"command":"git log"}}]}}` + "\n" +
		`{"type":"user","timestamp":"` + ts + `","message":{"content":[{"type":"tool_result","tool_use_id":"call-` + id +
		`","content":"ok"}]}}` + "\n"
}

// usageSession is a transcript of the session with one turn for each id.
func usageSession(session string, ids ...string) string {
	out := `{"type":"agent-name","agentName":"` + session + `"}` + "\n"
	for i, id := range ids {
		out += usageTurn(id, time.Date(2026, 3, 10, 12, i, 0, 0, time.UTC))
	}

	return out
}

func usageWrite(t *testing.T, path, body string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// A session read twice is counted once: a project directory reached under two names (a link), a file linked into a
// second directory, and a conversation resumed from another directory, which copies it there, all show the same replies
// to two files. Another session's replies are still its own.
func TestUsageCountsASessionOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, projects, session string)
		turns int
	}{
		{"one project directory", func(t *testing.T, projects, session string) {}, 2},
		{"project directory linked under a second name", func(t *testing.T, projects, session string) {
			require.NoError(t, os.Symlink("-a", filepath.Join(projects, "-b")))
		}, 2},
		{"file linked into a second project directory", func(t *testing.T, projects, session string) {
			require.NoError(t, os.MkdirAll(filepath.Join(projects, "-b"), 0o700))
			require.NoError(t, os.Symlink(filepath.Join("..", "-a", "s1.jsonl"), filepath.Join(projects, "-b", "s1.jsonl")))
		}, 2},
		{"session copied into a second project directory", func(t *testing.T, projects, session string) {
			usageWrite(t, filepath.Join(projects, "-b", "s1.jsonl"), usageSession(session, "m1", "m2"))
		}, 2},
		{"a copy that is behind", func(t *testing.T, projects, session string) {
			usageWrite(t, filepath.Join(projects, "-b", "s1.jsonl"), usageSession(session, "m1"))
		}, 2},
		{"a copy that is ahead", func(t *testing.T, projects, session string) {
			usageWrite(t, filepath.Join(projects, "-b", "s1.jsonl"), usageSession(session, "m1", "m2", "m3"))
		}, 3},
		{"another session in a second project directory", func(t *testing.T, projects, session string) {
			usageWrite(t, filepath.Join(projects, "-b", "s2.jsonl"), usageSession(session, "m3", "m4"))
		}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crewFile, projects, session := usageCrew(t, "")
			usageWrite(t, filepath.Join(projects, "-a", "s1.jsonl"), usageSession(session, "m1", "m2"))
			tc.setup(t, projects, session)

			out, _, err := zm(t, crewFile, "usage", "--json", "--since", "3650d")
			require.NoError(t, err)

			var rep usageReport
			require.NoError(t, json.Unmarshal([]byte(out), &rep))
			require.Len(t, rep.Rows, 1)
			assert.Equal(t, "alpha", rep.Rows[0].Key)
			assert.Equal(t, tc.turns, rep.Total.Turns, "turns")
			assert.InDelta(t, 0.080016*float64(tc.turns), rep.Total.Cost, 1e-9, "dollars")
			assert.Equal(t, tc.turns*(4+200000+2000), rep.Total.Tokens(), "tokens")

			out, _, err = zm(t, crewFile, "usage", "--json", "--by", "tool", "--since", "3650d")
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal([]byte(out), &rep))
			require.Len(t, rep.Fills, 1)
			assert.Equal(t, "Bash: git", rep.Fills[0].Tool)
			assert.Equal(t, tc.turns, rep.Fills[0].Calls, "tool calls")
		})
	}
}

// A session that ran in a task's worktree is the crew's "(outside)" row whichever name of its project directory is
// the one read: the worktree's directory is a link to where the transcripts lie, and the directory they lie in sorts
// first, so it is the one listed.
func TestUsageOutsideSessionThroughALinkedDirectory(t *testing.T) {
	crewFile, projects, _ := usageCrew(t, "")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	wt := filepath.Join(t.TempDir(), "wt")
	_, err = taskStore(c).Add(task.Task{Role: "alpha", Asked: "x", Worktree: wt}, time.Now())
	require.NoError(t, err)

	usageWrite(t, filepath.Join(projects, "-0-real", "s9.jsonl"), usageSession("", "m1", "m2"))
	require.NoError(t, os.Symlink("-0-real", (&registry.Registry{Namespace: c.Namespace}).ProjectDir(wt)))

	out, _, err := zm(t, crewFile, "usage", "--json", "--since", "3650d")
	require.NoError(t, err)

	var rep usageReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	require.Len(t, rep.Rows, 1)
	assert.Equal(t, outsideRole, rep.Rows[0].Key)
	assert.Equal(t, 2, rep.Total.Turns, "each turn once")
}
