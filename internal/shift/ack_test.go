package shift

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/peer"
)

// upNative starts a role with socket delivery, idle and past its boot grace.
func upNative(t *testing.T, f fix, name string) {
	t.Helper()

	require.NoError(t, f.s.Up(name))
	f.status(name, "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
}

func TestAckSignalsCarryIDs(t *testing.T) {
	f, p := nativeFixture(t, watchYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	_, err = f.s.Note("ci-red", "pipeline red", false) // a watch: its own ack
	require.NoError(t, err)

	line := f.s.deliver(*f.now)
	assert.Equal(t, "[zmina] S-1 dev: T-5 result ready · ci-red: pipeline red (zmina S-1)", line)
	assert.Equal(t, []string{line}, p.posted["sid-dispatcher"])

	pend := f.s.Pending("dispatcher")
	require.Len(t, pend, 1)
	assert.Equal(t, "dev: T-5 result ready", pend[0].Text)
	assert.Equal(t, *f.now, pend[0].First)
}

func TestAckRenotifiesUntilAcked(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	since := f.s.clock(*f.now)

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliver(*f.now))
	p.showAll() // it reached the conversation, then a compaction lost it
	f.s.deliverInbox("dispatcher", *f.now)

	*f.now = f.now.Add(29 * time.Minute)
	f.s.renotify(*f.now)
	assert.Empty(t, f.s.State.Signals, "inside renotify")

	*f.now = f.now.Add(time.Minute)
	f.s.renotify(*f.now)
	assert.Equal(t, []string{"S-1 (still pending since " + since + ") dev: T-5 result ready"}, f.s.State.Signals)

	f.s.renotify(*f.now)
	assert.Len(t, f.s.State.Signals, 1, "queued already: not twice")

	line := f.s.deliver(*f.now)
	assert.Equal(t, "[zmina] S-1 (still pending since "+since+") dev: T-5 result ready (zmina S-1)", line)
	assert.Len(t, f.s.State.Acks, 1, "the same id, no new one")
	p.showAll()
	f.s.deliverInbox("dispatcher", *f.now)

	for range 5 {
		*f.now = f.now.Add(30 * time.Minute)
		f.s.renotify(*f.now)
		f.s.deliver(*f.now)
		p.showAll()
		f.s.deliverInbox("dispatcher", *f.now)
	}

	assert.Equal(t, renotesMax, f.s.State.Acks["S-1"].Renotes, "said again at most renotesMax times")
	assert.Len(t, f.s.Pending("dispatcher"), 1, "still listed")

	acked, warn := f.s.AckLines([]string{"s-1"}, false, "dispatcher", "dispatcher")
	assert.Empty(t, warn)
	assert.Equal(t, []string{"S-1"}, acked)
	assert.Empty(t, f.s.Pending(""))
	assert.True(t, f.ev.has("dispatcher", "acked"))
}

func TestAckLinesSkipsWhatItCannotAck(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	upNative(t, f, "dispatcher")

	id, err := f.s.QueueTracked("designer", "[dispatcher] look at T-1")
	require.NoError(t, err)
	assert.Equal(t, "S-1", id)
	_, err = f.s.QueueTracked("dispatcher", "[designer] and T-2")
	require.NoError(t, err)

	acked, warn := f.s.AckLines([]string{"S-1", "S-9", "S-2"}, false, "designer", "designer")
	assert.Equal(t, []string{"S-1"}, acked, "an unknown or another role's id does not stop the rest")
	require.Len(t, warn, 2)
	assert.Contains(t, warn[0], "S-9 waits for no ack")
	assert.Contains(t, warn[1], "S-2 is dispatcher's line")

	acked, warn = f.s.AckLines([]string{"S-1"}, false, "designer", "designer")
	assert.Empty(t, acked, "acked twice: a warning, nothing else")
	assert.Len(t, warn, 1)

	acked, warn = f.s.AckLines([]string{"S-2"}, false, "hand", "hand")
	assert.Equal(t, []string{"S-2"}, acked, "the owner acks any role's line")
	assert.Empty(t, warn)

	_, err = f.s.QueueTracked("designer", "[dispatcher] T-3")
	require.NoError(t, err)
	_, err = f.s.QueueTracked("designer", "[dispatcher] T-4")
	require.NoError(t, err)

	acked, warn = f.s.AckLines(nil, true, "designer", "dispatcher")
	assert.Empty(t, acked, "--all of another role: only the owner")
	assert.Len(t, warn, 2)

	acked, _ = f.s.AckLines(nil, true, "designer", "designer")
	assert.Equal(t, []string{"S-3", "S-4"}, acked)
}

func TestAckBriefNamesOnlyTheCurrentTag(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	first := f.s.State.Role("designer").Tag

	// The next start: a new tag in its argv, while the saved state may still hold the old one when its startup
	// hook runs.
	f.s.State.Role("designer").Tag = ""
	r, err := f.s.Role("designer")
	require.NoError(t, err)
	argv := strings.Join(f.s.Command(r, ""), " ")
	now := f.s.State.Role("designer").Tag
	require.NotEqual(t, first, now)
	assert.Contains(t, argv, tagMark(now))
	assert.NotContains(t, argv, first)

	f.s.State.Role("designer").Tag = first // the state the startup hook reads
	assert.NotContains(t, f.s.PendingBrief("designer", false), "[zmina:", "at a start: the boot line names the tag")

	f.s.State.Role("designer").Tag = now
	brief := f.s.PendingBrief("designer", true)
	assert.Contains(t, brief, tagMark(now), "after a trim: the tag of this start")
	assert.NotContains(t, brief, first)
}

func TestAckBriefIsCappedAndTellsTheTag(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	for i := range briefMax + 5 {
		_, err := f.s.QueueTracked("designer", fmt.Sprintf("[dispatcher] line %d", i))
		require.NoError(t, err)
		f.s.deliverInbox("designer", *f.now)
		p.showAll()
		f.s.deliverInbox("designer", *f.now)
	}

	brief := f.s.PendingBrief("designer", true)
	assert.Contains(t, brief, "`"+tagMark(f.s.State.Role("designer").Tag)+"`", "a trim may have lost the boot line")
	assert.Equal(t, briefMax, strings.Count(brief, "\n- S-"), "the newest briefMax")
	assert.Contains(t, brief, "S-25 (since")
	assert.NotContains(t, brief, "S-5 (since")
	assert.Contains(t, brief, "and 5 older ones: zmina pending")

	assert.Empty(t, f.s.PendingBrief("dispatcher", true), "not up: nothing to tell")
}

func TestAckQueuedLineToARole(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	since := f.s.clock(*f.now)

	_, err := f.s.QueueTracked("designer", "[dispatcher] look at T-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"[dispatcher] look at T-1 (zmina S-1)"},
		f.s.State.Role("designer").Inbox)

	f.s.renotify(f.now.Add(time.Hour))
	assert.Len(t, f.s.State.Role("designer").Inbox, 1, "not delivered yet: not said again")

	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	p.showAll()
	f.s.deliverInbox("designer", *f.now) // settles the posted line

	*f.now = f.now.Add(30 * time.Minute)
	f.s.renotify(*f.now)
	assert.Equal(t, []string{"[dispatcher] look at T-1 (zmina S-1, still pending since " + since +
		")"}, f.s.State.Role("designer").Inbox)

	assert.Contains(t, f.s.PendingBrief("designer", true), "S-1 (since "+since+") [dispatcher] look at T-1")
}

func TestAckOldRoleIsLeftAsItWas(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	f.s.State.Role("designer").Inbound = false // started by an earlier zmina

	id, err := f.s.QueueTracked("designer", "[dispatcher] plain")
	require.NoError(t, err)
	assert.Empty(t, id)
	assert.Equal(t, []string{"[dispatcher] plain"}, f.s.State.Role("designer").Inbox)
	assert.Empty(t, f.s.State.Acks)

	g := fixtureWith(t, crewYAML) // no inbox socket at all: signals go as before
	require.NoError(t, g.s.Up("dispatcher"))
	g.status("dispatcher", "idle", *g.now)
	*g.now = g.now.Add(3 * time.Minute)
	_, err = g.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	assert.Equal(t, "[zmina] dev: T-5 result ready", g.s.deliver(*g.now))
}

// A signal has its id from the moment it is noted: a failed post leaves it queued with the same id, never delivered.
func TestAckFailedDeliveryKeepsTheQueuedID(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	p.err = assert.AnError

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	assert.Empty(t, f.s.deliver(*f.now))
	require.Contains(t, f.s.State.Acks, "S-1")
	assert.True(t, f.s.State.Acks["S-1"].First.IsZero(), "never delivered")
	assert.Equal(t, []string{"S-1 dev: T-5 result ready"}, f.s.State.Signals)
	assert.Empty(t, f.s.PendingBrief("dispatcher", false), "a queued line is not one waiting for an ack after a compaction")

	p.err = nil
	assert.Equal(t, "[zmina] S-1 dev: T-5 result ready (zmina S-1)", f.s.deliver(*f.now))
}

func TestAckBrokenPostKeepsItsIDs(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	p.err = &peer.Committed{Err: fmt.Errorf("broken pipe")}

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	assert.NotEmpty(t, f.s.deliver(*f.now), "the session may have it")
	assert.Contains(t, f.s.State.Acks, "S-1", "the id stays with the line")
	assert.Empty(t, f.s.State.Signals)
	assert.Len(t, f.s.State.Role("dispatcher").Posted, 1, "followed until it shows")

	p.err = nil
	_, err = f.s.Note("dev", "T-6 result ready", false)
	require.NoError(t, err)
	assert.Empty(t, f.s.deliver(*f.now), "waits for the broken one")

	p.showAll()
	assert.Equal(t, "[zmina] S-2 dev: T-6 result ready (zmina S-2)", f.s.deliver(*f.now),
		"never the first signal again under a new id")
}

func TestAckHookInSettings(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)

	var plain map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &plain))
	assert.Equal(t, map[string]any{"crossSessionInbound": "accept"}, plain, "no executable known: no hook")

	f.s.Self = "/opt/zmina"
	raw := f.s.roleSettings("designer")

	var got struct {
		Inbound string `json:"crossSessionInbound"`
		Hooks   struct {
			SessionStart []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type, Command string
				} `json:"hooks"`
			}
		} `json:"hooks"`
	}

	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	assert.Equal(t, "accept", got.Inbound)
	require.Len(t, got.Hooks.SessionStart, 2)
	assert.Empty(t, got.Hooks.SessionStart[0].Matcher, "every start: new, resumed, compacted, cleared")
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' pending --role 'designer' --brief",
		got.Hooks.SessionStart[0].Hooks[0].Command)
	assert.Equal(t, "compact", got.Hooks.SessionStart[1].Matcher, "a compaction ends no turn: its start hook posts")
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' _turn-end --role 'designer' >/dev/null 2>&1 || true",
		got.Hooks.SessionStart[1].Hooks[0].Command)
}

// The zmina a role's turn-end hook runs is read from its command line, quoted as zmina writes it.
func TestHooksInCommandLine(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/it's zmina"
	args := "claude --settings " + f.s.roleSettings("designer") + " --disallowedTools=Agent --name x"
	assert.Equal(t, "/opt/it's zmina", hooksIn(args))

	f.s.Self = ""
	assert.Equal(t, hooksNone, hooksIn("claude --settings "+f.s.roleSettings("designer")+" --name x"))
	assert.Equal(t, hooksNone, hooksIn("claude --settings {\"hooks\":{\"SessionStart\":[]}} --name x"))
	assert.Equal(t, hooksNone, hooksIn("claude --name x"))
}

// A role an earlier zmina started is asked once: its command line tells the hooks it runs.
func TestHooksOfAnEarlierStartAreAsked(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	f.s.Self = "/opt/zmina-now"
	f.s.State.Role("dispatcher").Hooks = "" // an earlier zmina did not record them

	asked := 0
	f.s.ProcArgs = func(int) (string, error) {
		asked++

		return `claude --settings {"crossSessionInbound":"accept","hooks":{"SessionStart":[{"hooks":[{"command":"'/gone/zmina' --crew 'c' pending --role 'dispatcher' --brief","type":"command"}]}]}} --name d`, nil
	}

	r, err := f.s.Role("dispatcher")
	require.NoError(t, err)
	note := f.s.hooksNote("dispatcher", r)
	assert.Contains(t, note, "dispatcher: runs without zmina's turn-end hook")
	assert.Contains(t, note, "zmina restart dispatcher --continue (from its own session too",
		"the safe restart, on its conversation, also when the role runs it itself")
	assert.Empty(t, f.s.hooksNote("dispatcher", r), "once per session")
	assert.Equal(t, 1, asked)
	assert.Equal(t, hooksNone, f.s.State.Role("dispatcher").Hooks, "kept for the session")
}

func TestAckInRecoveryPackAndBootRule(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliver(*f.now))

	_, p, err := f.s.RecoveryPack()
	require.NoError(t, err)
	require.Len(t, p.Entries["unacked"], 1)
	assert.Equal(t, "zmina ack S-1", p.Entries["unacked"][0].Ref)

	r, err := f.s.Role("dispatcher")
	require.NoError(t, err)
	assert.Contains(t, f.s.BootText(r), "run `zmina ack S-12 S-14`")
	assert.Contains(t, f.s.BootText(r), "said again after 30m")
}

// A role with read_guard starts with the PreToolUse hook that runs zmina _read-guard, and is told of it at boot.
func TestReadGuardHookInSettings(t *testing.T) {
	f, _ := nativeFixture(t, strings.Replace(crewYAML, "  designer:\n", "  designer:\n    read_guard: true\n", 1))
	f.s.Self = "/opt/zmina"

	var got struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type, Command string
					Timeout       int
				} `json:"hooks"`
			}
		} `json:"hooks"`
	}

	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &got))
	require.Len(t, got.Hooks.PreToolUse, 1)
	assert.Equal(t, "Bash|Read", got.Hooks.PreToolUse[0].Matcher)
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' _read-guard --role 'designer'", got.Hooks.PreToolUse[0].Hooks[0].Command)
	assert.Equal(t, readGuardTimeout, got.Hooks.PreToolUse[0].Hooks[0].Timeout)

	got.Hooks.PreToolUse = nil
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer#2")), &got))
	assert.Len(t, got.Hooks.PreToolUse, 1, "a copy has its role's guard")

	got.Hooks.PreToolUse = nil
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("harvester")), &got))
	assert.Empty(t, got.Hooks.PreToolUse, "off unless asked")

	boot := f.s.BootText(f.s.Crew.Roles["designer"])
	assert.Contains(t, boot, ReadRule+" "+ReadGuardRule)
	assert.NotContains(t, f.s.BootText(f.s.Crew.Roles["harvester"]), ReadRule)
}

// skills: a role starts with its skill list trimmed — the overrides and description cap in its settings, the
// listing's budget in its environment — whether it takes lines natively or not.
func TestSkillsAtStart(t *testing.T) {
	body := strings.Replace(crewYAML, "roles:\n", "skills:\n  listing: 9k\n  off: [kreida]\nroles:\n", 1)
	body = strings.Replace(body, "  designer:\n", "  designer:\n    skills:\n      desc_max: 250\n", 1)
	f, _ := nativeFixture(t, body)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &got))
	assert.Equal(t, "accept", got["crossSessionInbound"])
	assert.Equal(t, map[string]any{"kreida": "off"}, got["skillOverrides"])
	assert.InDelta(t, 250, got["skillListingMaxDescChars"], 0)
	assert.Contains(t, f.s.Env(f.s.Crew.Roles["designer"]), "SLASH_COMMAND_TOOL_CHAR_BUDGET=9000")

	plain := fixtureWith(t, body) // no channel: lines are typed, no inbound settings
	argv := strings.Join(plain.s.Command(plain.s.Crew.Roles["harvester"], ""), " ")
	assert.Contains(t, argv, `--settings {"skillOverrides":{"kreida":"off"}}`)
	assert.NotContains(t, argv, "crossSessionInbound")

	none := fixtureWith(t, crewYAML)
	assert.NotContains(t, strings.Join(none.s.Command(none.s.Crew.Roles["harvester"], ""), " "), "--settings")
	assert.NotContains(t, strings.Join(none.s.Env(none.s.Crew.Roles["harvester"]), " "), "SLASH_COMMAND")
}
