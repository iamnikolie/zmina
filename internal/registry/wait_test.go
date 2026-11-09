package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitCall(t *testing.T) {
	for cmd, want := range map[string]bool{
		"sleep 60; gl mr show 35":                   true,
		"until test -f /tmp/done; do sleep 5; done": true,
		"while pgrep -f build; do sleep 10; done":   true,
		"kubectl rollout status deploy/api -n dev":  true,
		"gh run watch 123":                          true,
		"go test ./...":                             false,
		"grep -n 'sleep' main.go":                   false,
		"cat <<EOF\nwhile the tests run\nEOF":       false,
		"for f in *.go; do gofmt -l $f; done":       false,
		"timeout 30 curl -s localhost:8080/health":  false,
	} {
		got, _ := WaitCall(cmd)
		assert.Equal(t, want, got, cmd)
	}

	_, check := WaitCall("gl pipeline  show 42")
	assert.Equal(t, "gl pipeline show 42", check, "a status check, spaces folded")
	_, check = WaitCall("git status")
	assert.Empty(t, check)
}

func TestMessagesMarkWaits(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	n := 0
	reply := func(id, calls string) string {
		n++
		content := `[{"type":"text","text":"ok"}]`
		if calls != "" {
			content = "[" + calls + "]"
		}

		return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-11-09T10:%02d:00Z","message":{"id":%q,"model":"claude-opus-5-5",`+
			`"content":%s,"usage":{"input_tokens":1,"cache_read_input_tokens":100000,"output_tokens":10}}}`, n, id, content)
	}
	bash := func(cmd string, bg bool) string {
		return fmt.Sprintf(`{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":%q,"run_in_background":%t}}`, n, cmd, bg)
	}
	result := `{"type":"user","timestamp":"2026-11-09T10:00:30Z","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`

	lines := []string{
		`{"type":"user","timestamp":"2026-11-09T10:00:00Z","message":{"role":"user","content":"deploy it"}}`,
		reply("a1", ""),
		reply("a1", bash("sleep 120", false)), // the same reply's second line makes the wait
		result,
		reply("a2", bash("gl pipeline show 42", false)), // after a sleep: a wait turn; one look is no poll
		result,
		reply("a3", bash("gl pipeline show 42", false)), // the same look again: a poll
		result,
		reply("a4", bash("until gl pipeline show 42 | grep -q passed; do sleep 30; done", true)), // background
		result,
		reply("a5", ""),
		reply("a6", bash("go test ./...", false)),
		result,
		reply("a7", ""),
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	ms, err := Messages(Transcript{Path: filepath.Join(dir, "s1.jsonl")}, time.Time{})
	require.NoError(t, err)

	var waits []string
	for _, m := range ms {
		if m.Wait {
			waits = append(waits, m.ID)
		}
	}

	assert.Equal(t, []string{"a2", "a4"}, waits, "after the sleep and after the repeated check; never the background wait")
}
