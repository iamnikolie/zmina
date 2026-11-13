package shift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/screenrules"
)

// look is the view and the explanation of the fixture's designer.
func look(t *testing.T, f fix) (View, Explained) {
	t.Helper()

	v, why, ok := f.s.Look("designer")
	require.True(t, ok)

	return v, f.s.Explain(v, why)
}

// idleRole is a fixture with the designer up and idle since now.
func idleRole(t *testing.T) fix {
	t.Helper()

	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)

	return f
}

// The explanation is of the very look Views takes: the view Look returns is the one Views lists, and the source Explain says
// decided is the one the view's own fields show — a hook's error note, else the session file's status, else the screen's
// dialog mark — for every golden screen under every status the session file may give.
func TestExplainIsOfTheLookViewsTakes(t *testing.T) {
	statuses := map[string]func(f fix){
		"idle":               func(f fix) { f.status("designer", "idle", *f.now) },
		"busy":               func(f fix) { f.status("designer", "busy", *f.now) },
		"waiting permission": func(f fix) { f.waitingOn("designer", "permission prompt") },
		"waiting question":   func(f fix) { f.waitingOn("designer", "input needed") },
		"no session file":    func(f fix) { delete(f.rg.byPID, f.tm.up["designer"]) },
		"no status":          func(f fix) { f.rg.byPID[f.tm.up["designer"]] = &registry.Session{SessionID: "sid-designer"} },
	}

	var names []string
	for name := range screens {
		names = append(names, name)
	}

	sort.Strings(names)

	cases := 0

	for _, name := range names {
		for status, set := range statuses {
			for _, inputLine := range []bool{true, false} {
				f := fixtureWith(t, crewYAML)
				require.NoError(t, f.s.Up("designer"))
				set(f)
				f.tm.screen = loadScreen(t, name)

				if !inputLine {
					f.tm.drafts["designer"] = "-"
				}

				views := f.s.Views()
				var listed View
				for _, v := range views {
					if v.Role == "designer" {
						listed = v
					}
				}

				v, e := look(t, f)
				label := name + " / " + status

				assert.Equal(t, listed, v, label+": Look is the view Views lists")
				assert.Equal(t, PresenceUp, e.State, label)
				assert.NotEmpty(t, e.Summary, label)

				switch {
				case v.TurnError != "":
					assert.Equal(t, SourceHook, e.Decided.Source, label)
				case v.Status != "":
					assert.Equal(t, SourceRegistry, e.Decided.Source, label)
				case v.Prompt:
					assert.Equal(t, SourceScreen, e.Decided.Source, label)
					assert.Equal(t, screenrules.BuiltinInputLine, e.Decided.Kind, label)
				default:
					assert.Equal(t, SourceNone, e.Decided.Source, label)
				}

				// Every source consulted but not decisive says why, and the decider is not among them.
				for _, l := range e.Lost {
					assert.NotEmpty(t, l.Why, label+": "+string(l.Source)+" "+l.Kind)
					assert.NotEqual(t, e.Decided.Source, l.Source, label)
				}

				cases++
			}
		}
	}

	t.Logf("%d screens, %d fixtures of the session file and the input line", len(names), cases)
	assert.GreaterOrEqual(t, cases, 36*6*2)
}

// What a hook's record says decides the status over the session file, while the record is of the turn the session is idle
// since; the session file's word is kept, and the record that is not that turn's is a source that lost, with why.
func TestExplainNamesTheHookRecordThatDecided(t *testing.T) {
	f := idleRole(t)
	f.failed(t, "designer", failedInput, f.now.Add(-hookSkew/2))
	*f.now = f.now.Add(7 * time.Minute)

	v, e := look(t, f)
	require.NotEmpty(t, v.TurnError)

	assert.Equal(t, SourceHook, e.Decided.Source)
	assert.Equal(t, "StopFailure", e.Decided.Kind)
	assert.Contains(t, e.Decided.Says, "ended in an API error: model_not_found")
	assert.Equal(t, 7*60, e.Decided.AgeSeconds, "its age, from the look's own clock")
	assert.Contains(t, e.Decided.Evidence, "record of session sid-desi")

	require.Len(t, e.Lost, 2)
	assert.Equal(t, SourceRegistry, e.Lost[0].Source)
	assert.Equal(t, "status", e.Lost[0].Kind)
	assert.Contains(t, e.Lost[0].Says, "says it is idle")
	assert.Contains(t, e.Lost[0].Evidence, "status=idle")
	assert.Contains(t, e.Lost[0].Why, "its word is kept")
	assert.Equal(t, SourceScreen, e.Lost[1].Source)
	assert.Equal(t, screenrules.BuiltinInputLine, e.Lost[1].Kind)
}

func TestExplainSaysWhyARecordDidNotDecide(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f fix)
		says string
		why  string
	}{
		"no record": {func(f fix) {}, "nothing recorded for this session", "no record"},
		"another session": {func(f fix) {
			f.failed(t, "designer", strings.Replace(failedInput, "sid-designer", "sid-before-a-clear", 1), *f.now)
		}, "nothing recorded for this session", "another session"},
		"an older turn": {func(f fix) {
			f.failed(t, "designer", failedInput, f.now.Add(-time.Minute))
		}, "ended in an API error", "went idle at"},
		"the next turn started": {func(f fix) {
			f.failed(t, "designer", failedInput, *f.now)
			f.rg.byPID[f.tm.up["designer"]].Status = "busy"
		}, "ended in an API error", "the session is busy now"},
	} {
		t.Run(name, func(t *testing.T) {
			f := idleRole(t)
			tc.set(f)

			v, e := look(t, f)
			assert.Empty(t, v.TurnError)
			assert.Equal(t, SourceRegistry, e.Decided.Source)

			require.NotEmpty(t, e.Lost)
			assert.Equal(t, SourceHook, e.Lost[0].Source, "the hook is consulted first and lost")
			assert.Contains(t, e.Lost[0].Says, tc.says)
			assert.Contains(t, e.Lost[0].Why, tc.why)
		})
	}
}

// A role at a dialog: the session file says what it waits on and decides the status; the screen's missing input line is the
// mark ls adds, and the question's words come from the hook's record while that is the question on the screen.
func TestExplainSaysWhereADialogsWordsCameFrom(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "input needed")
	f.tm.screen = askedScreen
	require.NoError(t, RecordQuestion(f.s.Crew.Layout(), "designer", []byte(askedInput), *f.now))

	v, e := look(t, f)
	assert.Equal(t, "waiting", v.Status)
	assert.True(t, v.Prompt)

	assert.Equal(t, SourceRegistry, e.Decided.Source)
	assert.Contains(t, e.Decided.Says, "waiting on a question put to the owner")
	assert.Contains(t, e.Decided.Evidence, "waitingFor=input needed")

	require.NotNil(t, e.Screen.Dialog)
	assert.Equal(t, SourceHook, e.Screen.Dialog.From, "the recorded question is the one on the screen")
	assert.Equal(t, "Which screen first?", e.Screen.Dialog.Question)
	assert.Equal(t, 2, e.Screen.Dialog.Options)

	// The screen's no input line adds the mark; it is not the status word.
	last := e.Lost[len(e.Lost)-1]
	assert.Equal(t, SourceScreen, last.Source)
	assert.Contains(t, last.Why, "(dialog) mark")
	assert.Contains(t, last.Says, "no input line")
	assert.Contains(t, last.Evidence, "Esc to cancel", "the last row of the screen")

	// A record of an earlier question is not the one on the screen: the screen's reading is shown, and why.
	f.rg.byPID[f.tm.up["designer"]].StatusUpdatedAt = f.now.Add(time.Minute).UnixMilli()
	_, e = look(t, f)
	require.NotNil(t, e.Screen.Dialog)
	assert.Equal(t, SourceScreen, e.Screen.Dialog.From)
	assert.Contains(t, e.Screen.Dialog.Why, "a record of an earlier question")
}

// A session file Claude Code has not written yet and a screen that shows no input line: the screen rule decides; with an
// input line, nothing does; and a file that does not read decides "unknown" in its own words.
func TestExplainLetsTheScreenDecideOnlyWhenNothingElseSpeaks(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	delete(f.rg.byPID, f.tm.up["designer"])
	f.tm.screen = "Do you trust the files in this folder?\n❯ 1. Yes, proceed\n  2. No, exit\nEnter to confirm · Esc to cancel"
	f.tm.drafts["designer"] = "-"

	v, e := look(t, f)
	assert.Empty(t, v.Status)
	assert.True(t, v.Prompt)
	assert.Equal(t, SourceScreen, e.Decided.Source)
	assert.Equal(t, screenrules.BuiltinInputLine, e.Decided.Kind)
	assert.Contains(t, e.Decided.Says, "no input line")
	assert.Contains(t, e.Decided.Evidence, "Enter to confirm")
	assert.Equal(t, "its Claude Code session file is not there for its process yet", e.Lost[1].Says)
	assert.Equal(t, "it gave no status", e.Lost[1].Why)

	f.tm.drafts["designer"] = ""
	v, e = look(t, f)
	assert.False(t, v.Prompt)
	assert.Equal(t, SourceNone, e.Decided.Source)
}

// A usage-limit notice on the last rows is read by the rule that says so, and shown with the row and the line; ls does not
// show it, so it is not what decided the status.
func TestExplainQuotesTheScreenRuleAndTheLineItMatched(t *testing.T) {
	f := idleRole(t)
	f.tm.screen = loadScreen(t, "claude/limit-five-hour")

	_, e := look(t, f)
	assert.Equal(t, SourceRegistry, e.Decided.Source)

	require.NotNil(t, e.Screen.Limit)
	assert.Equal(t, "claude.limit.notice", e.Screen.Limit.Kind)
	assert.Equal(t, "⎿ 5-hour limit reached ∙ resets 9pm", e.Screen.Limit.Evidence)
	assert.Contains(t, e.Screen.Limit.Says, "usage-limit notice")
	assert.Contains(t, e.Screen.Limit.Why, "not part of the status")

	// A control character in a matched line never reaches the output, and a long line is cut.
	f.tm.screen = "ok\n5-hour limit reached \x1b[31m\x07" + strings.Repeat("x", 400)

	_, e = look(t, f)
	require.NotNil(t, e.Screen.Limit)
	assert.NotContains(t, e.Screen.Limit.Evidence, "\x1b")
	assert.NotContains(t, e.Screen.Limit.Evidence, "\x07")
	assert.LessOrEqual(t, len([]rune(e.Screen.Limit.Evidence)), evidenceMax)

	// The rules of the crew are the ones quoted: an added wording is named by its own id.
	path := filepath.Join(t.TempDir(), "screen-rules.yaml")
	require.NoError(t, os.WriteFile(path, []byte("rules:\n  - id: acme.limit.wording\n    engine: claude\n    sets: usage_limit\n"+
		"    means: a newer wording\n    match: {contains: plan exhausted}\n"), 0o600))

	table, o := screenrules.LoadOverrides(screenrules.Default(), path)
	require.NoError(t, o.Err)
	f.s.Rules = table

	f.tm.screen = "⎿ plan exhausted until 9pm"
	_, e = look(t, f)
	require.NotNil(t, e.Screen.Limit)
	assert.Equal(t, "acme.limit.wording", e.Screen.Limit.Kind)
}

func TestExplainSaysWhatStandsInTheInputLine(t *testing.T) {
	f := idleRole(t)

	for name, tc := range map[string]struct {
		draft, suggestion, want string
	}{
		"empty":      {"", "", "empty"},
		"suggestion": {"", "run the tests", "empty (a dim suggestion is shown, not typed)"},
		"a draft":    {"rename the helper", "", "holds a draft of 17 characters"},
	} {
		t.Run(name, func(t *testing.T) {
			f.tm.drafts["designer"], f.tm.suggestions["designer"] = tc.draft, tc.suggestion

			_, e := look(t, f)
			assert.Equal(t, tc.want, e.Screen.Draft)

			if tc.draft != "" {
				assert.NotContains(t, mustJSON(t, e), tc.draft, "the draft's own words are not quoted: only its length")
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	return string(raw)
}

// A session file that does not read is "unknown", and the registry says it in its own words.
func TestExplainSaysAnUnreadableSessionFile(t *testing.T) {
	f := idleRole(t)

	v, why, ok := f.s.Look("designer")
	require.True(t, ok)

	why.SessionErr = &registry.BadFileError{Path: "/ns/sessions/4242.json", Empty: true}
	why.Session = nil
	v.Status, v.StatusError = "unknown", why.SessionErr.Error()

	e := f.s.Explain(v, why)
	assert.Equal(t, SourceRegistry, e.Decided.Source)
	assert.Equal(t, "session file", e.Decided.Kind)
	assert.Contains(t, e.Decided.Says, "does not read")
	assert.Contains(t, e.Decided.Evidence, "4242.json")
}

// A role that is down says so, and what is kept of it; a role tmux cannot be asked about says that.
func TestExplainWorksForARoleThatIsDown(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	rs := f.s.State.Role("designer")
	rs.Started = f.now.Add(-time.Hour)
	rs.LastSession = "1a2b3c4d5e6f"
	rs.Held = true

	v, e := look(t, f)
	assert.Equal(t, PresenceDown, v.State)
	assert.Equal(t, PresenceDown, e.State)
	assert.Equal(t, "down: no session", e.Summary)
	assert.Equal(t, SourceTmux, e.Decided.Source)
	assert.Empty(t, e.Lost, "no session, no source to consult")

	require.NotNil(t, e.LastKnown)
	assert.Equal(t, "1a2b3c4d", e.LastKnown.LastSession)
	assert.True(t, e.LastKnown.Held)
	assert.Contains(t, e.LastKnown.Words, "last started 2026-10-24T11:00:00Z")
	assert.Contains(t, e.LastKnown.Words, "on conversation 1a2b3c4d")
	assert.Contains(t, e.LastKnown.Words, "put down by hand")

	f.tm.probeErr["designer"] = os.ErrDeadlineExceeded

	_, e = look(t, f)
	assert.Equal(t, PresenceUnknown, e.State)
	assert.Contains(t, e.Summary, "tmux could not be asked")
}

// The lines queued for a role: what holds them and until when, how many wait, the line typed and not landed, a settling
// slash command — all from what the delivery itself reads.
func TestExplainSaysWhatHoldsTheQueue(t *testing.T) {
	f, _, failedAt := heldFixture(t, limitInput(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC).Add(3*time.Hour)))
	_ = failedAt

	v, e := look(t, f)
	require.NotNil(t, e.Queue.Held, "a limit with a reset time holds the queue")
	assert.Equal(t, 2, e.Queue.Lines)
	assert.Equal(t, "limit", e.Queue.Held.Kind)
	assert.Contains(t, e.Queue.Held.Why, "a usage limit: the lines queued for it wait until")
	assert.True(t, e.Queue.Held.Until.Equal(v.TurnErrorHeldUntil), "the very instant ls shows")
	assert.Equal(t, SourceHook, e.Decided.Source)

	// And the delivery's own list of held queues (zmina doctor) says the same of the role.
	holds := f.s.FailureHolds(f.s.Now())
	require.Len(t, holds, 1)
	assert.Equal(t, "designer", holds[0].Role)
	assert.True(t, holds[0].Until.Equal(e.Queue.Held.Until), "the instant doctor shows")
	assert.Equal(t, holds[0].Text, e.Queue.Held.Text)

	// The hold ends: not held any more, and ls, doctor and explain agree on it.
	*f.now = f.now.Add(4 * time.Hour)
	f.rg.byPID[f.tm.up["designer"]].StatusUpdatedAt = f.now.UnixMilli()

	v, e = look(t, f)
	assert.Nil(t, e.Queue.Held)
	assert.True(t, v.TurnErrorHeldUntil.IsZero())
	assert.Empty(t, f.s.FailureHolds(f.s.Now()))

	rs := f.s.State.Role("designer")
	rs.Sent = &Sent{Line: "[dispatcher] first\nsecond", At: *f.now}
	rs.SlashAt = *f.now

	_, e = look(t, f)
	require.NotNil(t, e.Queue.InFlight)
	assert.Equal(t, InFlightFact{Kind: "line", Length: len([]rune("[dispatcher] first\nsecond"))}, *e.Queue.InFlight)
	assert.Contains(t, e.Queue.Waits, "a line typed into it has not landed yet")
	assert.Contains(t, e.Queue.Waits, "a slash command typed into it is settling")
	assert.True(t, e.Queue.SettlingUntil.Equal(f.now.Add(slashSettle)))
}

func TestExplainPutsTheContextAgainstItsThresholds(t *testing.T) {
	f := fixtureWith(t, crewYAML+"context_max: 150k\n")
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.rg.context["sid-designer"] = 362_000

	v, e := look(t, f)
	assert.Equal(t, 362_000, v.Context)
	assert.Equal(t, 362_000, e.Context.Tokens)
	assert.Equal(t, 150_000, e.Context.ContextMax)
	assert.Equal(t, 250_000, e.Context.RestartAt)
	assert.Equal(t, 150_000, e.Context.RestartDone)
	assert.Contains(t, e.Context.Words, "362k tokens")
	assert.Contains(t, e.Context.Words, "context_max 150k: over it, the ticker types /compact")
	assert.Contains(t, e.Context.Words, "restart_at 250k: over it")

	f.rg.context["sid-designer"] = 40_000
	_, e = look(t, f)
	assert.Contains(t, e.Context.Words, "context_max 150k: under it")
}

func TestExplainSaysWhatRestartIsPending(t *testing.T) {
	f := idleRole(t)
	rs := f.s.State.Role("designer")
	rs.Pending = &PendingDown{Kind: pendingRestart, Claim: *f.now, Why: "past restart_at"}
	rs.RestartAsked, rs.RestartWhy = f.now.Add(-time.Minute), "restart_at"
	rs.RestartDue, rs.TrimPending = "sid-designer", true

	_, e := look(t, f)
	assert.Equal(t, "restart: past restart_at", e.Restart.Pending)
	assert.True(t, e.Restart.Due)
	assert.True(t, e.Restart.Trim)
	assert.Contains(t, e.Restart.Words, "a restart the ticker began")
	assert.Contains(t, e.Restart.Words, "asked for its note before a restart")
	assert.Contains(t, e.Restart.Words, "a /compact or /clear typed into it has not shown yet")
}

// Looking changes nothing: the state, the hook records and the registry are as they were.
func TestExplainChangesNothing(t *testing.T) {
	f := heldFixtureFor(t)
	f.tm.screen = loadScreen(t, "claude/limit-five-hour")
	f.s.State.Role("designer").Sent = &Sent{Line: "x", At: *f.now}

	before := mustJSON(t, f.s.State)
	keysBefore := len(f.tm.keys["designer"])
	typedBefore := len(f.tm.typed["designer"])

	for range 3 {
		look(t, f)
	}

	assert.Equal(t, before, mustJSON(t, f.s.State), "the state as it was")
	assert.Len(t, f.tm.keys["designer"], keysBefore, "no key pressed")
	assert.Len(t, f.tm.typed["designer"], typedBefore, "nothing typed")
}

func heldFixtureFor(t *testing.T) fix {
	t.Helper()

	f, _, _ := heldFixture(t, limitInput(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC).Add(3*time.Hour)))

	return f
}

// A Codex role's waiting comes from its bridge's status file, not from the screen: the screen rule says it was not read, and
// names the bridge's marker, not the Claude input line.
func TestExplainOfACodexRoleDoesNotReadTheInputLine(t *testing.T) {
	r := Reasoning{Engine: screenrules.Codex}
	said := r.inputSaid()

	assert.Equal(t, SourceScreen, said.Source)
	assert.Equal(t, screenrules.BuiltinBridgeMode, said.Kind)
	assert.Contains(t, said.Says, "not read")

	assert.Equal(t, "its Codex bridge's status file is not there for its process yet", Reasoning{Engine: screenrules.Codex}.registrySaid(View{}).Says)
	assert.Equal(t, "its Claude Code session file is not there for its process yet", Reasoning{Engine: screenrules.Claude}.registrySaid(View{}).Says)
}

// recordingTmux records how many rows each capture asked for.
type recordingTmux struct {
	*fakeTmux
	asked []int
}

func (r *recordingTmux) Capture(name string, n int) (string, error) {
	r.asked = append(r.asked, n)

	return r.fakeTmux.Capture(name, n)
}

// The ticker and zmina explain read the usage-limit notice out of the same rows: the rows the rule names, which the ticker
// captures and explain cuts out of its larger capture. A row's reach is bounded by that capture (the loader refuses more), so
// the two cannot differ.
func TestExplainAndTheWatchReadTheLimitNoticeFromTheSameRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "screen-rules.yaml")
	require.NoError(t, os.WriteFile(path, []byte("rules:\n  - id: acme.limit.wording\n    engine: claude\n    sets: usage_limit\n"+
		"    means: a newer wording\n    match: {contains: plan exhausted}\n    region: {last_lines: 20}\n"), 0o600))

	table, o := screenrules.LoadOverrides(screenrules.Default(), path)
	require.NoError(t, o.Err)

	filler := func(n int) string { return strings.Repeat("work\n", n) }

	for name, tc := range map[string]struct {
		screen string
		seen   bool
	}{
		"inside the rule's rows":  {"⎿ plan exhausted until 9pm\n" + filler(18), true},
		"just outside them":       {"⎿ plan exhausted until 9pm\n" + filler(20), false},
		"far above them":          {"⎿ plan exhausted until 9pm\n" + filler(60), false},
		"at the top of a capture": {"⎿ plan exhausted until 9pm\n" + filler(79), false},
	} {
		t.Run(name, func(t *testing.T) {
			f := idleRole(t)
			f.s.Rules = table
			rec := &recordingTmux{fakeTmux: f.tm}
			f.s.Tmux = rec
			f.tm.screen = strings.TrimRight(tc.screen, "\n")

			*f.now = f.now.Add(10 * time.Minute)
			notes := f.s.watchScreen("designer", *f.now)
			_, e := look(t, f)

			assert.Equal(t, tc.seen, e.Screen.Limit != nil, "explain")
			assert.Equal(t, tc.seen, len(notes) == 1, "the ticker's watch: %v", notes)
			assert.Contains(t, rec.asked, 20, "the ticker captures just the rows the rule names")
			assert.Contains(t, rec.asked, screenrules.MaxLastLines, "explain captures the most a rule may name")
		})
	}
}

// everything is what explain says of a role, as the owner would read it, text and JSON together.
func everything(t *testing.T, e Explained) string { return mustJSON(t, e) }

// A session tag — the random [zmina:<12 characters>] a role's lines carry — is never quoted by explain: not in a screen row,
// not in the words last said, not in a question, not in an event's words.
func TestExplainRedactsSessionTags(t *testing.T) {
	f := idleRole(t)

	f.tm.screen = strings.Join([]string{
		"⏺ [zmina:aaaaaaaaaaaa] I read the line and went on.",
		"⎿ 5-hour limit reached [zmina:bbbbbbbbbbbb] resets 9pm",
		"❯ ",
	}, "\n")

	_, e := look(t, f)
	out := everything(t, e)

	assert.Contains(t, out, "[zmina:…]", "the tag is marked as there, not shown")

	for _, tag := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		assert.NotContains(t, out, tag)
	}

	// The last row of a screen with no input line, and a dialog's question.
	f2 := fixtureWith(t, crewYAML)
	require.NoError(t, f2.s.Up("designer"))
	f2.waitingOn("designer", "input needed")
	f2.tm.screen = "Which one [zmina:cccccccccccc]?\n❯ 1. Yes\n  2. No\nEnter to select · [zmina:dddddddddddd] · Esc to cancel"

	_, e2 := look(t, f2)
	out2 := everything(t, e2)

	for _, tag := range []string{"cccccccccccc", "dddddddddddd"} {
		assert.NotContains(t, out2, tag)
	}

	// Only the tag's own form is redacted: a word that is not one stays.
	assert.Equal(t, "[zmina:…] and [zmina:short] and zmina:aaaaaaaaaaaa", Quote("[zmina:aaaaaaaaaaaa] and [zmina:short] and zmina:aaaaaaaaaaaa"))
	assert.Equal(t, "[zmina:…]", Quote("[zmina:aa\x00aaaaaaaaaa]"), "stripped of control characters first, so a tag cannot hide in them")
}

// The line typed into a role that has not landed is not printed at all: its kind and its length only.
func TestExplainDoesNotPrintTheLineInFlight(t *testing.T) {
	f := idleRole(t)
	f.s.State.Role("designer").Sent = &Sent{Line: "[dispatcher] the owner's private words [zmina:eeeeeeeeeeee]", At: *f.now}

	_, e := look(t, f)
	out := everything(t, e)

	assert.NotContains(t, out, "private words")
	assert.NotContains(t, out, "dispatcher")
	assert.NotContains(t, out, "eeeeeeeeeeee")
	require.NotNil(t, e.Queue.InFlight)
	assert.Equal(t, "line", e.Queue.InFlight.Kind)
	assert.Equal(t, len([]rune("[dispatcher] the owner's private words [zmina:eeeeeeeeeeee]")), e.Queue.InFlight.Length)
}

// What explain quotes from the session file and from a crew's override — a status, a row's words — goes through the same
// stripping and bound as a screen line.
func TestExplainBoundsWhatAFileAndAnOverrideSay(t *testing.T) {
	f := idleRole(t)
	f.rg.byPID[f.tm.up["designer"]].Status = "idle\x1b[31m\a" + strings.Repeat("s", 400)
	f.rg.byPID[f.tm.up["designer"]].WaitingFor = "x\x1b]0;title\a" + strings.Repeat("w", 400)

	rules, err := screenrules.NewTable([]screenrules.Rule{{ID: "claude.test.limit", Engine: screenrules.Claude, Sets: screenrules.UsageLimit,
		Means: "a\x1b[31m wording " + strings.Repeat("m", 400), Match: screenrules.Match{Contains: "limit"}}})
	require.NoError(t, err)

	f.s.Rules = rules
	f.tm.screen = "5-hour limit reached"

	v, e := look(t, f)
	require.NotNil(t, e.Screen.Limit)
	assert.NotContains(t, everything(t, e), "\x1b")
	assert.NotContains(t, everything(t, e), "\a")

	for name, said := range map[string]string{"registry says": e.Decided.Says, "evidence": e.Decided.Evidence, "limit says": e.Screen.Limit.Says,
		"limit kind": e.Screen.Limit.Kind, "summary": e.Summary} {
		assert.LessOrEqual(t, len([]rune(said)), 320, name)
	}

	assert.Contains(t, v.Status, "idle")

	for _, w := range e.Queue.Waits {
		assert.LessOrEqual(t, len([]rune(w)), 200, w)
	}
}
