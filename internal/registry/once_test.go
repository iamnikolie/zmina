package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onceBody is a transcript of a session named alpha: for each id a reply with its tokens and a Bash call of its own,
// and the call's result.
func onceBody(ids ...string) string {
	out := []string{`{"type":"agent-name","agentName":"alpha"}`}

	for _, id := range ids {
		out = append(out,
			`{"type":"assistant","timestamp":"2026-10-26T12:00:00Z","message":{"id":"`+id+`","model":"claude-opus-5-5",`+
				`"usage":{"input_tokens":2,"cache_read_input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use",`+
				`"id":"call-`+id+`","name":"Bash","input":{"command":"git log"}}]}}`,
			`{"type":"user","timestamp":"2026-10-26T12:00:01Z","message":{"content":[{"type":"tool_result",`+
				`"tool_use_id":"call-`+id+`","content":"ok"}]}}`)
	}

	return strings.Join(out, "\n") + "\n"
}

func writeOnce(t *testing.T, path, body string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// A session's file is listed once however many names reach it, and a session lying in two files is listed in both:
// the replies are what is read once (TestMessagesOnce).
func TestTranscriptsListEachFileOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, projects string)
		want  []string
	}{
		{"one project directory", func(t *testing.T, projects string) {}, []string{"s1"}},
		{"project directory linked under a second name", func(t *testing.T, projects string) {
			require.NoError(t, os.Symlink("-a", filepath.Join(projects, "-b")))
		}, []string{"s1"}},
		{"file linked into a second project directory", func(t *testing.T, projects string) {
			require.NoError(t, os.MkdirAll(filepath.Join(projects, "-b"), 0o700))
			require.NoError(t, os.Symlink(filepath.Join("..", "-a", "s1.jsonl"), filepath.Join(projects, "-b", "s1.jsonl")))
		}, []string{"s1"}},
		{"the same session copied into a second project directory", func(t *testing.T, projects string) {
			writeOnce(t, filepath.Join(projects, "-b", "s1.jsonl"), onceBody("m1"))
		}, []string{"s1", "s1"}},
		{"another session in a second project directory", func(t *testing.T, projects string) {
			writeOnce(t, filepath.Join(projects, "-b", "s2.jsonl"), onceBody("m2"))
		}, []string{"s1", "s2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := t.TempDir()
			projects := filepath.Join(ns, "projects")
			writeOnce(t, filepath.Join(projects, "-a", "s1.jsonl"), onceBody("m1"))
			tc.setup(t, projects)

			ts, err := (&Registry{Namespace: ns}).Transcripts(time.Time{})
			require.NoError(t, err)

			var ids []string
			for _, tr := range ts {
				ids = append(ids, tr.ID)
			}

			assert.Equal(t, tc.want, ids)
		})
	}
}

// A reply, and a tool call, is counted once across the files of a read that shares seen, whichever file is behind.
func TestMessagesAndFillsOnce(t *testing.T) {
	for _, tc := range []struct {
		name         string
		first, other []string
		want         []string
	}{
		{"a copy of the same replies", []string{"m1", "m2"}, []string{"m1", "m2"}, []string{"m1", "m2"}},
		{"a copy that is behind", []string{"m1", "m2"}, []string{"m1"}, []string{"m1", "m2"}},
		{"a copy that is ahead", []string{"m1"}, []string{"m1", "m2"}, []string{"m1", "m2"}},
		{"another session's replies", []string{"m1"}, []string{"m2"}, []string{"m1", "m2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a, b := filepath.Join(dir, "-a", "s1.jsonl"), filepath.Join(dir, "-b", "s1.jsonl")
			writeOnce(t, a, onceBody(tc.first...))
			writeOnce(t, b, onceBody(tc.other...))

			replies, calls := map[string]bool{}, map[string]bool{}

			var ids []string

			fills := Fill{}

			for _, p := range []string{a, b} {
				ms, err := MessagesOnce(Transcript{Path: p}, time.Time{}, replies)
				require.NoError(t, err)

				for _, m := range ms {
					ids = append(ids, m.ID)
				}

				got, err := FillsOnce(Transcript{Path: p}, time.Time{}, time.Time{}, calls)
				require.NoError(t, err)

				fills.Add(got["Bash: git"])
			}

			assert.ElementsMatch(t, tc.want, ids)
			assert.Equal(t, len(tc.want), fills.Calls, "each call once")
			assert.Equal(t, 2*len(tc.want), fills.Chars, "each result once")
		})
	}
}
