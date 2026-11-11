package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
)

func crewChanged(t *testing.T, crewFile string) []events.Event {
	t.Helper()

	evs, err := (&events.Log{Path: filepath.Join(filepath.Dir(crewFile), "events.jsonl")}).Read(events.Filter{})
	require.NoError(t, err)

	var out []events.Event

	for _, e := range evs {
		if e.Event == "crew-changed" {
			out = append(out, e)
		}
	}

	return out
}

// A seat edit keeps the file it replaced and logs which role and key changed, by which command; the single .bak is gone.
func TestSeatEditGoesThroughTheHistory(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n")

	out, _, err := zm(t, crewFile, "seat", "set", "dev", "model=sonnet", "about=code")
	require.NoError(t, err)
	assert.Contains(t, out, "zmina crew history")
	assert.NoFileExists(t, crewFile+".bak")

	evs := crewChanged(t, crewFile)
	require.Len(t, evs, 1, "the first look at the file is a start, not a change")
	assert.Equal(t, "dev", evs[0].Role)
	assert.Equal(t, "hand", evs[0].By)
	assert.Equal(t, "dev: about, model (zmina seat set)", evs[0].Detail)

	out, _, err = zm(t, crewFile, "crew", "history")
	require.NoError(t, err)
	assert.Contains(t, out, "WHAT CHANGED")
	assert.Contains(t, out, "dev: about, model · zmina seat set  (the file now)")
	assert.Contains(t, out, "first seen")

	_, _, err = zm(t, crewFile, "seat", "add", "scribe", "--about", "notes")
	require.NoError(t, err)
	_, _, err = zm(t, crewFile, "seat", "rm", "scribe")
	require.NoError(t, err)

	evs = crewChanged(t, crewFile)
	require.Len(t, evs, 3)
	assert.Equal(t, "added scribe (zmina seat add)", evs[1].Detail)
	assert.Equal(t, "removed scribe (zmina seat rm)", evs[2].Detail)
}

// A role editing the crew file with its own tool is no seat command: the next command that loads the file records it.
func TestAnEditByHandIsNoticedOnTheNextCommand(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Empty(t, crewChanged(t, crewFile))

	raw, err := os.ReadFile(crewFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(crewFile, []byte(strings.Replace(string(raw), "  dev: {}", "  dev:\n    model: opus", 1)), 0o600))

	_, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)

	evs := crewChanged(t, crewFile)
	require.Len(t, evs, 1)
	assert.Equal(t, "dev", evs[0].Role)
	assert.Empty(t, evs[0].By, "who edited it is not known")
	assert.Equal(t, "dev: model (edited by hand)", evs[0].Detail)

	// Another command on the same file adds nothing.
	_, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Len(t, crewChanged(t, crewFile), 1)

	out, _, err := zm(t, crewFile, "crew", "history", "--json")
	require.NoError(t, err)

	var rows []struct {
		N   int    `json:"n"`
		How string `json:"how"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 2)
	assert.Equal(t, 0, rows[0].N)
	assert.Equal(t, crew.HowHand, rows[0].How)
	assert.Equal(t, crew.HowFirst, rows[1].How)
}

// --diff shows a version against the file now; --restore puts one back, as a change of its own, even when the file now
// no longer loads.
func TestCrewHistoryDiffAndRestore(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n")
	before, err := os.ReadFile(crewFile)
	require.NoError(t, err)

	_, _, err = zm(t, crewFile, "seat", "set", "dev", "model=sonnet")
	require.NoError(t, err)

	out, _, err := zm(t, crewFile, "crew", "history", "--diff", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "--- version 1 (")
	assert.Contains(t, out, "+++ now (crew.yaml)")
	assert.Contains(t, out, "-  dev: {}\n+  dev: {model: sonnet}\n")

	out, _, err = zm(t, crewFile, "crew", "history", "--diff", "0")
	require.NoError(t, err)
	assert.Equal(t, "version 0 is the file as it is now\n", out)

	_, _, err = zm(t, crewFile, "crew", "history", "--diff", "9")
	require.ErrorContains(t, err, "no version 9")

	_, _, err = zm(t, crewFile, "crew", "history", "--diff", "1", "--restore", "1")
	require.Error(t, err)

	// Someone breaks the file by hand.
	require.NoError(t, os.WriteFile(crewFile, []byte("roles:\n  dev:\n    keep: sometimes\n"), 0o600))

	_, _, err = zm(t, crewFile, "ls")
	require.Error(t, err, "a crew that does not load stops the other commands")

	out, _, err = zm(t, crewFile, "crew", "history")
	require.NoError(t, err, "the history of a file that does not load is still readable")
	assert.Contains(t, out, "edited by hand")

	_, _, err = zm(t, crewFile, "crew", "history", "--restore", "0")
	require.ErrorContains(t, err, "0 is the file as it is now")

	// Version 2 is the file as it was before the seat edit; version 1 is the seat edit.
	out, _, err = zm(t, crewFile, "crew", "history", "--restore", "2")
	require.NoError(t, err)
	assert.Contains(t, out, "restored version 2")

	got, err := os.ReadFile(crewFile)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(got))

	_, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err, "it loads again")

	evs := crewChanged(t, crewFile)
	require.NotEmpty(t, evs)
	assert.Contains(t, evs[len(evs)-1].Detail, "(zmina crew history --restore 2)")
	assert.Equal(t, "hand", evs[len(evs)-1].By)

	// The restore is a version: the broken file it replaced is still there to look at.
	out, _, err = zm(t, crewFile, "crew", "history")
	require.NoError(t, err)
	assert.Contains(t, out, "zmina crew history --restore 2  (the file now)")
}

// A version that no longer loads is not put back.
func TestCrewHistoryRestoreRefusesAFileThatDoesNotLoad(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, crewFile, "ls")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(crewFile, []byte("roles:\n  dev:\n    keep: sometimes\n"), 0o600))
	_, _, err = zm(t, crewFile, "crew", "history")
	require.NoError(t, err)

	// The broken file is the newest version; a good one replaces it, then the broken one is version 1.
	_, _, err = zm(t, crewFile, "crew", "history", "--restore", "1")
	require.NoError(t, err)
	_, _, err = zm(t, crewFile, "crew", "history", "--restore", "1")
	require.ErrorContains(t, err, "would not load")

	got, err := os.ReadFile(crewFile)
	require.NoError(t, err)
	assert.NotContains(t, string(got), "sometimes")
}
