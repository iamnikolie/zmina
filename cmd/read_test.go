package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamnikolie/zmina/internal/lean"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zmina read and grep need no crew file, and a role's call is metered in its crew's reads.jsonl.
func TestReadAndGrepCommands(t *testing.T) {
	dir := t.TempDir()
	crewFile := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(crewFile, []byte("dir: "+dir+"\nroles:\n  dispatcher:\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\n\nalpha\n\n## B\n\nbeta\n"), 0o644))
	t.Chdir(dir)
	t.Setenv("ZMINA_ROLE", "dev")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-1")

	out, _, err := zm(t, crewFile, "read", "a.md#B")
	require.NoError(t, err)
	assert.Equal(t, "== a.md · 7 lines · 23 · markdown · 5-7 ## B\n5\t## B\n6\t\n7\tbeta\n", out)

	out, _, err = zm(t, crewFile, "grep", "-i", "BETA")
	require.NoError(t, err)
	assert.Contains(t, out, "a.md (1)\n  ## B · 5-7 (1)\n    7: beta\n")

	out, _, err = zm(t, crewFile, "grep", "-c", "alpha|beta", "a.md")
	require.NoError(t, err)
	assert.Equal(t, "a.md 2\n  1\t# A · 1-7\n  1\t## B · 5-7\n— 2 hits in 1 files\n", out)

	_, _, err = zm(t, crewFile, "read", "a.md", "--max", "lots")
	require.ErrorContains(t, err, "--max: characters")

	raw, err := os.ReadFile(filepath.Join(dir, "reads.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.Len(t, lines, 3)

	var r lean.Record
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &r))
	assert.Equal(t, "dev", r.Role)
	assert.Equal(t, "s-1", r.Session)
	assert.Equal(t, "read", r.Kind)
	assert.Equal(t, "symbol", r.What)
	assert.Equal(t, filepath.Join(dir, "a.md"), r.Path)
	assert.Equal(t, 23, r.Size)
}

// The guard answers only when it steps in: a rewrite keeps the rest of the tool input and leaves the permission
// decision to the usual rules (Codex: allow); an outline is a deny whose reason the model reads.
func TestReadGuardCommand(t *testing.T) {
	dir := t.TempDir()
	crewFile := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(crewFile, []byte("dir: "+dir+"\nroles:\n  dispatcher:\n"), 0o644))

	big := filepath.Join(dir, "big.md")
	require.NoError(t, os.WriteFile(big, []byte("# Big\n\n"+strings.Repeat("words words words words\n", 1000)), 0o644))

	guard := func(in map[string]any) string {
		raw, _ := json.Marshal(in)

		var out, errb bytes.Buffer

		root := NewRoot(&out, &errb)
		root.SetIn(bytes.NewReader(raw))
		root.SetArgs([]string{"--crew", crewFile, "_read-guard", "--role", "dev"})
		require.NoError(t, root.Execute())
		assert.Empty(t, errb.String())

		return out.String()
	}

	var got struct {
		Spec map[string]any `json:"hookSpecificOutput"`
	}

	out := guard(map[string]any{"session_id": "s-2", "cwd": dir, "permission_mode": "bypassPermissions", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "cat big.md", "description": "show it", "timeout": 5000.0}})
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	input := got.Spec["updatedInput"].(map[string]any)
	assert.Equal(t, "show it", input["description"], "the whole input, the rest unchanged")
	assert.InDelta(t, 5000, input["timeout"], 0)
	assert.Contains(t, input["command"], " read --via 'cat big.md' big.md")
	assert.NotContains(t, got.Spec, "permissionDecision", "the usual permission rules judge the new command")

	assert.Empty(t, guard(map[string]any{"cwd": dir, "permission_mode": "default", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "cat big.md"}}), "a rewritten cat would wait for approval")

	got.Spec = nil
	out = guard(map[string]any{"cwd": dir, "tool_name": "Read", "tool_input": map[string]any{"file_path": big}})
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "deny", got.Spec["permissionDecision"])
	assert.Contains(t, got.Spec["permissionDecisionReason"], "1-1002\t# Big")

	assert.Empty(t, guard(map[string]any{"cwd": dir, "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}}))

	var out2, errb bytes.Buffer

	root := NewRoot(&out2, &errb)
	root.SetIn(strings.NewReader("not json"))
	root.SetArgs([]string{"--crew", crewFile, "_read-guard"})
	require.NoError(t, root.Execute())
	assert.Empty(t, out2.String()+errb.String(), "any doubt: silent, the call goes as typed")

	raw, err := os.ReadFile(filepath.Join(dir, "reads.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(raw), "\n"), "each step the guard took")
	assert.Contains(t, string(raw), `"kind":"guard-rewrite","what":"cat"`)
	assert.Contains(t, string(raw), `"session":"s-2"`)
}

// The hook takes a file in the crew's worktrees, or in an active task's worktree elsewhere, as the role's own though
// its session started in another directory; a burst of reader calls in one session is marked bulk in the meter.
func TestReadGuardTaskWorktrees(t *testing.T) {
	dir := t.TempDir()
	crewFile := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(crewFile, []byte("dir: "+dir+"\nroles:\n  dispatcher:\n  dev:\n"), 0o644))

	body := []byte("package acme\n\n" + strings.Repeat("// words words words words\n", 1000))
	inCrew := filepath.Join(dir, "worktrees", "acme-T-1")
	require.NoError(t, os.MkdirAll(inCrew, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inCrew, "big.go"), body, 0o644))

	away := filepath.Join(t.TempDir(), "acme-T-2")
	require.NoError(t, os.MkdirAll(away, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(away, "big.go"), body, 0o644))

	_, _, err := zm(t, crewFile, "task", "new", "dev", "Fix the build")
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, "tasks", "T-1.json"))
	require.NoError(t, err)

	var tk map[string]any
	require.NoError(t, json.Unmarshal(raw, &tk))
	tk["worktree"] = away
	raw, err = json.Marshal(tk)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tasks", "T-1.json"), raw, 0o600))

	session := t.TempDir()

	guard := func(cmd string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "s-1", "cwd": session, "permission_mode": "bypassPermissions",
			"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}})

		var out, errb bytes.Buffer

		root := NewRoot(&out, &errb)
		root.SetIn(bytes.NewReader(in))
		root.SetArgs([]string{"--crew", crewFile, "_read-guard", "--role", "dev"})
		require.NoError(t, root.Execute())

		return out.String()
	}

	assert.Contains(t, guard("cd "+inCrew+" && cat big.go"), " read --via 'cat big.go' big.go", "the crew's worktrees")
	assert.Contains(t, guard("cd "+away+" && cat big.go"), " read --via 'cat big.go' big.go", "an active task's worktree")
	assert.Empty(t, guard("cd "+t.TempDir()+" && cat big.go"), "elsewhere: as it was")

	for range lean.BulkPerMinute {
		guard("cat " + filepath.Join(inCrew, "big.go"))
	}

	lines, err := os.ReadFile(filepath.Join(dir, "reads.jsonl"))
	require.NoError(t, err)

	all := strings.Split(strings.TrimSpace(string(lines)), "\n")
	assert.NotContains(t, all[lean.BulkPerMinute-1], `"bulk"`)
	assert.Contains(t, all[len(all)-1], `"bulk":true`, "past 30 a minute in one session")
}
