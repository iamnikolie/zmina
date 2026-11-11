package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/task"
)

const criteriaRuleText = "Done means every criterion below holds; in your result say for each whether it holds and how you checked."

func TestTaskNewSetShowCriteria(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	out, _, err := zm(t, file, "task", "new", "dev", "fix the login form",
		"--criteria", "the form shows an error, not a blank page", "--criteria", "tests pass, race detector on")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev\n", out, "the first line stays as scripts read it")

	shown, _, err := zm(t, file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "asked: fix the login form\n"+
		"criteria:\n"+
		"  1. the form shows an error, not a blank page\n"+
		"  2. tests pass, race detector on\n", "a comma stays inside its criterion")

	var got task.Task

	raw, _, err := zm(t, file, "task", "show", "1", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	assert.Equal(t, []string{"the form shows an error, not a blank page", "tests pass, race detector on"}, got.Criteria)

	// set replaces the list.
	_, _, err = zm(t, file, "task", "set", "1", "--criteria", "only this")
	require.NoError(t, err)
	assert.Equal(t, []any{"only this"}, readTaskFile(t, file, "T-1")["criteria"])

	// An empty value alone clears it, and the key leaves the file.
	_, _, err = zm(t, file, "task", "set", "1", "--criteria", "")
	require.NoError(t, err)
	assert.NotContains(t, readTaskFile(t, file, "T-1"), "criteria")

	shown, _, err = zm(t, file, "task", "show", "1")
	require.NoError(t, err)
	assert.NotContains(t, shown, "\ncriteria:\n", "no block for a task without criteria")

	// Nothing else about the task moved.
	assert.Equal(t, "fix the login form", readTaskFile(t, file, "T-1")["asked"])
}

func TestTaskCriteriaMustBeOneNonEmptyLine(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "task", "new", "dev", "x", "--criteria", "first\nsecond")
	require.ErrorContains(t, err, "one line each")

	_, _, err = zm(t, file, "task", "new", "dev", "x", "--criteria", "")
	require.ErrorContains(t, err, "not empty")

	out, _, err := zm(t, file, "task", "ls", "--all")
	require.NoError(t, err)
	assert.Equal(t, "no tasks\n", out, "a refused task is not recorded")

	_, _, err = zm(t, file, "task", "new", "dev", "x", "--criteria", "one")
	require.NoError(t, err)

	_, _, err = zm(t, file, "task", "set", "1", "--criteria", "a", "--criteria", "")
	require.ErrorContains(t, err, "an empty --criteria alone clears the list")
	assert.Equal(t, []any{"one"}, readTaskFile(t, file, "T-1")["criteria"], "a refused change leaves the list as it was")
}

func TestTaskNewRouteTakesCriteria(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	out, _, err := zm(t, file, "task", "new", "--route", "--kind", "code", "--criteria", "tests pass", "fix the login form")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev (rule 6: claude 7d 41% < codex week 50%)\n", out)

	rec := readTaskFile(t, file, "T-1")
	assert.Equal(t, []any{"tests pass"}, rec["criteria"])
	assert.Equal(t, "fix the login form", rec["asked"], "the flags are not part of the asked text")
	assert.NotNil(t, rec["route"])
}

func packetOf(t *testing.T, file, id string) handoff.Packet {
	t.Helper()

	raw, _, err := zm(t, file, "task", "handoff", id, "--json")
	require.NoError(t, err)

	var p handoff.Packet
	require.NoError(t, json.Unmarshal([]byte(raw), &p))

	return p
}

func TestHandoffPacketCarriesCriteriaOnlyWhenThereAreSome(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "task", "new", "dev", "plain")
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "new", "dev", "with", "--criteria", "tests pass", "--criteria", "docs say so")
	require.NoError(t, err)

	plain := packetOf(t, file, "1")
	assert.Empty(t, plain.Task.Criteria)
	assert.NotContains(t, plain.Instructions, "Done means")

	with := packetOf(t, file, "2")
	assert.Equal(t, []string{"tests pass", "docs say so"}, with.Task.Criteria)
	assert.Contains(t, with.Instructions, criteriaRuleText+"\n1. tests pass\n2. docs say so\n")

	// The packet file (not only --json) is the same JSON: the key is in the task object.
	path, _, err := zm(t, file, "task", "handoff", "2")
	require.NoError(t, err)

	body, err := os.ReadFile(strings.TrimSpace(path))
	require.NoError(t, err)
	assert.Contains(t, string(body), `"criteria": [`)

	// Clearing them gives the plain packet again.
	_, _, err = zm(t, file, "task", "set", "2", "--criteria", "")
	require.NoError(t, err)
	assert.NotContains(t, packetOf(t, file, "2").Instructions, "Done means")
}

const criteriaResult = `{"summary":"Done.","done":"all","remaining":"none","open_questions":"none","artifacts":"none","next_step":"none"}`

func TestTaskDoneWithCriteriaNeedsASavedResult(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	for range 4 {
		_, _, err := zm(t, file, "task", "new", "dev", "work", "--criteria", "tests pass")
		require.NoError(t, err)
	}

	state := func(id string) any { return readTaskFile(t, file, id)["state"] }

	// Refused, in one line that names the way out.
	for _, close := range [][]string{{"task", "done", "1"}, {"task", "set", "1", "done"}} {
		_, _, err := zm(t, file, close...)
		require.Error(t, err)
		assert.Equal(t, "T-1: has criteria and no saved result: save one with zmina task result T-1 --file <json> "+
			"(--force closes it anyway)", err.Error())
		assert.NotContains(t, err.Error(), "\n")
		assert.Equal(t, "open", state("T-1"))
	}

	// A drop gives the work up and needs no result.
	_, _, err := zm(t, file, "task", "drop", "1")
	require.NoError(t, err)
	assert.Equal(t, "dropped", state("T-1"))

	// --force closes it anyway.
	_, _, err = zm(t, file, "task", "done", "2", "--force")
	require.NoError(t, err)
	assert.Equal(t, "done", state("T-2"))

	// A saved result lets it close.
	body := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(body, []byte(criteriaResult), 0o600))

	_, _, err = zm(t, file, "task", "result", "3", "--file", body)
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "done", "3")
	require.NoError(t, err)
	assert.Equal(t, "done", state("T-3"))

	// done --result saves it first.
	_, _, err = zm(t, file, "task", "done", "4", "--result", body)
	require.NoError(t, err)
	assert.Equal(t, "done", state("T-4"))
}

func TestTaskDoneWithoutCriteriaClosesAsBefore(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "task", "new", "dev", "work")
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "done", "1")
	require.NoError(t, err)
	assert.Equal(t, "done", readTaskFile(t, file, "T-1")["state"])

	// Criteria cleared: closing is as before again.
	_, _, err = zm(t, file, "task", "new", "dev", "work", "--criteria", "tests pass")
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "set", "2", "--criteria", "")
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "done", "2")
	require.NoError(t, err)
	assert.Equal(t, "done", readTaskFile(t, file, "T-2")["state"])
}
