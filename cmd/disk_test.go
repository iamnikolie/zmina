package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

const (
	mib = uint64(1) << 20
	gib = uint64(1) << 30
)

// The line a filesystem crosses is the smaller of an amount and a share of its size, and it is crossed by being under
// it, not at it.
func TestDiskLevel(t *testing.T) {
	cases := map[string]struct {
		free, total uint64
		want        string
	}{
		"plenty on a big disk":             {free: 200 * gib, total: 1000 * gib, want: "ok"},
		"a big disk 90% full is not low":   {free: 100 * gib, total: 1000 * gib, want: "ok"},
		"at the warn amount":               {free: 5 * gib, total: 1000 * gib, want: "ok"},
		"a byte under the warn amount":     {free: 5*gib - 1, total: 1000 * gib, want: "warn"},
		"between the two lines":            {free: 3 * gib, total: 1000 * gib, want: "warn"},
		"at the fail amount":               {free: 1 * gib, total: 1000 * gib, want: "warn"},
		"a byte under the fail amount":     {free: 1*gib - 1, total: 1000 * gib, want: "fail"},
		"a few MiB left":                   {free: 40 * mib, total: 1000 * gib, want: "fail"},
		"nothing left":                     {free: 0, total: 1000 * gib, want: "fail"},
		"small disk, share is the line":    {free: 3 * gib, total: 20 * gib, want: "ok"},
		"small disk, under the warn share": {free: 1900 * mib, total: 20 * gib, want: "warn"},
		"small disk, at the fail share":    {free: 20 * gib / 50, total: 20 * gib, want: "warn"},
		"small disk, under the fail share": {free: 20*gib/50 - mib, total: 20 * gib, want: "fail"},
		"a 4 GiB volume is not low at 1G":  {free: 1 * gib, total: 4 * gib, want: "ok"},
		"size not known, amounts only":     {free: 3 * gib, total: 0, want: "warn"},
		"size not known, plenty":           {free: 50 * gib, total: 0, want: "ok"},
		"size not known, nothing":          {free: 0, total: 0, want: "fail"},
		"more free than the size says":     {free: 10 * gib, total: 1 * gib, want: "ok"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, diskLevel(c.free, c.total))
		})
	}
}

func TestDiskLimits(t *testing.T) {
	warn, fail := diskLimits(1000 * gib)
	assert.Equal(t, 5*gib, warn)
	assert.Equal(t, 1*gib, fail)

	warn, fail = diskLimits(20 * gib)
	assert.Equal(t, 2*gib, warn, "10% of 20 GiB")
	assert.Equal(t, 20*gib/50, fail, "2% of 20 GiB")

	warn, fail = diskLimits(0)
	assert.Equal(t, 5*gib, warn)
	assert.Equal(t, 1*gib, fail)
}

func TestDiskSize(t *testing.T) {
	cases := map[uint64]string{
		0:                     "0 B",
		512:                   "512 B",
		1024:                  "1.0 KiB",
		300 * mib:             "300 MiB",
		640*mib + 512*1024:    "640 MiB",
		gib:                   "1.0 GiB",
		gib + gib/4:           "1.2 GiB",
		5 * gib:               "5.0 GiB",
		465*gib + gib/2:       "466 GiB",
		2 * (uint64(1) << 40): "2.0 TiB",
		1000 * gib:            "1000 GiB",
		uint64(1)<<40 + 1<<39: "1.5 TiB",
		99*mib + mib/2:        "99.5 MiB",
		512 * 1024:            "512 KiB",
	}

	for n, want := range cases {
		assert.Equal(t, want, diskSize(n), "%d bytes", n)
	}
}

// fakeDisks is a diskFree that answers by path, and the paths it was asked about.
func fakeDisks(t *testing.T, by map[string]diskSpace) *[]string {
	t.Helper()

	var asked []string

	keep := diskFree
	diskFree = func(path string) (diskSpace, error) {
		asked = append(asked, path)

		sp, ok := by[path]
		if !ok {
			return diskSpace{}, errors.New("no such filesystem")
		}

		return sp, nil
	}
	t.Cleanup(func() { diskFree = keep })

	return &asked
}

func diskCrew(t *testing.T, roles string) (*crew.Crew, string) {
	t.Helper()

	crewFile := testCrew(t, roles)
	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	return c, crewFile
}

// doctor says the free space of the crew's directory and of the Claude Code config, as one line when one filesystem
// holds both.
func TestCheckDiskReadsEachFilesystemOnce(t *testing.T) {
	c, _ := diskCrew(t, "")
	home, ns := c.StateDir(), c.Namespace

	lines := func() []string {
		var out []string

		checkDisk(c, func(name, level, format string, args ...any) {
			assert.Equal(t, "disk", name)
			out = append(out, level+" "+fmt.Sprintf(format, args...))
		})

		return out
	}

	// One filesystem under both.
	fakeDisks(t, map[string]diskSpace{
		home: {Free: 120 * gib, Total: 500 * gib, ID: "a"},
		ns:   {Free: 120 * gib, Total: 500 * gib, ID: "a"},
	})
	got := lines()
	require.Len(t, got, 1)
	assert.Equal(t, "ok "+home+" (zmina home and Claude Code config "+ns+"): 120 GiB free of 500 GiB", got[0])

	// Two filesystems, the Claude one low.
	fakeDisks(t, map[string]diskSpace{
		home: {Free: 120 * gib, Total: 500 * gib, ID: "a"},
		ns:   {Free: 3 * gib, Total: 500 * gib, ID: "b"},
	})
	got = lines()
	require.Len(t, got, 2)
	assert.Equal(t, "ok "+home+" (zmina home): 120 GiB free of 500 GiB", got[0])
	assert.Equal(t, "warn "+ns+" (Claude Code config): 3.0 GiB free of 500 GiB · under 5.0 GiB: free some space before "+
		"it fills", got[1])

	// Under the fail line it says what follows.
	fakeDisks(t, map[string]diskSpace{
		home: {Free: 640 * mib, Total: 500 * gib, ID: "a"},
		ns:   {Free: 640 * mib, Total: 500 * gib, ID: "a"},
	})
	got = lines()
	require.Len(t, got, 1)
	assert.Equal(t, "fail "+home+" (zmina home and Claude Code config "+ns+"): 640 MiB free of 500 GiB · under 1.0 GiB: "+
		"files written now can come out empty or cut short (a role's session file, the state); free space, then restart "+
		"a role that shows up unknown", got[0])

	// A filesystem that cannot be read is a warning, not a fail, and not a silent skip.
	fakeDisks(t, map[string]diskSpace{home: {Free: 120 * gib, Total: 500 * gib, ID: "a"}})
	got = lines()
	require.Len(t, got, 2)
	assert.Contains(t, got[1], "warn cannot read the free space under "+ns)
}

// A crew of Codex roles has no Claude Code config to look at.
func TestCheckDiskSkipsTheClaudeConfigWithoutClaudeRoles(t *testing.T) {
	c, _ := diskCrew(t, "")
	c.Roles = map[string]*crew.Role{"dev": {Name: "dev", Runtime: "codex"}}

	asked := fakeDisks(t, map[string]diskSpace{c.StateDir(): {Free: 120 * gib, Total: 500 * gib}})

	checkDisk(c, func(string, string, string, ...any) {})
	assert.Equal(t, []string{c.StateDir()}, *asked)
}

// zmina doctor shows the disk line in its place and a fail exits 1.
func TestDoctorDiskLine(t *testing.T) {
	crewFile := testCrew(t, "")
	fakeAuth(t, filepath.Dir(crewFile), `{"loggedIn": true}`)

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^ok +disk +\S+ \(zmina home and Claude Code config \S+\): 200 GiB free of 500 GiB$`, out)
	assert.Less(t, strings.Index(out, "disk "), strings.Index(out, "registry"), "before what a full disk explains")

	diskFree = func(string) (diskSpace, error) { return diskSpace{Free: 90 * mib, Total: 500 * gib, ID: "x"}, nil }

	out, _, err = zm(t, crewFile, "doctor")
	require.Error(t, err)
	assert.Regexp(t, `(?m)^fail +disk +\S+ .*90\.0 MiB free of 500 GiB · under 1.0 GiB`, out)

	out, _, _ = zm(t, crewFile, "doctor", "--json")
	assert.Contains(t, out, `"name": "disk"`)
	assert.Contains(t, out, `"level": "fail"`)
}

// resetDiskMemo forgets what this process told, before and after a test.
func resetDiskMemo(t *testing.T) {
	t.Helper()

	reset := func() {
		diskMemo.Lock()
		diskMemo.at = map[string]time.Time{}
		diskMemo.Unlock()
	}

	reset()
	t.Cleanup(reset)
}

// The dispatcher hears once per span, for each filesystem under the fail line, which one and how much is left. A
// filesystem that recovers is not told of.
func TestDiskBeatTellsTheDispatcherOncePerSpan(t *testing.T) {
	resetDiskMemo(t)

	c, crewFile := diskCrew(t, "")
	dir := filepath.Dir(crewFile)
	home, ns := c.StateDir(), c.Namespace

	var printed []string

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	state := func() *shift.State {
		st, err := shift.Read(dir)
		require.NoError(t, err)

		return st
	}
	beat := func(now time.Time) []string {
		require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Notes, s.State.NoticeMeta = nil, nil; return nil }))

		printed = nil
		a.out = writerFunc(func(p []byte) { printed = append(printed, string(p)) })
		a.diskBeat(now)

		return state().Notes
	}
	space := func(freeHome, freeNs uint64) {
		fakeDisks(t, map[string]diskSpace{
			home: {Free: freeHome, Total: 500 * gib, ID: "a"},
			ns:   {Free: freeNs, Total: 500 * gib, ID: "b"},
		})
	}

	day := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	// Room, and a filesystem only under the warn line: nothing to tell and nothing recorded.
	space(100*gib, 3*gib)
	assert.Empty(t, beat(day))
	assert.NoFileExists(t, diskToldPath(dir))

	// The Claude config's filesystem under the fail line.
	space(100*gib, 700*mib)

	notes := beat(day)
	require.Len(t, notes, 1)
	assert.Equal(t, "disk: the filesystem of "+ns+" (Claude Code config) has 700 MiB free of 500 GiB, under 1.0 GiB: "+
		"files written now can come out empty or cut short (a role's session file, the state); free space, then "+
		"restart a role that shows up unknown (zmina doctor names it)", notes[0])
	assert.Len(t, printed, 1, "the ticker's line shows it too")

	events, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(events), `"event":"disk-low"`)

	// Not again within the span, however much it falls.
	space(100*gib, 500*mib)
	assert.Empty(t, beat(day.Add(time.Minute)))
	assert.Empty(t, beat(day.Add(diskNoteEvery-time.Minute)))

	// After the span, while it lasts, once more.
	notes = beat(day.Add(diskNoteEvery))
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "has 500 MiB free")

	// The other filesystem joins at once and does not bring the first back.
	space(300*mib, 500*mib)
	notes = beat(day.Add(diskNoteEvery + time.Minute))
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "the filesystem of "+home+" (zmina home)")

	// It recovers: no note; and what was told is forgotten once its span is over, so the file does not grow.
	space(100*gib, 100*gib)
	assert.Empty(t, beat(day.Add(diskNoteEvery+2*time.Minute)))
	assert.Len(t, readDiskTold(dir), 2, "within the span the record stays")
	assert.Empty(t, beat(day.Add(3*diskNoteEvery)))
	assert.Empty(t, readDiskTold(dir))
}

// One filesystem under both directories is told once, as both.
func TestDiskBeatOneFilesystemOneNote(t *testing.T) {
	resetDiskMemo(t)

	c, crewFile := diskCrew(t, "")
	home, ns := c.StateDir(), c.Namespace
	fakeDisks(t, map[string]diskSpace{
		home: {Free: 10 * mib, Total: 100 * gib, ID: "a"},
		ns:   {Free: 10 * mib, Total: 100 * gib, ID: "a"},
	})

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	a.diskBeat(time.Now())

	st, err := shift.Read(filepath.Dir(crewFile))
	require.NoError(t, err)
	require.Len(t, st.Notes, 1)
	assert.Contains(t, st.Notes[0], "("+"zmina home and Claude Code config "+ns+")")
}

// A beat that cannot record that it told (the disk is full) does not crash or loop: it says it once in this process.
func TestDiskBeatWithoutARecord(t *testing.T) {
	resetDiskMemo(t)

	c, crewFile := diskCrew(t, "")
	dir := filepath.Dir(crewFile)
	fakeDisks(t, map[string]diskSpace{
		c.StateDir(): {Free: 10 * mib, Total: 100 * gib, ID: "a"},
		c.Namespace:  {Free: 10 * mib, Total: 100 * gib, ID: "a"},
	})

	// The record cannot be written: a directory stands where the file goes.
	require.NoError(t, os.Mkdir(diskToldPath(dir), 0o700))

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	now := time.Now()

	a.diskBeat(now)

	st, err := shift.Read(dir)
	require.NoError(t, err)
	assert.Len(t, st.Notes, 1, "told, though it could not be recorded")

	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Notes, s.State.NoticeMeta = nil, nil; return nil }))

	a.diskBeat(now.Add(time.Minute))
	a.diskBeat(now.Add(2 * time.Minute))

	st, err = shift.Read(dir)
	require.NoError(t, err)
	assert.Empty(t, st.Notes, "not again in this process")

	// A record that is not valid JSON is no record: a first time.
	resetDiskMemo(t)
	require.NoError(t, os.Remove(diskToldPath(dir)))
	require.NoError(t, os.WriteFile(diskToldPath(dir), []byte("not json"), 0o600))

	a.diskBeat(now)

	st, err = shift.Read(dir)
	require.NoError(t, err)
	assert.Len(t, st.Notes, 1)
	assert.Len(t, readDiskTold(dir), 1, "and now it is recorded")
}

// A note that could not be queued (the state does not save when the disk is full) is not recorded as told: the next
// beat tries again.
func TestDiskBeatThatCannotQueueTriesAgain(t *testing.T) {
	resetDiskMemo(t)

	c, crewFile := diskCrew(t, "")
	dir := filepath.Dir(crewFile)
	fakeDisks(t, map[string]diskSpace{
		c.StateDir(): {Free: 10 * mib, Total: 100 * gib, ID: "a"},
		c.Namespace:  {Free: 10 * mib, Total: 100 * gib, ID: "a"},
	})

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	require.NoError(t, a.withState(func(s *shift.Shift) error { return nil }))

	state := filepath.Join(dir, "state.json")
	good, err := os.ReadFile(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(state, []byte("{not json"), 0o600))

	a.diskBeat(time.Now())
	assert.NoFileExists(t, diskToldPath(dir), "nothing was told, so nothing is recorded")

	require.NoError(t, os.WriteFile(state, good, 0o600))

	a.diskBeat(time.Now())

	st, err := shift.Read(dir)
	require.NoError(t, err)
	assert.Len(t, st.Notes, 1)
	assert.FileExists(t, diskToldPath(dir))
}

// The minute beat runs it: a tick on a crew whose disk is low leaves the event.
func TestTickRunsTheDiskBeat(t *testing.T) {
	resetDiskMemo(t)

	crewFile := testCrew(t, "")
	diskFree = func(string) (diskSpace, error) { return diskSpace{Free: 50 * mib, Total: 100 * gib, ID: "a"}, nil }

	_, _, err := zm(t, crewFile, "tick")
	require.NoError(t, err)

	events, err := os.ReadFile(filepath.Join(filepath.Dir(crewFile), "events.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(events), `"event":"disk-low"`)
	assert.FileExists(t, diskToldPath(filepath.Dir(crewFile)))
}
