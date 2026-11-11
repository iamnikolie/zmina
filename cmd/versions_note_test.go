package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// The dispatcher hears once a day, for each thing that runs the code of an earlier zmina, that it needs a restart:
// the cockpit and a Codex role's bridge. Claude roles are only listed by doctor, and a skew that is gone is forgotten.
func TestVersionBeatTellsTheDispatcherOnceADayPerSkew(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n    boot: hi\n  rev:\n    runtime: codex\n    boot: hi\n")
	dir := filepath.Dir(crewFile)

	keep := version
	version = "v1.1.0"
	t.Cleanup(func() { version = keep })

	var printed []string

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	state := func() *shift.State {
		st, err := shift.Read(dir)
		require.NoError(t, err)

		return st
	}
	set := func(fn func(st *shift.State)) {
		require.NoError(t, a.withState(func(s *shift.Shift) error { fn(s.State); return nil }))
	}
	clearNotes := func() { set(func(st *shift.State) { st.Notes, st.NoticeMeta = nil, nil }) }
	beat := func(now time.Time) []string {
		clearNotes()
		a.out = writerFunc(func(p []byte) { printed = append(printed, string(p)) })
		printed = nil
		a.versionBeat(now)

		return state().Notes
	}

	day := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	// Everything current, or started by an earlier zmina whose hooks only keep command lines: nothing to tell.
	set(func(st *shift.State) {
		st.Role("dispatcher").Up, st.Role("dispatcher").Version = true, "v1.1.0"
		st.Role("dev").Up, st.Role("dev").Version = true, "v1.0.0"
	})
	assert.Empty(t, beat(day), "a Claude role of an earlier zmina is for doctor to list")
	assert.NoFileExists(t, versionToldPath(dir))

	// A Codex bridge running an earlier zmina's code.
	set(func(st *shift.State) { st.Role("rev").Up, st.Role("rev").Version = true, "v1.0.0" })

	notes := beat(day)
	require.Len(t, notes, 1)
	assert.Equal(t, "version: the Codex bridge of rev runs the code of zmina v1.0.0, this zmina is v1.1.0: "+
		"zmina restart rev --continue starts it with this one", notes[0])
	assert.Len(t, printed, 1, "the ticker's line shows it too")

	events, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(events), `"event":"version-skew"`)

	assert.Empty(t, beat(day.Add(time.Minute)), "the next beat says nothing")
	assert.Empty(t, beat(day.Add(23*time.Hour)), "nor in the same day")

	notes = beat(day.Add(24 * time.Hour))
	require.Len(t, notes, 1, "a day later it is told again while it lasts")

	// Another skew is told at once and does not bring the first back.
	raw, err := json.Marshal(uiRunning{URL: "http://127.0.0.1:7517/", PID: os.Getpid(), Handoff: "h", Version: "v1.0.0"})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(uiStatePath()), 0o700))
	require.NoError(t, os.WriteFile(uiStatePath(), raw, 0o600))

	notes = beat(day.Add(25 * time.Hour))
	require.Len(t, notes, 1)
	assert.Equal(t, "version: zmina ui (pid "+strconv.Itoa(os.Getpid())+") runs v1.0.0, this zmina is v1.1.0: stop it "+
		"and run zmina ui again to serve this one", notes[0])

	// Restarted: the skew is forgotten, and one that comes back is told again at once.
	set(func(st *shift.State) { st.Role("rev").Version = "v1.1.0" })
	require.NoError(t, os.Remove(uiStatePath()))
	assert.Empty(t, beat(day.Add(26*time.Hour)))

	told := readVersionTold(dir)
	assert.Empty(t, told, "what is gone is no longer kept")

	set(func(st *shift.State) { st.Role("rev").Version = "v1.0.0" })
	assert.Len(t, beat(day.Add(27*time.Hour)), 1)
}

// A beat that cannot record that it told says nothing, and a missing or unreadable record is a first time.
func TestVersionBeatWithoutARecord(t *testing.T) {
	crewFile := testCrew(t, "  rev:\n    runtime: codex\n    boot: hi\n")
	dir := filepath.Dir(crewFile)

	keep := version
	version = "v1.1.0"
	t.Cleanup(func() { version = keep })

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		s.State.Role("rev").Up, s.State.Role("rev").Version = true, "v1.0.0"

		return nil
	}))

	require.NoError(t, os.WriteFile(versionToldPath(dir), []byte("not json"), 0o600))
	a.versionBeat(time.Now())

	st, err := shift.Read(dir)
	require.NoError(t, err)
	assert.Len(t, st.Notes, 1, "an unreadable record is no record")

	// The record cannot be written: a directory stands where the file goes.
	require.NoError(t, os.Remove(versionToldPath(dir)))
	require.NoError(t, os.Mkdir(versionToldPath(dir), 0o700))
	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Notes, s.State.NoticeMeta = nil, nil; return nil }))

	a.versionBeat(time.Now())

	st, err = shift.Read(dir)
	require.NoError(t, err)
	assert.Empty(t, st.Notes, "said nothing: it could not be recorded, so it would be said every minute")
}

// writerFunc is an io.Writer that hands each write to a function.
type writerFunc func(p []byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }
