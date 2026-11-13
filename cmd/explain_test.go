package cmd

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// screenTmux is a pane that shows whatever text a test puts on it, with an input line or without.
type screenTmux struct {
	shift.Tmux
	pid      int
	screen   string
	noInput  bool
	draft    string
	absent   bool
	probeErr error
}

func (t *screenTmux) Probe(string) (tmux.Pane, error) {
	switch {
	case t.probeErr != nil:
		return tmux.Pane{}, t.probeErr
	case t.absent:
		return tmux.Pane{State: tmux.Absent}, nil
	}

	return tmux.Pane{State: tmux.Up, PID: t.pid}, nil
}

func (t *screenTmux) Prompt(string) (string, bool, error) { return t.draft, !t.noInput, nil }

func (t *screenTmux) Input(string) (string, string, bool, error) { return t.draft, "", !t.noInput, nil }

func (t *screenTmux) Capture(string, int) (string, error) { return t.screen, nil }
func (*screenTmux) KeepScreens(string) error              { return nil }
func (*screenTmux) Attached(string) (bool, error)         { return false, nil }

// explainRig is a crew of one role, dev, over a pane and a session registry the test controls, and a clock that stands
// still, so that a command's output is the same on every run.
type explainRig struct {
	t        *testing.T
	crewFile string
	ns       string
	pid      int
	named    string
	tm       *screenTmux
	now      time.Time
}

func newExplainRig(t *testing.T) *explainRig {
	t.Helper()

	crewFile := testCrew(t, "  dev:\n")
	// Times in the crew file's zone, whatever the machine's.
	f, err := os.OpenFile(crewFile, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("timezone: UTC\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	// The pane's process is this one: a pid that is alive, as the registry's live lookup by session name wants.
	r := &explainRig{t: t, crewFile: crewFile, ns: filepath.Join(filepath.Dir(crewFile), "ns"), pid: os.Getpid(),
		named: c.Roles["dev"].Session, now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	r.tm = &screenTmux{pid: r.pid}

	require.NoError(t, os.MkdirAll(filepath.Join(r.ns, "sessions"), 0o700))

	return r
}

// session writes the registry entry of dev: its status since the rig's now, and what it waits on.
func (r *explainRig) session(status, waitingFor string) {
	r.t.Helper()

	body := `{"pid": ` + strconv.Itoa(r.pid) + `, "sessionId": "sid-dev-0001", "name": "` + r.named + `", "status": "` + status +
		`", "statusUpdatedAt": ` + strconv.FormatInt(r.now.UnixMilli(), 10)
	if waitingFor != "" {
		body += `, "waitingFor": "` + waitingFor + `"`
	}

	require.NoError(r.t, os.WriteFile(filepath.Join(r.ns, "sessions", strconv.Itoa(r.pid)+".json"), []byte(body+"}"), 0o600))
}

func (r *explainRig) noSession() {
	r.t.Helper()

	require.NoError(r.t, os.RemoveAll(filepath.Join(r.ns, "sessions", strconv.Itoa(r.pid)+".json")))
}

// seams is what a shift of the rig talks to: its pane, its registry and its clock.
func (r *explainRig) seams() func(s *shift.Shift) {
	return func(s *shift.Shift) {
		s.Tmux, s.Reg, s.Now = r.tm, &registry.Registry{Namespace: r.ns}, func() time.Time { return r.now }
	}
}

// run runs one command against the rig, reading its state as a fresh process would.
func (r *explainRig) run(args ...string) (string, string, error) {
	r.t.Helper()

	var out, errb bytes.Buffer

	a := &app{out: &out, err: &errb, shiftSeams: r.seams()}
	root := newRoot(a)
	root.SetArgs(append([]string{"--crew", r.crewFile}, args...))
	err := root.Execute()

	return out.String(), errb.String(), err
}

func (r *explainRig) mustRun(args ...string) string {
	r.t.Helper()

	out, errOut, err := r.run(args...)
	require.NoError(r.t, err, errOut)

	return out
}

// explainJSON is zmina explain --json decoded.
func (r *explainRig) explainJSON() explainOut {
	r.t.Helper()

	var out explainOut

	require.NoError(r.t, json.Unmarshal([]byte(r.mustRun("explain", "dev", "--json")), &out))

	return out
}

// lsCells are what zmina ls writes for dev: the state and status of its compact line, and the STATUS column of --wide.
func (r *explainRig) lsCells() (shows, status string) {
	r.t.Helper()

	compact := r.mustRun("ls")

	for _, line := range strings.Split(compact, "\n") {
		if rest, ok := strings.CutPrefix(line, "dev "); ok {
			// The compact line is role, state and status, for how long, context, tasks: columns two spaces apart or more.
			cols := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(rest), -1)
			shows = cols[0]
		}
	}

	require.NotEmpty(r.t, shows, "ls has a line for dev:\n%s", compact)

	wide := r.mustRun("ls", "--wide", "dev")
	for _, line := range strings.Split(wide, "\n") {
		if strings.HasPrefix(line, "dev ") && !strings.HasPrefix(line, "dev now:") && status == "" {
			// ROLE SESSION STATE STATUS FOR CONTEXT TASKS KEEP ABOUT: columns two spaces apart or more.
			cols := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), -1)
			require.GreaterOrEqual(r.t, len(cols), 4, line)
			status = cols[3]
		}
	}

	// An empty cell is a dash in the table.
	if status == "-" {
		status = ""
	}

	return shows, status
}

const (
	rigFailed = `{"session_id":"sid-dev-0001","hook_event_name":"StopFailure","error":"rate_limit",` +
		`"last_assistant_message":"You've hit your limit · resets 3:00pm (UTC)"}`
	rigAsked = `{"session_id":"sid-dev-0001","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion",` +
		`"tool_input":{"questions":[{"question":"Which screen first?","header":"Screen","multiSelect":false,"options":[` +
		`{"label":"Home","description":"the landing page"},{"label":"Talk","description":"the conversation"}]}]}}`
	rigAskedScreen = " ☐ Screen\n│ Which screen first?\n❯ 1. Home\n     the landing page\n  2. Talk\n     the conversation\n" +
		"  3. Type something.\n  4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"
)

// What zmina ls shows for a role and what zmina explain says it shows are one thing, over every golden screen under every
// status, with and without an input line, and over each hook record and session file fixture: both read the one look at the
// role, and say its status with the same functions.
func TestLsAndExplainAgreeOnTheStatus(t *testing.T) {
	r := newExplainRig(t)

	screens := corpus(t)

	var names []string
	for name := range screens {
		names = append(names, name)
	}

	sort.Strings(names)

	sessions := []struct {
		name       string
		set        func()
		wantSource shift.Source
	}{
		{"idle", func() { r.session("idle", "") }, shift.SourceRegistry},
		{"busy", func() { r.session("busy", "") }, shift.SourceRegistry},
		{"waiting permission", func() { r.session("waiting", "permission prompt") }, shift.SourceRegistry},
		{"no session file", r.noSession, ""},
	}

	checked := 0

	for _, name := range names {
		for _, s := range sessions {
			for _, noInput := range []bool{false, true} {
				r.tm.screen, r.tm.noInput = screens[name], noInput
				s.set()

				out := r.explainJSON()
				shows, status := r.lsCells()
				label := name + " / " + s.name

				assert.Equal(t, shows, out.Shows, label+": the state cell of the compact ls")
				assert.Equal(t, status, out.Status, label+": the STATUS column of ls --wide")

				if s.wantSource != "" {
					assert.Equal(t, s.wantSource, out.Decided.Source, label)
				}

				checked++
			}
		}
	}

	t.Logf("%d screens, %d fixtures of status and input line, each read by ls twice and explain once", len(names), checked)

	// The hook records and the session file's own failures.
	r.tm.screen, r.tm.noInput = "", false

	hook := map[string]func(){
		"a failed turn the session is idle since": func() {
			r.session("idle", "")
			_, err := shift.RecordFailure(crewLayout(t, r), "dev", []byte(rigFailed), r.now)
			require.NoError(t, err)
		},
		"a failed turn of an earlier turn": func() {
			r.session("idle", "")
			_, err := shift.RecordFailure(crewLayout(t, r), "dev", []byte(rigFailed), r.now.Add(-time.Minute))
			require.NoError(t, err)
		},
		"a question asked": func() {
			r.session("waiting", "input needed")
			r.tm.screen, r.tm.noInput = rigAskedScreen, true
			require.NoError(t, shift.RecordQuestion(crewLayout(t, r), "dev", []byte(rigAsked), r.now))
		},
		"a session file cut short": func() {
			require.NoError(t, os.WriteFile(filepath.Join(r.ns, "sessions", strconv.Itoa(r.pid)+".json"), []byte(`{"pid": 4242, "sessionId": "sid-`), 0o600))
		},
	}

	var kinds []string
	for k := range hook {
		kinds = append(kinds, k)
	}

	sort.Strings(kinds)

	for _, k := range kinds {
		_ = os.RemoveAll(crewLayout(t, r).Hooks())

		r.tm.screen, r.tm.noInput = "", false
		hook[k]()

		out := r.explainJSON()
		shows, status := r.lsCells()

		assert.Equal(t, shows, out.Shows, k)
		assert.Equal(t, status, out.Status, k)
	}
}

func crewLayout(t *testing.T, r *explainRig) crew.Layout {
	t.Helper()

	return crew.Layout{Dir: filepath.Dir(r.crewFile)}
}

// The text says, in plain words, the status shown, who decided it with what evidence, and who was consulted and lost.
func TestExplainSaysWhoDecidedAndWhoLost(t *testing.T) {
	r := newExplainRig(t)
	r.session("idle", "")
	r.now = r.now.Add(4 * time.Minute)

	_, err := shift.RecordFailure(crewLayout(t, r), "dev", []byte(rigFailed), r.now.Add(-4*time.Minute))
	require.NoError(t, err)

	r.tm.screen = "⏺ Working on the exporter.\n⎿ 5-hour limit reached ∙ resets 3pm\n❯ \n"

	out := goldenExplain(r.mustRun("explain", "dev"))

	for _, want := range []string{
		"dev: up idle (last turn ended in an API error, usage limit, resets Sat 15:00 UTC, queue held until 15:00 UTC: rate_limit: You've hit your limit",
		"status shown: idle (last turn ended in an API error, usage limit",
		"decided by hook record StopFailure: its last turn ended in an API error: rate_limit: You've hit your limit · resets 3:00pm (UTC) [record of session sid-dev- written by the StopFailure hook] at 12:00:00 (4m ago)",
		"consulted and not decisive, in the order they are trusted (hook record, session file, screen):",
		"  - session file status: its Claude Code session file says it is idle [status=idle, pid=PID] at 12:00:00 (4m ago) — not decisive: its word is kept (idle); the hook's record adds the error to it",
		"  - screen rule claude.input.line: an input line is on the screen: the role is at its prompt or in a turn, not at a dialog — not decisive: it does not change the status",
		`screen: rule claude.limit.notice matched "⎿ 5-hour limit reached ∙ resets 3pm": a usage-limit notice is on its last rows: Claude Code tells that a usage limit stops the role`,
		`screen: last said "Working on the exporter." since 12:04:00`,
		"queue: held for a failed turn (rate_limit: You've hit your limit · resets 3:00pm (UTC)): a usage limit: the lines queued for it wait until Sat 15:00 UTC (at least 5m, at most 6h); 0 lines waiting",
		"context: its conversation size is not known; context_max off; restart_at 250k; restart_done 150k",
	} {
		assert.Contains(t, out, want)
	}
}

func TestExplainSaysWhyTheStatusIsWhatItIsWithNoHook(t *testing.T) {
	r := newExplainRig(t)
	r.session("waiting", "input needed")
	r.tm.screen, r.tm.noInput = rigAskedScreen, true
	require.NoError(t, shift.RecordQuestion(crewLayout(t, r), "dev", []byte(rigAsked), r.now))

	out := goldenExplain(r.mustRun("explain", "dev"))
	assert.Contains(t, out, "dev: up waiting (question) (dialog)")
	assert.Contains(t, out, "decided by session file status: its Claude Code session file says it is waiting on a question put to the owner [status=waiting, waitingFor=input needed, pid=PID] at 12:00:00")
	assert.Contains(t, out, "  - hook record StopFailure: nothing recorded for this session — not decisive: no record")
	assert.Contains(t, out, `screen: a dialog is shown, its words from the hook record: "Which screen first?" (2 choices)`)
	assert.Contains(t, out, "  - screen rule claude.input.line: no input line is on the screen: a dialog, a login or a screen being drawn [Enter to select · ↑/↓ to navigate · Esc to cancel] — not decisive: it adds the (dialog) mark ls shows, not the status word")
	assert.Contains(t, out, "queue: 0 lines waiting; it waits on a dialog: nothing is typed into a dialog")
}

// A role that is down says so, with what is kept of it and the newest event about it.
func TestExplainSaysARoleThatIsDown(t *testing.T) {
	r := newExplainRig(t)
	r.tm.absent = true

	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(r.crewFile), "events.jsonl"),
		[]byte(`{"ts":"2026-10-23T09:30:00Z","role":"dev","event":"ended","detail":"dev: its process ended after 2h"}`+"\n"+
			`{"ts":"2026-10-23T09:31:00Z","role":"dev","event":"queued","detail":"a line"}`+"\n"+
			`{"ts":"2026-10-23T09:32:00Z","role":"other","event":"ended","detail":"not this role"}`+"\n"), 0o600))

	out := r.mustRun("explain", "dev")
	assert.Equal(t, "dev: down: no session; last known: never started under this state; last event: ended 10-23 09:30 (dev: its process ended after 2h)\n"+
		"queue: 0 lines waiting\n", out)

	asJSON := r.explainJSON()
	assert.Equal(t, shift.PresenceDown, asJSON.State)
	assert.Equal(t, "down: no session", asJSON.Summary)
	require.NotNil(t, asJSON.LastEvent)
	assert.Equal(t, "ended", asJSON.LastEvent.Event)
}

// goldenExplain is what a command printed with the one thing that differs from run to run (the pid of the test process, which
// stands for the pane's) made the same.
func goldenExplain(out string) string {
	return regexp.MustCompile(`pid=\d+`).ReplaceAllString(out, "pid=PID")
}

var updateExplainGolden = flag.Bool("update", false, "rewrite cmd/testdata/explain from what explain prints")

// goldenDir is where the goldens are, found before any command a test runs can change the directory.
var goldenDir = func() string {
	dir, _ := filepath.Abs(filepath.Join("testdata", "explain"))

	return dir
}()

func assertGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join(goldenDir, name)

	if *updateExplainGolden {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), got, "%s: go test ./cmd -run TestExplainOutputIsStable -update records it", name)
}

// The text and the JSON of an explanation are pinned for two roles: one up, idle since a turn that ended on a usage limit, with
// the notice on its screen; one that is down. `why` is the same command as `explain`.
func TestExplainOutputIsStable(t *testing.T) {
	r := newExplainRig(t)
	r.session("idle", "")
	r.now = r.now.Add(4 * time.Minute)

	_, err := shift.RecordFailure(crewLayout(t, r), "dev", []byte(rigFailed), r.now.Add(-4*time.Minute))
	require.NoError(t, err)

	r.tm.screen = "⏺ Working on the exporter.\n⎿ 5-hour limit reached ∙ resets 3pm\n❯ \n"

	assertGolden(t, "up-limit.txt", goldenExplain(r.mustRun("explain", "dev")))
	assertGolden(t, "up-limit.json", goldenExplain(r.mustRun("why", "dev", "--json")))

	r = newExplainRig(t)
	r.tm.absent = true

	assertGolden(t, "down.txt", goldenExplain(r.mustRun("explain", "dev")))
	assertGolden(t, "down.json", goldenExplain(r.mustRun("explain", "dev", "--json")))
}

// A role the crew does not have is an error that says so; the command takes one role.
func TestExplainNeedsARoleOfTheCrew(t *testing.T) {
	r := newExplainRig(t)

	_, _, err := r.run("explain", "nobody")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no role "nobody" in the crew`)

	_, _, err = r.run("explain")
	require.Error(t, err)

	_, _, err = r.run("explain", "dev", "dispatcher")
	require.Error(t, err)
}

// Explaining is a look: it takes no state lock (one held by a long command does not hold it up, as with ls), writes no state
// and presses no key.
func TestExplainTakesNoLockAndWritesNothing(t *testing.T) {
	r := newExplainRig(t)
	r.session("busy", "")
	r.tm.screen = rigAskedScreen

	stateDir := filepath.Dir(r.crewFile)

	held, err := shift.LockAs(stateDir, shift.LockOpts{Verb: "test"})
	require.NoError(t, err)

	t.Cleanup(held.Release)

	before, _ := os.ReadFile(filepath.Join(stateDir, "state.json"))

	done := make(chan string, 1)

	go func() { done <- r.mustRun("explain", "dev") }()

	select {
	case out := <-done:
		assert.Contains(t, out, "dev: up busy")
	case <-time.After(10 * time.Second):
		t.Fatal("explain waited for the state lock")
	}

	after, _ := os.ReadFile(filepath.Join(stateDir, "state.json"))
	assert.Equal(t, string(before), string(after), "the state file as it was")
}

// The cells explain prints for the status and the state — and says again in --json — quote what a file or the transcript
// wrote into them: the words of a failed turn, a status the session file kept. No control character, no session tag, a bound.
func TestExplainQuotesTheStatusCells(t *testing.T) {
	r := newExplainRig(t)
	r.session("idle", "")

	msg := "API Error: 500 \x1b[31mred\x1b[0m\x7f\u009b [zmina:abcDEF123456] [zmina:ghiJKL\n789012] " + strings.Repeat("w", 80)
	raw, err := json.Marshal(map[string]string{"session_id": "sid-dev-0001", "hook_event_name": "StopFailure", "error": "server_error",
		"last_assistant_message": msg})
	require.NoError(t, err)

	_, err = shift.RecordFailure(crewLayout(t, r), "dev", raw, r.now)
	require.NoError(t, err)

	clean := func(label, text string) {
		t.Helper()

		for _, bad := range []string{"\x1b", "\x7f", "\u009b", "abcDEF123456", "ghiJKL", "789012"} {
			assert.NotContains(t, text, bad, label)
		}
	}

	out := r.explainJSON()
	assert.Contains(t, out.Status, "last turn ended in an API error", "the cell says what it did")
	clean("json status", out.Status)
	clean("json shows", out.Shows)

	text := r.mustRun("explain", "dev")
	assert.Contains(t, text, "status shown: idle (last turn ended in an API error")
	clean("text", text)
}
