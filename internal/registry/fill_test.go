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

func TestToolKind(t *testing.T) {
	for cmd, want := range map[string]string{
		"cat main.go":                               "cat/sed/head/tail",
		"cd /w/acme && sed -n 1,20p x.go":           "cat/sed/head/tail",
		"command cat big.md":                        "cat/sed/head/tail",
		"GOOS=linux go build ./...":                 "go",
		"go test ./internal/...":                    "go test",
		"zmina --crew '/w/crew.yaml' task show T-1": "zmina task",
		"zmina read cmd/skill.md#Usage":             "zmina read",
		"~/.local/bin/zmina ack S-1":                "zmina ack",
		"rg -n Foo":                                 "grep/rg",
		"./deploy.sh":                               "other",
	} {
		assert.Equal(t, want, ToolKind(cmd), cmd)
	}
}

func TestFills(t *testing.T) {
	dir := t.TempDir()
	at := func(min int) string { return time.Date(2026, 10, 26, 9, min, 0, 0, time.UTC).Format(time.RFC3339) }
	use := func(min int, id, name, cmd string) string {
		return `{"type":"assistant","timestamp":"` + at(min) + `","message":{"content":[{"type":"tool_use","id":"` + id +
			`","name":"` + name + `","input":{"command":"` + cmd + `"}}]}}` + "\n"
	}
	result := func(min int, id, content string) string {
		return `{"type":"user","timestamp":"` + at(min) + `","message":{"content":[{"type":"tool_result","tool_use_id":"` +
			id + `","content":` + content + `}]}}` + "\n"
	}

	big := strings.Repeat("x", BigResult+1)
	body := use(1, "a", "Bash", "cat x.go") + result(1, "a", `"`+big+`"`) +
		use(2, "b", "Read", "") + result(2, "b", `[{"type":"text","text":"héllo"},{"type":"image","source":{"data":"`+
		strings.Repeat("A", 50_000)+`"}}]`) +
		use(3, "c", "Bash", "git log") + result(3, "c", `"abc"`) +
		use(0, "d", "Bash", "git status") + result(0, "d", `"before the window"`)

	path := filepath.Join(dir, "s.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s", "subagents"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s", "subagents", "a.jsonl"),
		[]byte(use(4, "e", "Grep", "")+result(4, "e", `"12345"`)), 0o600))

	got, err := Fills(Transcript{Path: path}, time.Date(2026, 10, 26, 9, 1, 0, 0, time.UTC), time.Time{})
	require.NoError(t, err)
	assert.Equal(t, map[string]Fill{
		"Bash: cat/sed/head/tail": {Tool: "Bash: cat/sed/head/tail", Calls: 1, Chars: BigResult + 1, Big: 1},
		"Read":                    {Tool: "Read", Calls: 1, Chars: 5, Images: 1},
		"Bash: git":               {Tool: "Bash: git", Calls: 1, Chars: 3},
		"Grep":                    {Tool: "Grep", Calls: 1, Chars: 5},
	}, got, "an image's base64 is no text; a subagent's tools count; before the window none")

	got, err = Fills(Transcript{Path: path}, time.Date(2026, 10, 26, 9, 1, 0, 0, time.UTC),
		time.Date(2026, 10, 26, 9, 3, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Len(t, got, 2, "until: the window's end")
}
