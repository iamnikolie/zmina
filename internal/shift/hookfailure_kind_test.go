package shift

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/crew"
)

// heldFixture is a role with two lines queued, whose turn ended on the error the input says a minute after its boot turn,
// with the session idle since. It returns the moment the turn failed.
func heldFixture(t *testing.T, input string) (fix, *fakePeer, time.Time) {
	t.Helper()

	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second) // the boot turn is over
	f.s.State.Role("designer").Booted = start
	require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] second"))

	failedAt := start.Add(time.Minute)
	*f.now = failedAt
	f.status("designer", "idle", failedAt)
	f.live("designer")
	f.failed(t, "designer", input, failedAt)

	return f, p, failedAt
}

// heldAt says whether the role's queue is held at the moment: Waiting leaves it out, and no path delivers to it.
func heldAt(t *testing.T, f fix, p *fakePeer, at time.Time) bool {
	t.Helper()

	*f.now = at

	held := !slicesContain(f.s.Waiting(), "designer")

	if held {
		assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "%v in: a turn's end", at)
		assert.Empty(t, f.s.DeliverTo("designer"), "%v in: the beat's delivery", at)
		assert.Empty(t, p.posted["sid-designer"])
		assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, f.s.State.Role("designer").Inbox)
	}

	return held
}

// How long the queue waits is the kind of the error. A transient one waits FailHold, as ever; an account error AccountHold,
// and no longer: a slow probe, after which one line goes; a limit until its reset time, never less than FailHold and at most
// LimitHoldMax, FailHold when it gave none or none that can be believed.
func TestHowLongAFailedTurnHoldsTheQueueIsItsKind(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 1, 0, 0, time.UTC) // the moment the turn fails in heldFixture

	for _, tc := range []struct {
		name  string
		input string
		// held are offsets from the failure at which the queue is held, free those at which it goes.
		held, free []time.Duration
	}{
		{name: "transient", input: transientInput,
			held: []time.Duration{time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second, time.Hour}},
		{name: "unknown words are transient", input: `{"session_id":"sid-designer","error":"unknown"}`,
			held: []time.Duration{time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second}},
		{name: "account, by code and words", input: failedInput,
			held: []time.Duration{time.Second, FailHold + time.Second, AccountHold - time.Second},
			free: []time.Duration{AccountHold + time.Second, 2 * time.Hour, 72 * time.Hour}},
		{name: "account, by words alone", input: `{"session_id":"sid-designer","error":"unknown","last_assistant_message":"Credit balance is too low"}`,
			held: []time.Duration{FailHold + time.Second, AccountHold - time.Second},
			free: []time.Duration{AccountHold + time.Second}},
		{name: "account, by the hook's error_details", input: `{"session_id":"sid-designer","error":"unknown","error_details":"OAuth token has expired"}`,
			held: []time.Duration{FailHold + time.Second, AccountHold - time.Second},
			free: []time.Duration{AccountHold + time.Second}},
		{name: "a limit until it lifts", input: limitInput(at.Add(2 * time.Hour)),
			held: []time.Duration{time.Second, FailHold + time.Second, time.Hour, 2*time.Hour - time.Second},
			free: []time.Duration{2*time.Hour + time.Second, 3 * time.Hour}},
		{name: "a limit that lifts in a minute is held FailHold", input: limitInput(at.Add(time.Minute)),
			held: []time.Duration{time.Second, time.Minute + time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second}},
		{name: "a limit that lifts far off waits at most LimitHoldMax", input: limitInput(at.Add(10 * time.Hour)),
			held: []time.Duration{FailHold + time.Second, LimitHoldMax - time.Second},
			free: []time.Duration{LimitHoldMax + time.Second, 9 * time.Hour}},
		{name: "a limit with no time waits FailHold", input: limitNoTimeInput,
			held: []time.Duration{time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second}},
		{name: "a limit whose clock time had passed when it was recorded waits FailHold", input: limitInput(at.Add(-3 * time.Hour)),
			held: []time.Duration{time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second, time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, failedAt := heldFixture(t, tc.input)
			require.Equal(t, at, failedAt)

			for _, d := range tc.held {
				assert.True(t, heldAt(t, f, p, failedAt.Add(d)), "%v after the failure: held", d)
			}

			for _, d := range tc.free {
				assert.False(t, heldAt(t, f, p, failedAt.Add(d)), "%v after the failure: free", d)
			}
		})
	}
}

// The reset time is read once, when the record is written, and the record keeps it with the kind; a record written before
// there was either has neither and is held as an error of unknown time would be, whatever zone its reader is in.
func TestTheHookRecordKeepsTheKindAndTheResetItReadOnce(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	got, err := RecordFailure(l, "designer", []byte(limitInput(at.Add(3*time.Hour))), at)
	require.NoError(t, err)

	assert.Equal(t, "limit", got.Kind)
	assert.True(t, at.Add(3*time.Hour).Equal(got.Reset), "%v", got.Reset)

	var read TurnFailure
	require.True(t, readRecord(l.HookFailure("designer"), &read))
	assert.Equal(t, "limit", read.Kind)
	assert.True(t, got.Reset.Equal(read.Reset))
	assert.Equal(t, FailureClass{Kind: KindLimit, Reset: read.Reset}, read.Class(), "the reader takes both from the record")

	raw, err := os.ReadFile(l.HookFailure("designer"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"kind":"limit"`)
	assert.Contains(t, string(raw), `"reset":"2026-10-24T15:00:00Z"`, "an RFC 3339 instant")

	// "resets 2am" recorded at 23:00 is a real reset three hours ahead, across midnight: the record keeps it and the hold lasts
	// until then; "resets 11:59am" recorded at noon would be a day off, and is no time (FailHold).
	late := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)

	across, err := RecordFailure(l, "designer", []byte(`{"session_id":"s","error":"rate_limit","last_assistant_message":"5-hour limit reached ∙ resets 2am (UTC)"}`), late)
	require.NoError(t, err)
	assert.True(t, late.Add(3*time.Hour).Equal(across.Reset), "%v", across.Reset)
	assert.Equal(t, 3*time.Hour, across.holdFor())
	assert.True(t, across.holds(late.Add(3*time.Hour-time.Second)))
	assert.False(t, across.holds(late.Add(3*time.Hour+time.Second)))

	noon := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	dayOff, err := RecordFailure(l, "designer", []byte(`{"session_id":"s","error":"rate_limit","last_assistant_message":"5-hour limit reached ∙ resets 11:59am (UTC)"}`), noon)
	require.NoError(t, err)
	assert.True(t, dayOff.Reset.IsZero(), "%v", dayOff.Reset)
	assert.Equal(t, FailHold, dayOff.holdFor())

	// An old record: the same words, no kind and no reset. Its kind is read from its words, its reset is not read at all.
	old := TurnFailure{SessionID: "s", At: at, Error: "rate_limit", Details: "You've hit your limit · resets 3pm (UTC)"}
	assert.Equal(t, FailureClass{Kind: KindLimit}, old.Class())
	assert.Equal(t, FailHold, old.holdFor())

	oldAccount := TurnFailure{SessionID: "s", At: at, Error: "model_not_found"}
	assert.Equal(t, FailureClass{Kind: KindAccount}, oldAccount.Class())
	assert.Equal(t, AccountHold, oldAccount.holdFor())
}

// An account error is held a slow AccountHold and no longer, and what lifts it sooner is the role's next turn ending or a
// restart. Past the hold one line goes; a turn that fails the same way is a record of its own and holds another AccountHold.
func TestAnAccountFailureHoldIsAProbeAndAnyNextTurnLiftsItSooner(t *testing.T) {
	f, p, failedAt := heldFixture(t, failedInput)

	require.True(t, heldAt(t, f, p, failedAt.Add(AccountHold-time.Second)))
	require.False(t, heldAt(t, f, p, failedAt.Add(AccountHold+time.Second)), "the probe: one line goes")

	// It fails the same way: a new record, and another AccountHold.
	again := failedAt.Add(AccountHold + time.Minute)
	f.status("designer", "idle", again)
	f.live("designer")
	f.failed(t, "designer", failedInput, again)

	assert.True(t, heldAt(t, f, p, again.Add(AccountHold-time.Second)))
	assert.False(t, heldAt(t, f, p, again.Add(AccountHold+time.Second)))

	// The role goes through a turn since, say the owner fixed the login and typed to it: the record is no longer how the
	// session stands, and the queue is free long before the hold would have run out.
	g, q, failedAt := heldFixture(t, failedInput)
	require.True(t, heldAt(t, g, q, failedAt.Add(10*time.Minute)))
	g.status("designer", "idle", failedAt.Add(11*time.Minute))
	g.live("designer")
	assert.False(t, heldAt(t, g, q, failedAt.Add(11*time.Minute+time.Second)))

	// A start takes the record away.
	h, r, failedAt := heldFixture(t, failedInput)
	require.True(t, heldAt(t, h, r, failedAt.Add(10*time.Minute)))
	h.s.dropHookRecords("designer")
	assert.False(t, heldAt(t, h, r, failedAt.Add(10*time.Minute+time.Second)))
}

// ownerHeld is a role held for a failed turn whose queue is the lines given, the owner's own among them (by the receipt Chat
// makes for it): the lines are queued first, then the turn fails.
func ownerHeld(t *testing.T, input string, lines []string, owner ...string) (fix, *fakePeer, time.Time) {
	t.Helper()

	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second)
	f.s.State.Role("designer").Booted = start

	for _, line := range lines {
		require.NoError(t, f.s.Queue("designer", line))
	}

	rs := f.s.State.Role("designer")
	for i, line := range owner {
		rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: fmt.Sprintf("c%d", i), Line: line, At: start})
	}

	failedAt := start.Add(time.Minute)
	*f.now = failedAt.Add(time.Second)
	f.status("designer", "idle", failedAt)
	f.live("designer")
	f.failed(t, "designer", input, failedAt)

	return f, p, failedAt
}

// The owner's own message goes through a hold wherever it stands in the queue: it is taken to the head, out of its order,
// only while the role is held. The lines before it stay held, and in their order.
func TestTheOwnersMessageGoesThroughAHoldFromAnyPlaceInTheQueue(t *testing.T) {
	const owner = "try that again, please"

	for name, input := range map[string]string{"a transient hold": transientInput, "an account hold": failedInput,
		"a limit hold": limitNoTimeInput} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := ownerHeld(t, input, []string{"[dispatcher] first", "[dispatcher] second", owner, "[dispatcher] third"}, owner)

			require.True(t, f.s.failureHeld("designer", *f.now))
			assert.Equal(t, 2, f.s.ownerLine("designer"))
			assert.True(t, slicesContain(f.s.Waiting(), "designer"), "the owner's line is something a pass could give")

			assert.NotEmpty(t, f.s.DeliverTo("designer"), "it goes")
			assert.Equal(t, []string{owner}, f.tm.typed["designer"], "typed, as the owner's own words are")
			assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second", "[dispatcher] third"}, f.s.State.Role("designer").Inbox,
				"what zmina queued stays, in its order")
		})
	}
}

// The first of the owner's lines in the queue goes first, the other after it, and the lines before them stay held; with no
// owner's line in the queue a hold lets nothing go and the queue is not touched; and once the hold is over the queue goes in
// its own order, the owner's line behind the others not taken ahead.
func TestAHoldTakesNoLineButTheOwnersAndNeverReordersWithoutOne(t *testing.T) {
	t.Run("two of the owner's lines", func(t *testing.T) {
		f, _, _ := ownerHeld(t, failedInput, []string{"[dispatcher] first", "second owner line", "[dispatcher] third", "first owner line"},
			"second owner line", "first owner line")

		assert.Equal(t, 1, f.s.ownerLine("designer"), "the first of them in the queue")

		f.s.DeliverTo("designer")
		assert.Equal(t, []string{"second owner line"}, f.tm.typed["designer"])
		assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] third", "first owner line"}, f.s.State.Role("designer").Inbox)
		assert.Equal(t, 2, f.s.ownerLine("designer"), "the other is the next to go")
	})

	t.Run("none of the owner's", func(t *testing.T) {
		g, q, _ := heldFixture(t, failedInput)
		before := slicesClone(g.s.State.Role("designer").Inbox)

		assert.False(t, slicesContain(g.s.Waiting(), "designer"))
		assert.Empty(t, g.s.DeliverTo("designer"))
		assert.Equal(t, before, g.s.State.Role("designer").Inbox, "nothing moves, nothing goes")
		assert.Empty(t, q.posted["sid-designer"])
	})

	t.Run("no hold", func(t *testing.T) {
		h, r, failedAt := ownerHeld(t, transientInput, []string{"[dispatcher] first", "an owner line"}, "an owner line")
		*h.now = failedAt.Add(FailHold + time.Minute)
		require.False(t, h.s.failureHeld("designer", *h.now))
		require.Equal(t, 1, h.s.ownerLine("designer"))

		assert.NotEmpty(t, h.s.TurnEnd("designer", "sid-designer"))
		assert.Equal(t, []string{"[dispatcher] first"}, r.posted["sid-designer"], "the first line first")
		assert.Equal(t, []string{"an owner line"}, h.s.State.Role("designer").Inbox)
	})
}

// A full queue behind a hold: the owner's line at its very end is found and goes; and the owner's new message is not refused
// for the lines that fill the queue, which nothing will read before the hold is over, though only a few past the bound.
func TestTheOwnersMessageGetsThroughAFullQueueBehindAHold(t *testing.T) {
	const owner = "are you there?"

	lines := make([]string, 0, inboxMax)
	for i := range inboxMax - 1 {
		lines = append(lines, fmt.Sprintf("[dispatcher] line %d", i))
	}

	lines = append(lines, owner)
	require.Len(t, lines, inboxMax)

	f, _, _ := ownerHeld(t, failedInput, lines, owner)

	require.Len(t, f.s.State.Role("designer").Inbox, inboxMax)
	assert.Equal(t, inboxMax-1, f.s.ownerLine("designer"), "the last place of a full queue")
	assert.NotEmpty(t, f.s.DeliverTo("designer"))
	assert.Equal(t, []string{owner}, f.tm.typed["designer"])
	assert.Len(t, f.s.State.Role("designer").Inbox, inboxMax-1)

	// Full of what nothing will read, and held: the owner writes, and it is taken, past the bound, and goes first.
	g, _, _ := heldFixture(t, failedInput)
	for i := len(g.s.State.Role("designer").Inbox); i < inboxMax; i++ {
		require.NoError(t, g.s.Queue("designer", fmt.Sprintf("[dispatcher] filler %d", i)))
	}

	require.Len(t, g.s.State.Role("designer").Inbox, inboxMax)
	require.Error(t, g.s.Queue("designer", "[dispatcher] too many"), "any other sender is refused")

	require.NoError(t, g.s.QueueOwner("designer", "owner again"))
	assert.Len(t, g.s.State.Role("designer").Inbox, inboxMax+1)

	for range ownerRoom - 1 {
		require.NoError(t, g.s.QueueOwner("designer", "and again"))
	}

	require.Error(t, g.s.QueueOwner("designer", "no more room"), "only ownerRoom past the bound")

	// Not held: the bound is the bound for the owner as for anyone.
	h, _ := nativeFixture(t, crewYAML)
	require.NoError(t, h.s.Up("designer"))

	for i := range inboxMax {
		require.NoError(t, h.s.Queue("designer", fmt.Sprintf("[dispatcher] line %d", i)))
	}

	require.ErrorContains(t, h.s.QueueOwner("designer", "the owner"), "lines already wait")
}

// A held dispatcher is given the owner's own line from any place in its queue too, and nothing else of what waits for it.
func TestAHeldDispatcherIsGivenTheOwnersLineWhereverItStands(t *testing.T) {
	const owner = "what is going on?"

	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	start := *f.now
	f.status("dispatcher", "idle", start)
	f.live("dispatcher")
	f.rg.endedAt["sid-dispatcher"] = start.Add(time.Second)
	f.s.State.Role("dispatcher").Booted = start

	_, err := f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)
	require.NoError(t, f.s.Queue("dispatcher", "[zmina] first of its own"))
	require.NoError(t, f.s.Queue("dispatcher", owner))

	rs := f.s.State.Role("dispatcher")
	rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "c1", Line: owner, At: start})

	failedAt := start.Add(time.Minute)
	*f.now = failedAt.Add(time.Minute)
	f.status("dispatcher", "idle", failedAt)
	f.live("dispatcher")
	f.failed(t, "dispatcher", strings.Replace(failedInput, "sid-designer", "sid-dispatcher", 1), failedAt)

	require.True(t, f.s.failureHeld("dispatcher", *f.now))
	assert.Contains(t, f.s.Waiting(), "dispatcher", "the owner's line is something it can be given")

	assert.NotEmpty(t, f.s.DeliverTo("dispatcher"))
	assert.Equal(t, []string{owner}, f.tm.typed["dispatcher"])
	assert.Equal(t, []string{"[zmina] first of its own"}, f.s.State.Role("dispatcher").Inbox)
	assert.Len(t, f.s.State.Signals, 1, "the signals still wait")
	assert.Empty(t, p.posted["sid-dispatcher"])
}

// The held dispatcher is the one that would hear of its own failure, and it cannot: the owner is told in the log (an event
// of its own), in the view zmina ls prints, and by zmina doctor, and not only by a note to the queue the dispatcher does not
// read. A role that is not the dispatcher, and a transient error, make no such event.
func TestAHeldDispatcherIsSurfacedToTheOwnerNotOnlyByANote(t *testing.T) {
	dispatcherInput := strings.Replace(failedInput, "sid-designer", "sid-dispatcher", 1)

	// The crew keeps its own clock: the times the owner reads are in it, not in UTC.
	f := fixtureWith(t, crewYAML+"timezone: Asia/Kolkata\n")
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	f.live("dispatcher")
	f.failed(t, "dispatcher", dispatcherInput, *f.now)
	*f.now = f.now.Add(time.Minute)

	// The queue the note would go to is full: nothing more can be told through it.
	for i := range signalsMax {
		_, err := f.s.Note("mrs", fmt.Sprintf("acme/site!%d approved", i), false)
		require.NoError(t, err)
	}

	_, err := f.s.Note("mrs", "one more", false)
	require.ErrorContains(t, err, "it is not reading them")

	note := f.s.apiErrorNote("dispatcher", f.s.State.Role("dispatcher"))
	assert.Contains(t, note, "that needs the owner")
	assert.Contains(t, note, "its queued lines wait 30m, or less when its next turn ends or it is restarted")

	var held string

	for _, e := range f.ev.list {
		if e.Event == "dispatcher-held" {
			held = e.Detail
		}
	}

	require.NotEmpty(t, held, "an event in the log")
	assert.Contains(t, held, "account problem, needs the owner")
	assert.Contains(t, held, "the crew's signals and the health notes wait for it until 18:00 IST")
	assert.NotContains(t, held, "UTC")
	assert.Contains(t, held, "zmina ls, zmina doctor")

	// The view, in the crew's location too: the same instant.
	v := mustViewOf(t, f.s, "dispatcher")
	assert.Equal(t, "account", v.TurnErrorKind)
	assert.True(t, f.now.Add(-time.Minute).Add(AccountHold).Equal(v.TurnErrorHeldUntil), "%v", v.TurnErrorHeldUntil)
	assert.Equal(t, "18:00 IST", v.TurnErrorHeldUntil.Format("15:04 MST"))

	// Not the dispatcher, or not an account error: no such event.
	g := fixtureWith(t, crewYAML)
	require.NoError(t, g.s.Up("designer"))
	g.status("designer", "idle", *g.now)
	g.live("designer")
	g.failed(t, "designer", failedInput, *g.now)
	g.s.apiErrorNote("designer", g.s.State.Role("designer"))
	assert.False(t, g.ev.has("designer", "dispatcher-held"))

	h := fixtureWith(t, crewYAML)
	require.NoError(t, h.s.Up("dispatcher"))
	h.status("dispatcher", "idle", *h.now)
	h.live("dispatcher")
	h.failed(t, "dispatcher", strings.Replace(transientInput, "sid-designer", "sid-dispatcher", 1), *h.now)
	h.s.apiErrorNote("dispatcher", h.s.State.Role("dispatcher"))
	assert.False(t, h.ev.has("dispatcher", "dispatcher-held"))
}

// The kind is in the role's view only when it is not transient, so that a transient error is what it was in --json and in
// zmina ls; a limit carries the time it lifts, and either the time the hold ends.
func TestViewSaysTheKindOfAFailedTurn(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name, input, kind string
		reset, until      time.Time
	}{
		{name: "transient", input: transientInput},
		{name: "account", input: failedInput, kind: "account", until: at.Add(AccountHold)},
		{name: "a limit with its time", input: limitInput(at.Add(3 * time.Hour)), kind: "limit", reset: at.Add(3 * time.Hour),
			until: at.Add(3 * time.Hour)},
		{name: "a limit with none", input: limitNoTimeInput, kind: "limit", until: at.Add(FailHold)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.status("designer", "idle", *f.now)
			f.failed(t, "designer", tc.input, *f.now)

			v := mustViewOf(t, f.s, "designer")

			assert.NotEmpty(t, v.TurnError)
			assert.Equal(t, tc.kind, v.TurnErrorKind)
			assert.True(t, tc.reset.Equal(v.TurnErrorReset), "%v", v.TurnErrorReset)
			assert.True(t, tc.until.Equal(v.TurnErrorHeldUntil), "%v", v.TurnErrorHeldUntil)

			raw, err := json.Marshal(v)
			require.NoError(t, err)

			if tc.kind == "" {
				assert.NotContains(t, string(raw), "turn_error_kind")
				assert.NotContains(t, string(raw), "turn_error_reset")
				assert.NotContains(t, string(raw), "turn_error_held_until")
			} else {
				assert.Contains(t, string(raw), `"turn_error_kind":"`+tc.kind+`"`)
				assert.Contains(t, string(raw), `"turn_error_held_until"`)
			}
		})
	}
}

// The dispatcher's line is the plain one for a transient error; a limit says so, with when it lifts; an account error says
// the owner has to mend it and how long the role waits, but only when the role is held for it: the note and the hold go by
// one classification (the hook record's), and a role with no hook record has no hold to claim. Told once, from the hook or the
// transcript.
func TestAPIErrorNoteSaysTheKind(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	const accountLine = "designer: its last turn ended in an API error that needs the owner (an account, billing, login or " +
		"permission problem, which retrying does not mend): "

	for _, tc := range []struct {
		name, input, transcript string
		// late is how long after the failure the note is told; unlive says the role's session is not found (no hold).
		late   time.Duration
		unlive bool
		want   string
	}{
		{name: "transient", input: transientInput, want: "designer: its last turn ended in an API error: " + transientText},
		{name: "account, held", input: failedInput,
			want: accountLine + failedText + "; its queued lines wait 30m, or less when its next turn ends or it is restarted"},
		{name: "a limit with its time", input: limitInput(at.Add(3 * time.Hour)),
			want: "designer: its last turn ended in an API error (usage limit, resets Sat 15:00 UTC): rate_limit: You've hit your limit · resets 3:00pm (UTC)"},
		{name: "a limit with none", input: limitNoTimeInput,
			want: "designer: its last turn ended in an API error (usage limit): rate_limit: API Error: 429 Too Many Requests"},
		{name: "the hook's kind, the transcript's words", input: failedInput, transcript: "API Error: Connection lost mid-response.",
			want: accountLine + "API Error: Connection lost mid-response.; its queued lines wait 30m, or less when its next turn ends or it is restarted"},
		{name: "account, but the hold has run out when it is told", input: failedInput, late: AccountHold + time.Minute,
			want: accountLine + failedText},
		{name: "account, but the role's session is not found: no hold", input: failedInput, unlive: true,
			want: accountLine + failedText},
		{name: "the transcript's words, no hook", transcript: "You've hit your limit · resets 9pm (UTC)",
			want: "designer: its last turn ended in an API error (usage limit, resets Sat 21:00 UTC): You've hit your limit · resets 9pm (UTC)"},
		{name: "the transcript's words of an account problem, no hook: no hold claimed", transcript: "OAuth token has expired",
			want: accountLine + "OAuth token has expired"},
		{name: "the transcript's words of a transient one, no hook", transcript: "API Error: Connection lost mid-response.",
			want: "designer: its last turn ended in an API error: API Error: Connection lost mid-response."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.status("designer", "idle", *f.now)

			if !tc.unlive {
				f.live("designer")
			}

			if tc.input != "" {
				f.failed(t, "designer", tc.input, *f.now)
			}

			if tc.transcript != "" {
				f.rg.errors["sid-designer"] = tc.transcript
			}

			*f.now = f.now.Add(time.Minute + tc.late)
			rs := f.s.State.Role("designer")

			assert.Equal(t, tc.want, f.s.apiErrorNote("designer", rs))
			assert.Empty(t, f.s.apiErrorNote("designer", rs), "once")
		})
	}
}

// The words of the failure-hold event: what it always said for a transient error, and for the others how long.
func TestHoldWords(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	// The times are in the location the caller gives, the crew's.
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		f    TurnFailure
		want string
	}{
		{name: "transient", f: TurnFailure{At: at, Error: "server_error", Details: "API Error: 529 Overloaded"},
			want: "the lines queued for it wait 5m0s"},
		{name: "account", f: TurnFailure{At: at, Error: "billing_error"},
			want: "an account problem that needs the owner: the lines queued for it wait 30m, or less when its next turn ends or it is restarted"},
		{name: "a limit with its time", f: TurnFailure{At: at, Kind: "limit", Reset: at.Add(3 * time.Hour)},
			want: "a usage limit: the lines queued for it wait until Sat 20:30 IST (at least 5m, at most 6h)"},
		{name: "a limit that lifts in a minute", f: TurnFailure{At: at, Kind: "limit", Reset: at.Add(time.Minute)},
			want: "a usage limit: the lines queued for it wait until Sat 17:35 IST (at least 5m, at most 6h)"},
		{name: "a limit that lifts far off", f: TurnFailure{At: at, Kind: "limit", Reset: at.Add(20 * time.Hour)},
			want: "a usage limit: the lines queued for it wait until Sat 23:30 IST (at least 5m, at most 6h)"},
		{name: "a limit with none", f: TurnFailure{At: at, Error: "rate_limit"},
			want: "a usage limit with no reset time given: the lines queued for it wait 5m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HoldWords(&tc.f, kolkata))
		})
	}
}

// What the hook says is kept as it said it; error_details is kept apart and read only to sort the error, so the words a view
// and a note show are what they were.
func TestRecordFailureKeepsErrorDetailsApart(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	got, err := RecordFailure(l, "designer", []byte(`{"session_id":"s","error":"unknown","last_assistant_message":"API Error",`+
		`"error_details":"invalid x-api-key"}`), at)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, "unknown: API Error", got.Text())
	assert.Equal(t, "invalid x-api-key", got.ErrorDetails)
	assert.Equal(t, KindAccount, got.Class().Kind)
	assert.Equal(t, "account", got.Kind)

	var read TurnFailure
	require.True(t, readRecord(l.HookFailure("designer"), &read))
	assert.Equal(t, *got, read)

	// A record written before the fields existed reads, and is sorted by the words it has.
	old := TurnFailure{SessionID: "s", At: at, Error: "model_not_found"}
	assert.Equal(t, KindAccount, old.Class().Kind)
}

// The roles held for a failed turn, for zmina doctor, by the delivery's own test: a record of the role's live session that is
// still how it stands. Not the records alone: a role that has been through a turn since, whose session changed, that is not up,
// or whose hold ran out is not listed, so that doctor and ls agree (TestViewSaysTheKindOfAFailedTurn is ls).
func TestFailureHoldsListsOnlyTheHoldsStillInForce(t *testing.T) {
	f := fixtureWith(t, crewYAML+"timezone: Asia/Kolkata\n")
	at := *f.now

	for role, input := range map[string]string{
		"designer":   strings.Replace(failedInput, "sid-designer", "sid-designer", 1),
		"harvester":  strings.Replace(limitInput(at.Add(2*time.Hour)), "sid-designer", "sid-harvester", 1),
		"dispatcher": strings.Replace(transientInput, "sid-designer", "sid-dispatcher", 1),
	} {
		require.NoError(t, f.s.Up(role))
		f.status(role, "idle", at)
		f.live(role)
		f.failed(t, role, input, at)
	}

	holds := func(now time.Time) map[string]FailureHold {
		t.Helper()

		out := map[string]FailureHold{}
		for _, h := range f.s.FailureHolds(now) {
			out[h.Role] = h
		}

		return out
	}

	now := at.Add(time.Minute)
	got := holds(now)
	require.Len(t, got, 3, "%v", got)

	assert.Equal(t, KindAccount, got["designer"].Class.Kind)
	assert.True(t, at.Add(AccountHold).Equal(got["designer"].Until))
	assert.Equal(t, "18:00 IST", got["designer"].Until.Format("15:04 MST"), "in the crew's location")
	assert.Equal(t, KindLimit, got["harvester"].Class.Kind)
	assert.True(t, at.Add(2*time.Hour).Equal(got["harvester"].Until))
	assert.Equal(t, KindTransient, got["dispatcher"].Class.Kind)

	// Each of these takes a hold away.
	f.status("designer", "idle", at.Add(10*time.Minute)) // a turn went through since
	f.live("designer")
	f.rg.byPID[f.tm.up["harvester"]].SessionID = "sid-after-a-clear" // the session changed
	f.status("dispatcher", "busy", at.Add(time.Second))
	f.live("dispatcher")

	assert.Empty(t, holds(now), "no hold is in force any more")

	// The hold that ran out.
	f.status("designer", "idle", at)
	f.live("designer")
	require.Len(t, holds(now), 1)
	assert.Empty(t, holds(at.Add(AccountHold+time.Minute)))
	assert.Empty(t, holds(at.Add(LimitHoldMax+time.Minute)), "past the longest hold: none")
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// mustViewOf is the view of a role, which the test fails without: an assertion made inside a loop over the views, on the one that
// matches, passes for a view that is not there.
func mustViewOf(t *testing.T, s *Shift, role string) View {
	t.Helper()

	found := false

	var out View

	for _, v := range s.Views() {
		if v.Role == role {
			out, found = v, true
		}
	}

	require.True(t, found, "no view of %s", role)

	return out
}

// The owner's line behind others is taken to the head only at the moment it is typed. A line that was not typed leaves the
// queue in its order; the tries counted (Attempts, TriedAt) are the first line's, which is not the one that goes: they neither
// hold the owner's line back, nor are cleared, nor become the owner's. The pass that names the role (Waiting) and the delivery
// that follows agree.
func TestTheOwnersLineMovesOnlyWhenTypedAndNeverTakesAnotherLinesTries(t *testing.T) {
	const owner = "the owner's own words"

	lines := []string{"[dispatcher] first", "[dispatcher] second", owner, "[dispatcher] third"}
	inOrder := func(f fix) []string { return slicesClone(f.s.State.Role("designer").Inbox) }

	// held is the fixture: the role is held, and the first line of its queue failed a moment ago.
	held := func(t *testing.T) (fix, time.Time) {
		t.Helper()

		f, _, _ := ownerHeld(t, failedInput, lines, owner)
		rs := f.s.State.Role("designer")
		rs.Attempts, rs.TriedAt = 2, *f.now
		require.True(t, rs.triedLately(*f.now), "the first line failed a moment ago")
		require.True(t, f.s.failureHeld("designer", *f.now))

		return f, rs.TriedAt
	}

	t.Run("typed: the first line keeps its tries and its place", func(t *testing.T) {
		f, tried := held(t)

		assert.True(t, slicesContain(f.s.Waiting(), "designer"), "its tries do not hold the owner's line back: a pass names the role")
		assert.NotEmpty(t, f.s.DeliverTo("designer"), "and the delivery gives it")
		assert.Equal(t, []string{owner}, f.tm.typed["designer"])

		rs := f.s.State.Role("designer")
		assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second", "[dispatcher] third"}, rs.Inbox)
		assert.Equal(t, 2, rs.Attempts, "the tries are the first line's still")
		assert.True(t, tried.Equal(rs.TriedAt))
	})

	t.Run("a draft in the input line: not typed, nothing moved", func(t *testing.T) {
		f, tried := held(t)
		before := inOrder(f)
		f.tm.drafts["designer"] = "half-typed words of the owner's own"

		assert.Empty(t, f.s.DeliverTo("designer"))
		assert.Empty(t, f.tm.typed["designer"])
		assert.Equal(t, before, inOrder(f), "the order the queue had, not the owner's line at its head")

		rs := f.s.State.Role("designer")
		assert.Equal(t, 2, rs.Attempts)
		assert.True(t, tried.Equal(rs.TriedAt))
	})

	t.Run("typing fails: the line is back in its place and the tries are the first line's", func(t *testing.T) {
		f, tried := held(t)
		before := inOrder(f)
		f.tm.typeErr = errNotTyped("tmux would not take it")

		assert.Empty(t, f.s.DeliverTo("designer"))
		assert.Empty(t, f.tm.typed["designer"])
		assert.Equal(t, before, inOrder(f), "put back where it stood")

		rs := f.s.State.Role("designer")
		assert.Equal(t, 2, rs.Attempts, "the failed try of the owner's line is not counted on the first line")
		assert.True(t, tried.Equal(rs.TriedAt))
	})

	t.Run("the owner's line at the head, failed a moment ago itself: not named, not given", func(t *testing.T) {
		f, _, _ := ownerHeld(t, failedInput, []string{owner, "[dispatcher] first"}, owner)
		rs := f.s.State.Role("designer")
		rs.Attempts, rs.TriedAt = 1, *f.now

		assert.False(t, slicesContain(f.s.Waiting(), "designer"), "its own failed try gates it, as any line's")
		assert.Empty(t, f.s.DeliverTo("designer"))
		assert.Equal(t, []string{owner, "[dispatcher] first"}, rs.Inbox)
		assert.Equal(t, 1, rs.Attempts)
	})
}
