package cmd

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/tmux"
)

func reconcileChecks(t *testing.T, key string, fn func(c *crew.Crew, add func(name, level, format string, args ...any))) []string {
	t.Helper()

	var got []string

	fn(mustCrew(t, testCrew(t, assignSeats+key)), func(name, level, format string, args ...any) {
		got = append(got, name+" "+level+" "+fmt.Sprintf(format, args...))
	})

	return got
}

// Doctor says which parts of reconcile are on and what each does, and nothing when it is off.
func TestDoctorSaysWhatReconcileDoes(t *testing.T) {
	got := reconcileChecks(t, "reconcile: on\n", checkReconcile)
	require.Len(t, got, 1)
	assert.True(t, strings.HasPrefix(got[0], "reconcile ok reconcile: assign (a task, or a line from the owner or the dispatcher, starts a seat that is down)"), got[0])
	assert.Contains(t, got[0], " · restart (a seat that holds an open task is started again; 3 failed starts in a row mark it broken)")
	assert.Contains(t, got[0], " · orphans: down (a session no role names is reported once, and taken down when nothing says it is used)")

	got = reconcileChecks(t, "reconcile: {restart: true, tries: 5, orphans: report}\n", checkReconcile)
	require.Len(t, got, 1)
	assert.NotContains(t, got[0], "assign")
	assert.Contains(t, got[0], "5 failed starts in a row")
	assert.Contains(t, got[0], "orphans: report (a session no role names is reported once)")

	assert.Empty(t, reconcileChecks(t, "", checkReconcile))
	assert.Empty(t, reconcileChecks(t, "reconcile: off\n", checkReconcile), "off says nothing")
	assert.Empty(t, reconcileChecks(t, "reconcile: {tries: 4}\n", checkReconcile), "a block that names no part is off")
}

// A spare crew has no ticker by design: restart and orphans never act there, and doctor says so. Assign, which acts when a
// task is given, is no reason to warn.
func TestDoctorWarnsOfReconcileInASpareCrew(t *testing.T) {
	got := reconcileChecks(t, "spare: true\nreconcile: on\n", checkReconcile)
	require.Len(t, got, 2)
	assert.Contains(t, got[1], "reconcile warn this is a spare crew (spare: true): it has no ticker by design, and restart and orphans are the ticker's")

	got = reconcileChecks(t, "spare: true\nreconcile: {assign: true}\n", checkReconcile)
	assert.Len(t, got, 1, "assign acts in a command: no warning")

	got = reconcileChecks(t, "reconcile: on\n", checkReconcile)
	assert.Len(t, got, 1, "a crew that is not spare")
}

// Each live session no role names is a warning, with orphans on; none with it off.
func TestDoctorWarnsOfEachLiveOrphan(t *testing.T) {
	file := testCrew(t, assignSeats+"reconcile: {orphans: down}\n")
	c := mustCrew(t, file)

	list := []tmux.SessionInfo{{Name: "dev", Tagged: true}, {Name: "dev#2", Tagged: true}, {Name: "ghost", Tagged: true}, {Name: "by-hand", Tagged: false}}

	var got []string

	add := func(name, level, format string, args ...any) {
		got = append(got, name+" "+level+" "+fmt.Sprintf(format, args...))
	}

	checkOrphans(c, nil, list, add)
	assert.Equal(t, []string{
		"orphans warn ghost: a tmux session no role of the crew file names (started by zmina; taken down by the ticker once it is idle with nothing queued and no terminal attached): zmina attach ghost",
		"orphans warn by-hand: a tmux session no role of the crew file names (not started by zmina: left alone; reported by the ticker, once): zmina attach by-hand",
	}, got, "dev is a role and dev#2 a copy of one")

	got = nil

	checkOrphans(mustCrew(t, testCrew(t, assignSeats+"reconcile: {orphans: report}\n")), nil, list[2:3], add)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "(started by zmina; reported by the ticker, once)")

	got = nil

	checkOrphans(mustCrew(t, testCrew(t, assignSeats)), nil, list, add)
	checkOrphans(mustCrew(t, testCrew(t, assignSeats+"reconcile: {orphans: off, restart: true}\n")), nil, list, add)
	assert.Empty(t, got, "orphans off: nothing")
}

// doctor, run, lists the orphan a real tmux server holds, with the key on, and says nothing of it with the key off.
func TestDoctorListsAnOrphanOfTheCrewsTmuxServer(t *testing.T) {
	for key, want := range map[string]bool{"reconcile: {orphans: report}\n": true, "": false} {
		file := testCrew(t, assignSeats+key)
		fakeAuth(t, filepath.Dir(file), `{"loggedIn": true}`)

		c := mustCrew(t, file)

		tm, err := tmux.New(c.Socket())
		if err != nil {
			t.Skip(err)
		}

		require.NoError(t, tm.Start("ghost", t.TempDir(), nil, []string{"cat"}))

		out, _, _ := zm(t, file, "doctor")
		assert.Equal(t, want, regexp.MustCompile(`warn\s+orphans\s+ghost: a tmux session no role of the crew file names`).MatchString(out), "%q: %s", key, out)
	}
}

// The ticker's beat, run for real, finds the session of its crew's tmux server that no role names and reports it once.
func TestTickReportsAnOrphanOfTheCrewsTmuxServerOnce(t *testing.T) {
	file := testCrew(t, assignSeats+"reconcile: {orphans: report}\n")
	c := mustCrew(t, file)

	tm, err := tmux.New(c.Socket())
	if err != nil {
		t.Skip(err)
	}

	require.NoError(t, tm.Start("ghost", t.TempDir(), nil, []string{"cat"}))

	out, _, err := zm(t, file, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "orphan session ghost")
	assert.True(t, tm.Has("ghost"), "reported, not touched")

	out, _, err = zm(t, file, "tick")
	require.NoError(t, err)
	assert.NotContains(t, out, "orphan session ghost", "once")

	// With the key off the beat does not look at all.
	off := testCrew(t, assignSeats)

	tm2, err := tmux.New(mustCrew(t, off).Socket())
	require.NoError(t, err)
	require.NoError(t, tm2.Start("ghost", t.TempDir(), nil, []string{"cat"}))

	out, _, err = zm(t, off, "tick")
	require.NoError(t, err)
	assert.NotContains(t, out, "orphan")
}
