package shift

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/watch"
)

// A state file in the first format, with every key it had.
const stateV1JSON = `{
  "started": {"dispatcher": "2026-10-24T18:35:01Z", "dev": "2026-10-25T10:41:47Z"},
  "up": {"dispatcher": true, "dev": false},
  "held": {"dev": true},
  "stuck": {"dispatcher": 1761300000000},
  "notes": ["mr-watch: !35 new threads", "dev: ended"],
  "instances": {"dev#2": true},
  "last_session": {"dev": "sid-dev"},
  "idle": {"dispatcher": 1761300000001},
  "quick_ends": {"dev": 1},
  "broken": {"nightly": true},
  "inbox": {"dev": ["first", "second"]},
  "next_run": {"nightly": "2026-10-26T22:00:00Z"},
  "expired": {"temp": "2026-10-23"},
  "next_spec": {"nightly": "0 22 * * *"},
  "skip_noted": {"nightly": "T-4"},
  "watch": {"ci-red": {"last_run": "2026-10-26T10:00:00Z", "failures": 2}}
}`

func TestStateV1IsConvertedAndBackedUp(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte(stateV1JSON), 0o600))

	l, err := Lock(dir)
	require.NoError(t, err)

	dev := l.Role("dev")
	assert.True(t, dev.Held)
	assert.False(t, dev.Up)
	assert.Equal(t, "sid-dev", dev.LastSession)
	assert.Equal(t, []string{"first", "second"}, dev.Inbox)
	assert.Equal(t, 1, dev.QuickEnds)
	assert.True(t, l.Role("dev#2").Instance)
	assert.True(t, l.Role("nightly").Broken)
	assert.Equal(t, "0 22 * * *", l.Role("nightly").NextSpec)
	assert.Equal(t, "T-4", l.Role("nightly").SkipNoted)
	assert.Equal(t, "2026-10-23", l.Role("temp").Expired)
	assert.Equal(t, int64(1761300000000), l.Role("dispatcher").StuckAt)
	assert.Equal(t, []string{"mr-watch: !35 new threads", "dev: ended"}, l.Signals,
		"the old mixed queue is kept whole, as signals, which are never dropped")
	assert.Equal(t, 2, l.Watch["ci-red"].Failures)

	require.NoError(t, l.Save())

	bak, err := os.ReadFile(filepath.Join(dir, "state.json.v1.bak"))
	require.NoError(t, err)
	assert.Equal(t, stateV1JSON, string(bak), "the original is kept as it was")

	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	assert.Contains(t, string(raw), `"version": 2`)

	l, err = Lock(dir)
	require.NoError(t, err)
	assert.Equal(t, "sid-dev", l.Role("dev").LastSession, "the new format reads back")
	l.Release()
}

func TestBrokenStateIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version": 2, "roles": {"dev": {"held": tru`), 0o600))

	_, err := Lock(dir)
	require.ErrorIs(t, err, ErrStateBroken)
	assert.Contains(t, err.Error(), "zmina state recover")

	st, err := Read(dir)
	require.ErrorIs(t, err, ErrStateBroken)
	assert.Empty(t, st.Roles, "a look still gets a state to show")

	raw, _ := os.ReadFile(path)
	assert.Contains(t, string(raw), "tru", "untouched")

	aside, err := Recover(dir, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, path+".broken-20261026-120000", aside)

	l, err := Lock(dir)
	require.NoError(t, err, "fresh after recover")
	l.Release()

	_, err = Recover(dir, time.Now())
	assert.Error(t, err, "nothing to recover in a readable (or missing) file")
}

func TestFutureStateIsRefused(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"version": 9}`), 0o600))

	_, err := Lock(dir)
	require.ErrorIs(t, err, ErrStateFuture)
	assert.Contains(t, err.Error(), "upgrade zmina")
}

func TestLockTimesOutNamingTheHolder(t *testing.T) {
	old := LockWait
	LockWait = 300 * time.Millisecond

	t.Cleanup(func() { LockWait = old })

	dir := t.TempDir()

	held, err := Lock(dir)
	require.NoError(t, err)

	_, err = Lock(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("zmina pid %d", os.Getpid()))

	held.Release()

	l, err := Lock(dir)
	require.NoError(t, err)
	l.Release()
}

func TestProbeFailureLeavesTheRoleAlone(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick()
	require.True(t, f.tm.Has("harvester"))

	starts := len(f.tm.starts)
	f.tm.probeErr["harvester"] = errors.New("tmux list-panes: no answer in 10s")
	*f.now = f.now.Add(time.Hour)

	did := f.s.Tick()
	assert.Contains(t, strings.Join(did, "\n"), "harvester: left alone this beat")
	assert.True(t, f.s.State.Role("harvester").Up, "not taken as ended")
	assert.Len(t, f.tm.starts, starts, "not started again")
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "harvester: ended")

	v := f.s.Views()
	assert.Equal(t, PresenceUnknown, v[len(v)-1].State)
}

func TestHealthNotesNeverEvictSignals(t *testing.T) {
	f := fixtureWith(t, crewYAML)

	_, err := f.s.Note("mr-watch", "!35 four new threads", false)
	require.NoError(t, err)

	var notes []string
	for i := range notesMax + 5 {
		notes = append(notes, fmt.Sprintf("dev: busy for %d min", 90+i))
	}

	f.s.addNotes(notes)
	f.s.addNotes([]string{"dev: busy for 90+ min", "dev: busy for 90+ min"})

	assert.Equal(t, []string{"mr-watch: !35 four new threads"}, f.s.State.Signals)
	assert.Len(t, f.s.State.Notes, notesMax)
	assert.Equal(t, "dev: busy for 90+ min", f.s.State.Notes[notesMax-1], "a note waiting is not added twice")
}

func TestStopStartsNothing(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  nightly:
    schedule: "* * * * *"
    prompt: run
`)
	f.s.Stop()
	f.s.Tick()
	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()

	assert.Empty(t, f.tm.starts, "no keep: always role, no scheduled run")
	assert.True(t, f.ev.has("nightly", "schedule-skipped"))
	assert.Empty(t, f.tk.list)

	require.NoError(t, f.s.Start())
	assert.True(t, f.tm.Has("dispatcher"))
	assert.True(t, f.s.State.Stopped.IsZero())
}

func TestDowningIsLeftAlone(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick()

	// The dispatcher's /exit takes a while: the session is still there for the next beats.
	f.tm.exitSlow = true

	wait, err := f.s.BeginDown("dispatcher", DownOptions{Force: true})
	require.NoError(t, err)
	assert.True(t, wait)

	f.tm.die("dispatcher")
	// Started long ago, so only the down keeps the ticker from opening a fresh one.
	f.s.State.Role("dispatcher").Started = f.now.Add(-time.Hour)
	*f.now = f.now.Add(10 * time.Second)
	starts := len(f.tm.starts)
	f.s.Tick()
	assert.Len(t, f.tm.starts, starts, "a role being put down is not started again meanwhile")
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "dispatcher: ended")

	require.NoError(t, f.s.FinishDown("dispatcher"))
	assert.False(t, f.tm.Dead("dispatcher"))

	f.s.Tick()
	assert.Len(t, f.tm.starts, starts+1, "after the down the ticker opens a fresh dispatcher")
}

func TestRemoteControlAndBootGrace(t *testing.T) {
	f := fixtureWith(t, `namespace: /ns
claude: claude
remote_control: false
boot_grace: 30s
roles:
  dispatcher:
  phone:
    remote_control: true
`)

	assert.NotContains(t, f.s.Command(f.s.Crew.Roles["dispatcher"], ""), "--remote-control")
	assert.Equal(t, []string{"claude", "--name", "zm-phone", "--remote-control", "zm-phone"},
		f.s.Command(f.s.Crew.Roles["phone"], ""))

	require.NoError(t, f.s.Up("phone"))
	require.NoError(t, f.s.Queue("phone", "hello"))
	f.status("phone", "idle", *f.now)

	*f.now = f.now.Add(20 * time.Second)
	f.s.Tick()
	assert.Empty(t, f.tm.typed["phone"])

	*f.now = f.now.Add(15 * time.Second)
	f.s.Tick()
	assert.Equal(t, []string{"hello"}, f.tm.typed["phone"], "typed once boot_grace has passed")
}

func TestStaleWatchResultIsDiscarded(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runs := f.s.DueWatches()
	require.Len(t, runs, 1)

	// The run outlived its timeout and another took the reservation.
	f.s.State.Watch["ci-red"].Running = &watch.Running{PID: 1, Since: *f.now, Token: "newer"}

	did := f.s.FinishWatch(runs[0], watch.Result{Events: []watch.Event{ev("main", "red")}}, false)
	assert.Contains(t, strings.Join(did, "\n"), "superseded")
	assert.Empty(t, f.s.State.Signals)
	assert.True(t, f.ev.has("watch:ci-red", "watch-stale"))

	// The watch changed while its run went.
	f.s.State.Watch["ci-red"].Running = nil
	*f.now = f.now.Add(10 * time.Minute)
	runs = f.s.DueWatches()
	require.Len(t, runs, 1)

	f.s.Crew.Watches["ci-red"].Every = "10m"

	did = f.s.FinishWatch(runs[0], watch.Result{Events: []watch.Event{ev("main", "red")}}, false)
	assert.Contains(t, strings.Join(did, "\n"), "changed while it ran")
	assert.Empty(t, f.s.State.Signals)
	assert.False(t, slices.Contains(f.s.Names(), "ci-red"))
}

// state.json kept elsewhere and linked in: state.json.prev is still the state before the save. Linux hard-links a
// symbolic link itself, so a .prev made from the link would show the new state (macOS follows the link either way).
func TestSaveKeepsThePreviousStateThroughALink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "kept", "state.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(real), 0o700))

	save := func(session string) {
		l, err := Lock(dir)
		require.NoError(t, err)
		l.Role("dev").LastSession = session
		require.NoError(t, l.Save())
	}

	save("first")
	require.NoError(t, os.Rename(filepath.Join(dir, "state.json"), real))
	require.NoError(t, os.Symlink(real, filepath.Join(dir, "state.json")))

	save("second")

	fi, err := os.Lstat(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "state.json is still the link")

	fi, err = os.Lstat(filepath.Join(dir, "state.json.prev"))
	require.NoError(t, err)
	assert.Zero(t, fi.Mode()&os.ModeSymlink, ".prev is a file of its own, not the link")

	prev, err := os.ReadFile(filepath.Join(dir, "state.json.prev"))
	require.NoError(t, err)
	assert.Contains(t, string(prev), `"first"`)
	assert.NotContains(t, string(prev), `"second"`)
}

// state.json linked to a file on another file system (tmpfs here): no hard link reaches it, and .prev is a copy.
func TestSaveKeepsThePreviousStateAcrossFileSystems(t *testing.T) {
	other := "/dev/shm"
	if fi, err := os.Stat(other); err != nil || !fi.IsDir() {
		t.Skip("no /dev/shm")
	}

	dir := t.TempDir()

	far, err := os.MkdirTemp(other, "zmina-state-")
	if err != nil {
		t.Skip(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(far) })

	if a, b := devOf(t, dir), devOf(t, far); a == b {
		t.Skip("the temp dir and /dev/shm are one file system")
	}

	save := func(session string) {
		l, err := Lock(dir)
		require.NoError(t, err)
		l.Role("dev").LastSession = session
		require.NoError(t, l.Save())
	}

	save("first")
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(far, "state.json"), raw, 0o600))
	require.NoError(t, os.Remove(filepath.Join(dir, "state.json")))
	require.NoError(t, os.Symlink(filepath.Join(far, "state.json"), filepath.Join(dir, "state.json")))

	save("second")
	save("third")

	prev, err := os.ReadFile(filepath.Join(dir, "state.json.prev"))
	require.NoError(t, err, "a backup across file systems")
	assert.Contains(t, string(prev), `"second"`)
}

func TestSaveKeepsThePreviousState(t *testing.T) {
	dir := t.TempDir()

	save := func(session string) {
		l, err := Lock(dir)
		require.NoError(t, err)
		l.Role("dev").LastSession = session
		require.NoError(t, l.Save())
	}

	save("first")
	_, err := os.Stat(filepath.Join(dir, "state.json.prev"))
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing before the first save")

	save("second")
	save("third")

	prev, err := os.ReadFile(filepath.Join(dir, "state.json.prev"))
	require.NoError(t, err)
	assert.Contains(t, string(prev), `"second"`, "the state before the last save")

	now, err := os.ReadFile(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
	assert.Contains(t, string(now), `"third"`)
}

// A beat killed after it typed a line, before its save at the end: the line was flushed with the effect, so the
// next beat, reading what is on disk, does not type it again.
func TestTypedLineIsSavedAtOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Queue("designer", "first"))
	require.NoError(t, f.s.Queue("designer", "second"))
	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)

	snapshot := func() []byte {
		raw, err := json.Marshal(f.s.State)
		require.NoError(t, err)

		return raw
	}

	disk := snapshot() // what the last save left
	f.s.Flush = func() error { disk = snapshot(); return nil }

	f.s.Tick()
	require.Equal(t, []string{"first"}, f.tm.typed["designer"])

	// Killed before the beat's own save: the next beat starts from the disk.
	st := NewState()
	require.NoError(t, json.Unmarshal(disk, st))
	f.s.State = st

	f.s.Tick()
	assert.Equal(t, []string{"first", "second"}, f.tm.typed["designer"], "first is not typed twice")
}

// A flush that fails stops the beat's further effects: what is on disk no longer says what was done.
func TestFailedFlushStopsTheBeat(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick() // the roles kept up start, saved as usual
	require.NoError(t, f.s.Up("designer"))
	if !f.tm.Has("harvester") {
		require.NoError(t, f.s.Up("harvester"))
	}
	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("harvester", "two"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)
	f.status("harvester", "idle", *f.now)

	var warned []error
	f.s.Warn = func(err error) { warned = append(warned, err) }
	f.s.Flush = func() error { return errors.New("no space left on device") }

	f.s.Tick()
	assert.Len(t, append(f.tm.typed["designer"], f.tm.typed["harvester"]...), 1, "one line, then nothing more")
	require.Len(t, warned, 1)
	assert.ErrorContains(t, warned[0], "state not saved, nothing more typed this beat")
}

func TestFlushWritesAndKeepsTheLock(t *testing.T) {
	dir := t.TempDir()

	l, err := Lock(dir)
	require.NoError(t, err)
	l.Role("dev").LastSession = "flushed"
	require.NoError(t, l.Flush())

	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"flushed"`, "on disk at once")
	assert.NotNil(t, l.lock, "still held")

	l.Role("dev").LastSession = "saved"
	require.NoError(t, l.Save())
	raw, _ = os.ReadFile(filepath.Join(dir, "state.json"))
	assert.Contains(t, string(raw), `"saved"`)
}
