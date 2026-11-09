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
		"sleep 1m":                                  true,
		"sleep 2 && curl localhost:8080":            false,
		"sleep 3; oko shot":                         false,
		"until test -f /tmp/done; do sleep 5; done": true,
		"while pgrep -f build; do sleep 10; done":   true,
		"while read f; do wc -l $f; done < files":   false,
		"kubectl rollout status deploy/api -n dev":  true,
		"gh run watch 123":                          true,
		"gl pipeline show 42":                       false,
		"go test ./...":                             false,
		"grep -n 'sleep 60' main.go":                true, // a sleep word in an argument reads as one: rare, kept simple
		"python3 - <<'EOF'\nimport time\ntime.sleep(60)\nos.system('sleep 60')\nEOF": false,
		"cat > a.sh <<EOF\nwhile true; do sleep 30; done\nEOF\nsleep 25":             true,
		"for f in *.go; do gofmt -l $f; done":                                        false,
	} {
		assert.Equal(t, want, WaitCall(cmd), cmd)
	}
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
		reply("a1", bash("sleep 120", false)), // the same reply's second line makes the first wait
		result,
		reply("a2", bash("zmina peek dev", false)), // its look: what a background wait costs too
		result,
		reply("a3", bash("sleep 60", false)), // the second wait of the chain
		result,
		reply("a4", bash("gl pipeline show 42", false)), // waste: after the second wait
		result,
		reply("a5", bash("go test ./...", false)), // waste: after its look
		result,
		reply("a6", bash("git diff", false)),
		result,
		reply("a6b", bash("git log -1", false)),
		result,
		reply("a6c", bash("git status", false)), // four calls that are not waits: work again, the chain is over
		result,
		reply("a7", bash("sleep 30", false)), // a new wait after work: a first one
		result,
		reply("a8", bash("gl pipeline show 42", false)),
		result,
		// The owner writes: a new prompt ends the chain.
		`{"type":"user","timestamp":"2026-11-09T11:00:00Z","message":{"role":"user","content":"push it again"}}`,
		reply("a9", bash("sleep 45", false)),
		result,
		reply("a10", bash("until gl pipeline show 43 | grep -q passed; do sleep 30; done", true)), // background
		result,
		reply("a11", bash("sleep 3; oko shot", false)), // a settle pause
		result,
		reply("a12", ""),
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

	assert.Equal(t, []string{"a4", "a5"}, waits,
		"after the second wait of a chain and after its look; work, the owner's line or a background wait ends it")
}
