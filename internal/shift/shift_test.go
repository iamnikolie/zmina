package shift

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

type fakeTmux struct {
	up     map[string]int // role -> pane pid
	dead   map[string]bool
	typed  map[string][]string
	keys   map[string][]string
	starts []string
	argv   map[string][]string
	screen string
	// typeErr makes Type fail.
	typeErr  error
	probeErr map[string]error
	// exitSlow keeps a session up after /exit, as a claude that takes a while to end.
	exitSlow bool
	attached map[string]bool
	drafts   map[string]string
	dirs     map[string]string
	// onType runs for every typed line: a claude that takes a line changes its status.
	onType func(name string)
	// draftStays keeps a draft on C-c; dialogStays keeps a dialog on Escape.
	draftStays, dialogStays bool
}

func (f *fakeTmux) KeepScreens(string) error { return nil }

func (f *fakeTmux) Attached(n string) (bool, error) { return f.attached[n], nil }

// Prompt is an empty input line unless drafts holds what is typed, or "-" for a screen without one.
func (f *fakeTmux) Prompt(n string) (string, bool, error) {
	if d := f.drafts[n]; d == "-" {
		return "", false, nil
	} else {
		return d, true, nil
	}
}

func (f *fakeTmux) Has(n string) bool { _, ok := f.up[n]; return ok }

// Probe is what the shift asks; probeErr makes it fail for one role, as a tmux that does not answer.
func (f *fakeTmux) Probe(n string) (tmux.Pane, error) {
	if err := f.probeErr[n]; err != nil {
		return tmux.Pane{}, err
	}

	if pid, ok := f.up[n]; ok {
		return tmux.Pane{State: tmux.Up, PID: pid, ID: "%" + n}, nil
	}

	if f.dead[n] {
		return tmux.Pane{State: tmux.Dead, ID: "%" + n}, nil
	}

	return tmux.Pane{State: tmux.Absent}, nil
}
func (f *fakeTmux) Dead(n string) bool { return f.dead[n] }

// die ends a role's process, keeping its screen, as remain-on-exit does.
func (f *fakeTmux) die(n string) { delete(f.up, n); f.dead[n] = true }

// Keys acts as Claude Code does on a few: C-c drops a draft, and two of them in one call on an empty input line end
// it; Escape closes a dialog unless dialogStays; a digit answers the exit dialog on the screen.
func (f *fakeTmux) Keys(n string, keys ...string) error {
	f.keys[n] = append(f.keys[n], keys...)

	presses := 0

	for _, k := range keys {
		switch d := f.drafts[n]; {
		case k == "Escape" && d == "-" && !f.dialogStays:
			delete(f.drafts, n)
		case k == "C-c" && d != "" && d != "-" && !f.draftStays:
			delete(f.drafts, n)
		case k == "C-c" && d == "":
			if presses++; presses == 2 {
				delete(f.up, n)
			}
		case strings.Contains(f.screen, k+". Exit and stop tasks"):
			delete(f.up, n)
			f.screen = ""
		}
	}

	return nil
}

func (f *fakeTmux) Capture(n string, _ int) (string, error) {
	if !f.Has(n) && !f.dead[n] {
		return "", errors.New("no session")
	}

	return f.screen, nil
}

func (f *fakeTmux) PanePID(n string) (int, error) {
	if pid, ok := f.up[n]; ok {
		return pid, nil
	}

	return 0, errors.New("no session")
}

func (f *fakeTmux) Start(n, dir string, _ []string, argv []string) error {
	if f.dirs == nil {
		f.dirs = map[string]string{}
	}

	f.dirs[n] = dir
	f.up[n] = 1000 + len(f.starts)
	f.starts = append(f.starts, n)
	f.argv[n] = argv

	return nil
}

func (f *fakeTmux) Kill(n string) error { delete(f.up, n); delete(f.dead, n); return nil }

func (f *fakeTmux) Type(n, text string) error {
	if f.typeErr != nil && text != "/exit" {
		return f.typeErr
	}

	f.typed[n] = append(f.typed[n], text)
	if f.onType != nil {
		f.onType(n)
	}
	if text == "/exit" && !f.exitSlow {
		delete(f.up, n)
	}

	return nil
}

type fakeReg struct {
	byPID   map[int]*registry.Session
	outside map[string]*registry.Session
	context map[string]int
	errors  map[string]string
}

func (f *fakeReg) Last(id string) (registry.Turn, error) {
	t := registry.Turn{Context: f.context[id]}
	if e := f.errors[id]; e != "" {
		t.Error, t.ErrorID = e, "e-"+e
	}

	return t, nil
}

func (f *fakeReg) ByPID(pid int) (*registry.Session, error) { return f.byPID[pid], nil }
func (f *fakeReg) LiveByName(n string) *registry.Session    { return f.outside[n] }

type fakeTasks struct{ list []task.Task }

func (f *fakeTasks) Active() []task.Task {
	var out []task.Task

	for _, t := range f.list {
		if t.State.Active() {
			out = append(out, t)
		}
	}

	return out
}

func (f *fakeTasks) Add(t task.Task, now time.Time) (task.Task, error) {
	t.ID = "T-" + string(rune('1'+len(f.list)))
	if t.State == "" {
		t.State = task.Open
	}

	t.Created = now
	f.list = append(f.list, t)

	return t, nil
}

type fakeEvents struct{ list []events.Event }

func (f *fakeEvents) Add(e events.Event) error { f.list = append(f.list, e); return nil }

func (f *fakeEvents) has(role, event string) bool {
	for _, e := range f.list {
		if e.Role == role && e.Event == event {
			return true
		}
	}

	return false
}

const crewYAML = `namespace: /ns
claude: claude
roles:
  dispatcher:
    boot: read d and boot
  designer:
  harvester:
    keep: always
`

type fix struct {
	s   *Shift
	tm  *fakeTmux
	rg  *fakeReg
	tk  *fakeTasks
	ev  *fakeEvents
	now *time.Time
}

func load(t *testing.T, body string) *Shift {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte("dir: "+dir+"\n"+body), 0o600))

	c, err := crew.Load(path)
	require.NoError(t, err)

	return &Shift{Crew: c}
}

func fixtureWith(t *testing.T, body string) fix {
	t.Helper()

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	s := load(t, body)
	f := fix{
		s:   s,
		tm:  &fakeTmux{attached: map[string]bool{}, drafts: map[string]string{}, up: map[string]int{}, dead: map[string]bool{}, typed: map[string][]string{}, keys: map[string][]string{}, argv: map[string][]string{}, probeErr: map[string]error{}},
		rg:  &fakeReg{byPID: map[int]*registry.Session{}, outside: map[string]*registry.Session{}, context: map[string]int{}, errors: map[string]string{}},
		tk:  &fakeTasks{},
		ev:  &fakeEvents{},
		now: &now,
	}
	s.Tmux, s.Reg, s.Tasks, s.Events, s.State, s.Path = f.tm, f.rg, f.tk, f.ev, NewState(), "/bin"
	s.Now = func() time.Time { return *f.now }
	s.By = "hand"
	f.tm.onType = func(n string) {
		if sess := f.rg.byPID[f.tm.up[n]]; sess != nil {
			sess.StatusUpdatedAt = f.now.UnixMilli() + 1
		}
	}

	return f
}

func fixture(t *testing.T) (*Shift, *fakeTmux, *fakeReg, *time.Time) {
	t.Helper()

	f := fixtureWith(t, crewYAML)

	return f.s, f.tm, f.rg, f.now
}

// idle marks a role idle (or busy) in the registry since the given time.
func (f fix) status(role, status string, since time.Time) {
	f.rg.byPID[f.tm.up[role]] = &registry.Session{Status: status, StatusUpdatedAt: since.UnixMilli(), SessionID: "sid-" + role}
}

func TestCommandOrder(t *testing.T) {
	s, _, _, _ := fixture(t)
	r := &crew.Role{Session: "zm-dev", Boot: "read AGENTS.md", Model: "opus", Args: []string{"--chrome"}}

	assert.Equal(t, []string{"claude", "--model", "opus", "--chrome", "--name", "zm-dev", "read AGENTS.md",
		"--remote-control", "zm-dev"}, s.Command(r, ""))
	assert.Equal(t, []string{"claude", "--resume", "abc", "--name", "zm-x", "--remote-control", "zm-x"},
		s.Command(&crew.Role{Session: "zm-x"}, "abc"))

	s.Crew.DropTools = []string{"Artifact", "Workflow"}
	assert.Equal(t, []string{"claude", "--chrome", "--disallowedTools=Artifact,Workflow,Agent", "--resume", "abc",
		"--name", "zm-x", "--remote-control", "zm-x"},
		s.Command(&crew.Role{Session: "zm-x", Args: []string{"--chrome"}, DropTools: []string{"Agent"}}, "abc"))
}

func TestTickStartsAlwaysRolesOnly(t *testing.T) {
	s, tm, _, _ := fixture(t)

	did := s.Tick()

	assert.ElementsMatch(t, []string{"dispatcher", "harvester"}, tm.starts)
	assert.Contains(t, did, "started dispatcher")
	assert.Equal(t, []string{"harvester: started again (keep: always)"}, s.State.Notes,
		"the dispatcher is not told about itself")
}

func TestTickPausesBetweenRestarts(t *testing.T) {
	s, tm, _, now := fixture(t)
	s.Tick()
	delete(tm.up, "harvester")

	*now = now.Add(time.Minute)
	s.Tick()
	assert.Len(t, tm.starts, 2, "no restart within five minutes")
	assert.Contains(t, s.State.Notes, "harvester: ended")

	*now = now.Add(5 * time.Minute)
	s.Tick()
	assert.Len(t, tm.starts, 3)
}

func TestTickLeavesOutsideSessionsAlone(t *testing.T) {
	s, tm, rg, _ := fixture(t)
	rg.outside["zm-harvester"] = &registry.Session{PID: 7, Name: "zm-harvester"}

	s.Tick()

	assert.Equal(t, []string{"dispatcher"}, tm.starts)
	assert.Error(t, s.Up("harvester"), "a name held outside zmina is not taken twice")
}

func TestTickDeliversToIdleBootedDispatcher(t *testing.T) {
	s, tm, rg, now := fixture(t)
	s.Tick()

	dpid := tm.up["dispatcher"]
	rg.byPID[dpid] = &registry.Session{Status: "idle"}
	assert.Empty(t, tm.typed["dispatcher"], "not while booting")

	*now = now.Add(3 * time.Minute)
	rg.byPID[dpid] = &registry.Session{Status: "busy", StatusUpdatedAt: now.UnixMilli()}
	s.Tick()
	assert.Empty(t, tm.typed["dispatcher"], "not while busy")

	rg.byPID[dpid] = &registry.Session{Status: "idle"}
	s.Tick()
	require.Len(t, tm.typed["dispatcher"], 1)
	assert.Equal(t, "[zmina] harvester: started again (keep: always)", tm.typed["dispatcher"][0])
	assert.Empty(t, s.State.Notes)
}

func TestTickNotesStuckOncePerStretch(t *testing.T) {
	s, tm, rg, now := fixture(t)
	s.Tick()

	busySince := now.Add(-2 * time.Hour).UnixMilli()
	rg.byPID[tm.up["harvester"]] = &registry.Session{Status: "busy", StatusUpdatedAt: busySince}

	s.Tick()
	s.Tick()

	n := 0
	for _, note := range s.State.Notes {
		if note == "harvester: busy for 120 min" {
			n++
		}
	}

	assert.Equal(t, 1, n)
}

func TestDownKeepsTheDispatcherWithoutForce(t *testing.T) {
	s, tm, _, now := fixture(t)
	s.Tick()

	assert.Error(t, s.Down("dispatcher", DownOptions{}))
	require.NoError(t, s.Down("harvester", DownOptions{}))
	assert.False(t, tm.Has("harvester"))
	assert.Equal(t, []string{"/exit"}, tm.typed["harvester"])

	*now = now.Add(time.Hour)
	s.Tick()
	assert.NotContains(t, s.State.Notes, "harvester: ended", "an intended down is not news")
	assert.False(t, tm.Has("harvester"), "a role put down by hand stays down")

	require.NoError(t, s.Up("harvester"))
	assert.False(t, s.State.Role("harvester").Held)
}

func TestCleanEnv(t *testing.T) {
	env := []string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=x", "CLAUDE_CONFIG_DIR=/p",
		"CLAUDE_CODE_MESSAGING_SOCKET=/s", "CLAUDE_CODE_USE_BEDROCK=1", "EXAMPLE_PROFILE=d", "TMUX=/t", "CMUX_TAB_ID=3",
		"HOME=/h"}

	assert.Equal(t, []string{"PATH=/bin", "CLAUDE_CODE_USE_BEDROCK=1", "EXAMPLE_PROFILE=d", "HOME=/h"},
		tmux.CleanEnv(env, nil), "session identity goes, provider settings stay")
	assert.Equal(t, []string{"PATH=/bin", "CLAUDE_CODE_USE_BEDROCK=1", "HOME=/h"},
		tmux.CleanEnv(env, []string{"EXAMPLE_*"}), "drop_env adds to the list")
}

func TestInstances(t *testing.T) {
	f := fixtureWith(t, crewYAML)

	require.NoError(t, f.s.Up("designer#2"))
	assert.Equal(t, []string{"dispatcher", "designer", "designer#2", "harvester"}, f.s.Names())
	assert.Contains(t, f.tm.argv["designer#2"], "zm-designer-2")

	v := f.s.Views()[2]
	assert.Equal(t, "designer#2", v.Role)
	assert.Equal(t, crew.KeepManual, v.Keep)

	_, err := f.s.Role("dispatcher#2")
	assert.Error(t, err)
	_, err = f.s.Role("designer#1")
	assert.Error(t, err)

	require.NoError(t, f.s.Down("designer#2", DownOptions{}))
	assert.NotContains(t, f.s.Names(), "designer#2", "an instance is gone once down")

	require.NoError(t, f.s.Up("designer#3"))
	f.tm.die("designer#3")
	f.s.Tick()
	assert.NotContains(t, f.s.Names(), "designer#3", "an instance that ended is gone")
}

func TestResume(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  writer:
    resume: always
`)

	assert.Error(t, f.s.UpWith("designer", UpOptions{Resume: "last"}), "nothing to resume yet")

	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	require.NoError(t, f.s.Down("designer", DownOptions{}))
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").LastSession)

	require.NoError(t, f.s.UpWith("designer", UpOptions{Resume: "last"}))
	assert.Contains(t, strings.Join(f.tm.argv["designer"], " "), "--resume sid-designer")

	require.NoError(t, f.s.Up("writer"))
	assert.NotContains(t, f.tm.argv["writer"], "--resume", "resume: always without a known session starts fresh")

	f.status("writer", "idle", *f.now)
	f.s.Tick()
	f.tm.die("writer")
	f.s.Tick()
	require.NoError(t, f.s.Up("writer"))
	assert.Contains(t, strings.Join(f.tm.argv["writer"], " "), "--resume sid-writer")
}

func TestEndedKeepsScreenAndNamesTask(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.tm.screen = "panic: boom"
	f.tk.list = []task.Task{{ID: "T-7", Role: "designer", State: task.Open}}

	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(time.Hour)
	f.tm.die("designer")
	f.s.Tick()

	require.NotEmpty(t, f.s.State.Notes)

	var note string

	for _, n := range f.s.State.Notes {
		if strings.HasPrefix(n, "designer: ended") {
			note = n
		}
	}

	assert.Contains(t, note, "(held T-7)")
	assert.Contains(t, note, "last screen screens/designer-")
	assert.False(t, f.tm.Dead("designer"), "the dead session is collected")

	files, _ := filepath.Glob(filepath.Join(f.s.ScreensDir(), "designer-*.txt"))
	require.Len(t, files, 1)

	raw, _ := os.ReadFile(files[0])
	assert.Equal(t, "panic: boom\n", string(raw))
	assert.True(t, f.ev.has("designer", "ended"))
}

func TestDiesOnBootMarksBroken(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick() // starts dispatcher and harvester

	for range 2 {
		*f.now = f.now.Add(time.Minute)
		f.tm.die("harvester")
		f.s.Tick()
		*f.now = f.now.Add(reopenPause)
		f.s.Tick()
	}

	assert.True(t, f.s.State.Role("harvester").Broken)
	assert.False(t, f.tm.Has("harvester"), "a broken role is not started again")
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "harvester: dies on boot")

	require.NoError(t, f.s.Up("harvester"))
	assert.False(t, f.s.State.Role("harvester").Broken, "a hand up clears it")
}

func TestStuckAndIdleNameTasks(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.tk.list = []task.Task{{ID: "T-3", Role: "designer", State: task.Open}}

	f.status("designer", "busy", f.now.Add(-2*time.Hour))
	f.s.Tick()
	assert.Contains(t, f.s.State.Notes, "designer: busy for 120 min on T-3")

	f.s.State.Notes = nil
	f.status("designer", "idle", f.now.Add(-time.Hour))
	f.s.Tick()
	assert.Empty(t, f.s.State.Notes, "idle with an active task is not news")

	f.tk.list[0].State = task.Done
	f.s.Tick()
	f.s.Tick()
	assert.Equal(t, []string{"designer: idle for 60 min, no active task"}, f.s.State.Notes, "once per idle stretch")
}

func TestScheduleRunsAndSkips(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  nightly:
    schedule: "0 22 * * *"
    prompt: run the harvest
`)
	*f.now = time.Date(2026, 10, 24, 21, 58, 0, 0, time.UTC)
	f.s.Tick()
	assert.Equal(t, time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun)
	assert.False(t, f.tm.Has("nightly"))

	*f.now = f.now.Add(3 * time.Minute)
	f.s.Tick()
	require.Len(t, f.tk.list, 1)
	assert.Equal(t, "schedule", f.tk.list[0].By)
	assert.True(t, f.tm.Has("nightly"), "a scheduled run starts the role")
	assert.Equal(t, []string{"[zmina T-1, scheduled] run the harvest"}, f.s.State.Role("nightly").Inbox)

	f.status("nightly", "idle", *f.now)
	f.s.Tick()
	assert.Empty(t, f.tm.typed["nightly"], "not while booting")

	*f.now = f.now.Add(bootPause)
	f.s.Tick()
	assert.Equal(t, []string{"[zmina T-1, scheduled] run the harvest"}, f.tm.typed["nightly"])
	assert.Empty(t, f.s.State.Role("nightly").Inbox)

	*f.now = f.now.Add(24 * time.Hour)
	f.s.Tick()
	assert.Len(t, f.tk.list, 1, "a run is skipped while the last one's task is active")
	assert.True(t, f.ev.has("nightly", "schedule-skipped"))
}

func TestQueueWaitsForIdle(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Queue("designer", "first"))
	require.NoError(t, f.s.Queue("designer", "second"))

	f.s.Tick()
	assert.Empty(t, f.tm.typed["designer"], "a role that is down keeps its lines")

	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "busy", *f.now)
	f.s.Tick()
	assert.Empty(t, f.tm.typed["designer"])

	f.status("designer", "idle", *f.now)
	f.s.Tick()
	assert.Equal(t, []string{"first"}, f.tm.typed["designer"], "one line per beat")
	f.s.Tick()
	assert.Equal(t, []string{"first", "second"}, f.tm.typed["designer"])
}

func TestExpiredSeat(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  temp:
    expires: 2026-10-23
`)
	f.s.Tick()
	f.s.Tick()

	n := 0

	for _, note := range f.s.State.Notes {
		if strings.HasPrefix(note, "temp: seat expired") {
			n++
		}
	}

	assert.Equal(t, 1, n)
}

func TestPressKeysAllowsNamesOnly(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))

	require.NoError(t, f.s.PressKeys("designer", []string{"Down", "Enter"}))
	assert.Equal(t, []string{"Down", "Enter"}, f.tm.keys["designer"])
	assert.Error(t, f.s.PressKeys("designer", []string{"rm -rf"}))
}

func TestPeekDeadSession(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.tm.screen = "last words"

	_, err := f.s.Peek("designer", 10)
	assert.ErrorIs(t, err, ErrNotUp)

	require.NoError(t, f.s.Up("designer"))
	f.tm.die("designer")

	out, err := f.s.Peek("designer", 10)
	require.NoError(t, err)
	assert.Equal(t, "last words", out)
}

func TestPrune(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.State.Role("gone").Held = true
	f.s.State.Role("gone#2").Instance = true
	f.s.State.Role("designer#2").Instance = true
	f.s.State.Role("designer").Up = true

	f.s.State.Prune(f.s.Known)

	assert.NotContains(t, f.s.State.Roles, "gone")
	assert.NotContains(t, f.s.State.Roles, "gone#2", "an instance of a role that left goes with it")
	assert.Contains(t, f.s.State.Roles, "designer#2")
	assert.True(t, f.s.State.Role("designer").Up)
}

func TestDownOfDeadRoleHoldsIt(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick()
	f.tm.die("harvester")

	require.NoError(t, f.s.Down("harvester", DownOptions{}))
	assert.True(t, f.s.State.Role("harvester").Held)

	*f.now = f.now.Add(time.Hour)
	f.s.Tick()
	assert.False(t, f.tm.Has("harvester"), "a dead role put down by hand stays down")
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "harvester: ended")
}

func TestScheduleChangeCountsFromNow(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  nightly:
    schedule: "0 22 * * *"
    prompt: run
`)
	*f.now = time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)
	f.s.Tick()
	assert.Equal(t, time.Date(2026, 10, 25, 22, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun)

	f.s.Crew.Roles["nightly"].Schedule = "0 6 * * *"
	reload(t, f.s)
	f.s.Tick()
	assert.Equal(t, time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun)
}

func TestScheduleSkipIsNotedOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  every:
    schedule: "* * * * *"
    prompt: run
`)
	f.s.Tick()
	*f.now = f.now.Add(time.Minute)
	f.s.Tick() // T-1
	require.Len(t, f.tk.list, 1)

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	n := 0

	for _, note := range f.s.State.Notes {
		if note == "every: scheduled run skipped, still holds T-1" {
			n++
		}
	}

	assert.Equal(t, 1, n)
}

func TestQueuedLineThatFailsMovesToFailed(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	require.NoError(t, f.s.Queue("designer", "bad"))
	require.NoError(t, f.s.Queue("designer", "good"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)

	f.tm.typeErr = errors.New("tmux refused")
	for range typeTries - 1 {
		f.s.Tick()
	}

	assert.Equal(t, []string{"bad", "good"}, f.s.State.Role("designer").Inbox, "retried, not dropped")
	f.s.Tick()
	assert.True(t, f.ev.has("designer", "type-failed"))
	require.Len(t, f.s.State.Role("designer").Failed, 1)
	assert.Equal(t, "bad", f.s.State.Role("designer").Failed[0].Line)

	f.tm.typeErr = nil
	f.s.Tick()
	assert.Equal(t, []string{"good"}, f.tm.typed["designer"], "the failed line does not block the next")

	n, err := f.s.Retry("designer")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	f.s.Tick()
	assert.Equal(t, []string{"good", "bad"}, f.tm.typed["designer"], "a retried line goes again")

	require.NoError(t, f.s.Queue("designer", "later"))
	n, err = f.s.Drop("designer", true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	for _, q := range f.s.Queues() {
		assert.NotEqual(t, "designer", q.Role, "nothing waits for the designer")
	}
}

func TestStaleDirRoleDoesNotStart(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  stale:
    keep: always
    dir: /nonexistent/zmina-test
`)
	err := f.s.Up("stale")
	require.Error(t, err)

	f.s.Tick()
	f.s.Tick()
	assert.NotContains(t, f.tm.starts, "stale")
	assert.Contains(t, f.tm.starts, "dispatcher", "the rest of the crew runs")
	assert.Contains(t, f.s.Views()[len(f.s.Views())-1].Problem, "not a directory")
}

const bootPause = 2 * time.Minute

// reload re-checks the crew after a test changed a role, as loading the file would.
func reload(t *testing.T, s *Shift) {
	t.Helper()

	raw := "namespace: /ns\nclaude: claude\nroles:\n"
	for _, n := range s.Crew.Names() {
		r := s.Crew.Roles[n]
		raw += "  " + n + ":\n    keep: " + string(r.Keep) + "\n"

		if r.Schedule != "" {
			raw += "    schedule: \"" + r.Schedule + "\"\n    prompt: " + r.Prompt + "\n"
		}
	}

	c := load(t, raw)
	s.Crew = c.Crew
}

func TestNoteReachesIdleDispatcherAsOneLine(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick()
	f.status("dispatcher", "idle", *f.now)

	queued, err := f.s.Note("mr-watch", "!35 — 4 new\nthreads (reviewer X)", false)
	require.NoError(t, err)
	assert.True(t, queued)

	queued, err = f.s.Note("mr-watch", "!35 — 4 new threads  (reviewer X)", false)
	require.NoError(t, err)
	assert.False(t, queued, "the same note still waiting is not queued twice")

	f.s.Tick()
	assert.Empty(t, f.tm.typed["dispatcher"], "not while booting")

	*f.now = f.now.Add(bootPause)
	f.s.Tick()
	require.Len(t, f.tm.typed["dispatcher"], 1)
	assert.Equal(t, "[zmina] mr-watch: !35 — 4 new threads (reviewer X) · harvester: started again (keep: always)",
		f.tm.typed["dispatcher"][0])
	assert.Empty(t, f.s.State.Signals)
	assert.True(t, f.ev.has("dispatcher", "noted"))

	queued, err = f.s.Note("mr-watch", "!35 — 4 new threads (reviewer X)", false)
	require.NoError(t, err)
	assert.True(t, queued, "once told, the same note is news again")
}

func TestNoteIfUp(t *testing.T) {
	f := fixtureWith(t, crewYAML)

	_, err := f.s.Note("mr-watch", "!35", true)
	require.ErrorIs(t, err, ErrNotUp)
	assert.Empty(t, f.s.State.Signals)

	queued, err := f.s.Note("mr-watch", "!35", false)
	require.NoError(t, err)
	assert.True(t, queued, "without --if-up it waits for the dispatcher")

	_, err = f.s.Note("", "  ", false)
	require.Error(t, err)

	for i := len(f.s.State.Signals); i < signalsMax; i++ {
		f.s.State.Signals = append(f.s.State.Signals, "x"+string(rune('a'+i)))
	}

	_, err = f.s.Note("mr-watch", "!36", false)
	require.Error(t, err, "a full queue refuses")
}

func TestDownTypesExitOnlyIntoAnEmptyInputLine(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick()

	// A draft: refused, nothing pressed, the role as it was.
	f.tm.drafts["harvester"] = "drop the steps, start with steps 1–2 ✓"
	require.ErrorContains(t, f.s.Down("harvester", DownOptions{}), "a draft sits in its input line")
	assert.Empty(t, f.tm.typed["harvester"])
	assert.Empty(t, f.tm.keys["harvester"])
	assert.True(t, f.tm.Has("harvester"))
	assert.False(t, f.s.State.Role("harvester").Held)

	// A draft C-c does not drop: refused even with clear.
	f.tm.draftStays = true
	require.ErrorContains(t, f.s.Down("harvester", DownOptions{Clear: true}), "did not clear")
	assert.Empty(t, f.tm.typed["harvester"])
	assert.True(t, f.tm.Has("harvester"))

	// Clear: C-c drops the draft, then /exit.
	f.tm.draftStays = false
	require.NoError(t, f.s.Down("harvester", DownOptions{Clear: true}))
	assert.Equal(t, []string{"C-c", "C-c"}, f.tm.keys["harvester"], "one C-c per try")
	assert.Equal(t, []string{"/exit"}, f.tm.typed["harvester"])
	assert.False(t, f.tm.Has("harvester"))
	assert.True(t, f.ev.has("harvester", "down"))
}

func TestDownAndADialog(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick()
	require.NoError(t, f.s.Up("designer"))

	// A dialog (a permission prompt): /exit's Enter would pick its highlighted answer.
	f.tm.drafts["designer"] = "-"
	require.ErrorContains(t, f.s.Down("designer", DownOptions{}), "a dialog is open")
	assert.Empty(t, f.tm.typed["designer"])
	assert.Empty(t, f.tm.keys["designer"])

	// Clear: Escape closes it, then /exit into the input line it leaves.
	require.NoError(t, f.s.Down("designer", DownOptions{Clear: true}))
	assert.Equal(t, []string{"Escape"}, f.tm.keys["designer"])
	assert.Equal(t, []string{"/exit"}, f.tm.typed["designer"])

	// A screen Escape does not clear: no text typed into it, C-c C-c in one call instead.
	require.NoError(t, f.s.Up("designer"))
	f.tm.keys["designer"], f.tm.typed["designer"] = nil, nil
	f.tm.drafts["designer"], f.tm.dialogStays = "-", true

	wait, err := f.s.BeginDown("designer", DownOptions{Clear: true})
	require.NoError(t, err)
	assert.True(t, wait)
	assert.Equal(t, []string{"Escape", "C-c", "C-c"}, f.tm.keys["designer"])
	assert.Empty(t, f.tm.typed["designer"])
	require.NoError(t, f.s.FinishDown("designer"))
	assert.False(t, f.tm.Has("designer"))
}

func TestWaitGoneAnswersTheExitDialog(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Tick()

	// Background work runs: /exit asks first, and the session stays until it is answered.
	f.tm.exitSlow = true
	f.tm.screen = "   Background work is running\n   The following will stop when you exit:\n   shell · sleep 900\n" +
		"   ❯ 1. Exit and stop tasks\n     2. Move to background and exit\n     3. Stay\n   Enter to confirm · Esc to cancel"

	wait, err := f.s.BeginDown("harvester", DownOptions{})
	require.NoError(t, err)
	require.True(t, wait)

	start := time.Now()
	WaitGone(f.tm, []string{"harvester"}, ExitWait)
	assert.Less(t, time.Since(start), 2*time.Second, "answered, not waited out")
	assert.Equal(t, []string{"1"}, f.tm.keys["harvester"])
	assert.False(t, f.tm.Has("harvester"))
}
