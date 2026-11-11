package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/record"
)

// saveResult saves a result of a task as a role reports it, each section "Recorded".
func saveResult(t *testing.T, f fix, role, id string) {
	t.Helper()

	r, err := f.s.Role(role)
	require.NoError(t, err)

	shape := f.s.Crew.HandoffFor(r)
	values := map[string]string{}

	for _, sec := range shape.Sections {
		values[sec.Name] = "Recorded"
	}

	_, err = record.Save(f.s.Crew.StateDir(), record.Document{Version: 1, Kind: "result", Task: id, Role: role, At: *f.now,
		Shape: shape, Values: values})
	require.NoError(t, err)
}

// closed are the info lines the event log says were closed, as "how: line".
func closed(f fix) []string {
	var out []string

	for _, e := range f.ev.list {
		if e.Event == "closed" {
			out = append(out, e.Detail)
		}
	}

	return out
}

// Each kind of info line goes as an FYI line with no ack id, is closed when the dispatcher's conversation shows it,
// and is never said again or listed in the brief.
func TestInfoLineClosesOnSeen(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T, f fix)
		want string
	}{
		{"a result ready", func(t *testing.T, f fix) {
			saveResult(t, f, "designer", "T-1")
			f.s.NotifyRecords()
		}, "[zmina] FYI designer: T-1 result ready (worker-reported): Recorded (zmina task result T-1 --revision "},
		{"a role released", func(t *testing.T, f fix) {
			require.NoError(t, f.s.Up("designer"))
			f.s.State.Role("designer").ReleaseIdle = 34 * time.Minute
			_, note := f.s.putDown("designer")
			f.s.addNotes([]string{note})
		}, "[zmina] FYI released designer after 34m idle"},
		{"a keep: always role started again", func(t *testing.T, f fix) {
			did := f.s.Tick()
			assert.Contains(t, did, "started harvester")
		}, "[zmina] FYI harvester: started again (keep: always)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, p := nativeFixture(t, crewYAML)
			upNative(t, f, "dispatcher")
			c.make(t, f)

			assert.Empty(t, f.s.deliver(*f.now), "alone, it waits for a line to go with")

			*f.now = f.now.Add(infoRide)
			line := f.s.deliver(*f.now)
			assert.True(t, strings.HasPrefix(line, c.want), line)
			assert.NotContains(t, line, "(zmina S-", "no ack tail")
			assert.NotContains(t, line, "S-1")
			assert.Empty(t, f.s.State.Acks, "no ack id")
			assert.Empty(t, f.s.Pending("dispatcher"))
			assert.Empty(t, f.s.PendingBrief("dispatcher", false), "not in the brief")
			assert.Empty(t, closed(f), "not closed before it is seen")

			p.showAll()
			assert.Empty(t, f.s.deliver(*f.now))
			require.Len(t, closed(f), 1)
			assert.Equal(t, "seen: "+strings.TrimPrefix(line, "[zmina] FYI "), closed(f)[0])
			assert.Empty(t, f.s.State.Role("dispatcher").Posted)

			// Nothing says it again, however long it goes unacked.
			for range 4 {
				*f.now = f.now.Add(f.s.Crew.RenotifyD())
				f.s.renotify(*f.now)
				assert.Empty(t, f.s.deliver(*f.now))
			}

			assert.Len(t, p.raw, 1, "posted once")
			assert.Empty(t, f.s.State.Signals)
			assert.Empty(t, f.s.State.Notes)
			assert.Empty(t, f.s.State.Info, "nothing is kept of it")
			assert.False(t, f.ev.has("dispatcher", "renoted"))
		})
	}
}

// A line to act on is what it was: its id, its ack tail, said again until acked, listed in the brief. An info line
// that waits goes with it at once, on the notices' line.
func TestActLineIsUnchangedAndCarriesTheInfoLines(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	since := f.s.clock(*f.now)

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	_, err := f.s.NoteSent("remote box-ops/ops", "«one»", *f.now)
	require.NoError(t, err)
	_, err = f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)
	f.s.AddNotes([]string{"designer: ended"})

	lines := strings.Split(f.s.deliver(*f.now), "\n")
	require.Len(t, lines, 3, "no wait: a line goes anyway")
	assert.Equal(t, "[zmina] S-2 mrs: acme/site!12 approved (zmina S-2)", lines[0])
	assert.Equal(t, "[zmina] S-1 remote box-ops/ops: «one»"+Lateness(*f.now, *f.now, f.s.Crew.Location())+" (zmina S-1)", lines[1],
		"a line of another crew is never an info line")
	assert.True(t, strings.HasPrefix(lines[2], "[zmina] FYI designer: T-1 result ready (worker-reported)"), lines[2])
	assert.True(t, strings.HasSuffix(lines[2], " · designer: ended"), "a health note that is not an info line has no mark")

	p.showAll()
	f.s.deliver(*f.now)
	require.Len(t, f.s.Pending("dispatcher"), 2, "seen closes the info line only")
	assert.Contains(t, f.s.PendingBrief("dispatcher", false), "S-2 (since "+since+") mrs: acme/site!12 approved")
	assert.NotContains(t, f.s.PendingBrief("dispatcher", false), "result ready")

	*f.now = f.now.Add(f.s.Crew.RenotifyD())
	f.s.renotify(*f.now)
	again := f.s.deliver(*f.now)
	assert.Contains(t, again, "S-2 (still pending since "+since+") mrs: acme/site!12 approved")
	assert.NotContains(t, again, "result ready")

	acked, warn := f.s.AckLines([]string{"S-1", "S-2"}, false, "dispatcher", "dispatcher")
	assert.Equal(t, []string{"S-1", "S-2"}, acked)
	assert.Empty(t, warn)
}

// The info lines wait up to infoRide for a line that goes anyway — a signal, another crew's line, the dispatcher's
// own queued line, a health note — then go alone.
func TestInfoLinesRideWithTheNextPost(t *testing.T) {
	cases := []struct {
		name  string
		after time.Duration
		with  func(t *testing.T, f fix)
		lines int
	}{
		{"alone inside the wait", infoRide - time.Second, nil, 0},
		{"alone once the wait is over", infoRide, nil, 1},
		{"with a signal", time.Second, func(t *testing.T, f fix) {
			_, err := f.s.Note("mrs", "acme/site!12 approved", false)
			require.NoError(t, err)
		}, 2},
		{"with another crew's line", time.Second, func(t *testing.T, f fix) {
			_, err := f.s.NoteSent("remote box-ops/ops", "«one»", *f.now)
			require.NoError(t, err)
		}, 2},
		{"with the dispatcher's own queued line", time.Second, func(_ *testing.T, f fix) {
			f.s.queueRecovery("/state/pack-1.json")
		}, 2},
		{"with a health note", time.Second, func(_ *testing.T, f fix) {
			f.s.AddNotes([]string{"designer: ended"})
		}, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, p := nativeFixture(t, crewYAML)
			upNative(t, f, "dispatcher")

			saveResult(t, f, "designer", "T-1")
			f.s.NotifyRecords()
			assert.Empty(t, f.s.deliver(*f.now))

			*f.now = f.now.Add(c.after)
			if c.with != nil {
				c.with(t, f)
			}

			text := f.s.deliver(*f.now)
			if c.lines == 0 {
				assert.Empty(t, text)
				assert.Empty(t, p.raw)
				assert.Len(t, f.s.State.Signals, 1, "it waits, queued")

				return
			}

			lines := strings.Split(text, "\n")
			require.Len(t, lines, c.lines)
			assert.Contains(t, lines[len(lines)-1], "FYI designer: T-1 result ready (worker-reported)")
			assert.Len(t, p.raw, 1, "one post")
			assert.Empty(t, f.s.State.Signals)
		})
	}
}

// A result's notice is made whatever the role wrote of the task itself, before or after: the role's own line names
// the task but need not be about its result, and a notice dropped for it would leave the result untold.
func TestResultReadyIsToldBesideTheRolesOwnLine(t *testing.T) {
	for _, first := range []string{"line", "result"} {
		t.Run(first+" first", func(t *testing.T) {
			f, _ := nativeFixture(t, crewYAML)
			upNative(t, f, "dispatcher")
			f.status("dispatcher", "waiting", *f.now)

			line := func() {
				_, err := f.s.Note("designer", "T-1: which of the two layouts?", false)
				require.NoError(t, err)
			}
			result := func() {
				saveResult(t, f, "designer", "T-1")
				f.s.NotifyRecords()
			}

			steps := []func(){line, result}
			if first == "result" {
				steps = []func(){result, line}
			}

			steps[0]()
			*f.now = f.now.Add(time.Minute)
			steps[1]()

			told, own := 0, 0

			for _, sig := range f.s.State.Signals {
				switch {
				case strings.Contains(sig, "T-1"+resultReady):
					told++

					assert.NotRegexp(t, `^S-[0-9]+ `, sig, "an info line: no id")
				case strings.Contains(sig, "which of the two layouts"):
					own++
				}
			}

			assert.Equal(t, 1, told, "the result is told")
			assert.Equal(t, 1, own, "the role's line waits too")
		})
	}
}

// A role's own line is never an info line, even with the text of one that waits or went: it gets its id.
func TestRoleLineWithAnInfoLinesTextIsActedOn(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	require.Len(t, f.s.State.Signals, 1)

	notice := strings.TrimPrefix(f.s.State.Signals[0], "designer: ")
	queued, err := f.s.Note("designer", notice, false)
	require.NoError(t, err)
	assert.False(t, queued, "the same text waits already")

	line := f.s.deliver(*f.now)
	assert.True(t, strings.HasPrefix(line, "[zmina] S-1 designer: T-1 result ready"), line)
	assert.True(t, strings.HasSuffix(line, "(repeated 2 times) (zmina S-1)"), line)
	assert.Len(t, f.s.Pending("dispatcher"), 1)
}

// Typed delivery shows no seen: an info line goes in the typed line behind its mark, with no id, and is closed when
// the line lands. One that never lands is kept as failed and goes back as an info line.
func TestInfoLineTyped(t *testing.T) {
	typed := func(t *testing.T, takes bool) (fix, string) {
		t.Helper()

		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "dispatcher")
		p.unusable = true // no inbox socket: its lines are typed, and it still acks

		if !takes {
			f.tm.onType = nil // the input is empty and the status never changes: unconfirmed
		}

		saveResult(t, f, "designer", "T-1")
		f.s.NotifyRecords()
		assert.Empty(t, f.s.deliver(*f.now), "alone, it waits for a line to go with")
		assert.Empty(t, f.tm.typed["dispatcher"])

		_, err := f.s.Note("mrs", "acme/site!12 approved", false)
		require.NoError(t, err)

		line := f.s.deliver(*f.now)
		require.Equal(t, []string{line}, f.tm.typed["dispatcher"])
		assert.True(t, strings.HasPrefix(line, "[zmina] S-1 mrs: acme/site!12 approved · FYI designer: T-1 result ready"), line)
		assert.True(t, strings.HasSuffix(line, ") (zmina S-1)"), "only the signal to act on has an id: "+line)
		assert.Len(t, f.s.State.Acks, 1)
		assert.Empty(t, f.s.State.Signals)
		assert.Empty(t, closed(f), "not closed before it lands")

		return f, line
	}

	t.Run("landed", func(t *testing.T) {
		f, _ := typed(t, true)

		*f.now = f.now.Add(time.Second)
		f.status("dispatcher", "busy", *f.now) // it took the line
		assert.True(t, f.s.landed("dispatcher", *f.now))
		require.Len(t, closed(f), 1)
		assert.True(t, strings.HasPrefix(closed(f)[0], "typed: designer: T-1 result ready"), closed(f)[0])

		f.status("dispatcher", "idle", *f.now)
		*f.now = f.now.Add(f.s.Crew.RenotifyD())
		f.s.renotify(*f.now)
		again := f.s.deliver(*f.now)
		assert.Contains(t, again, "S-1 (still pending since ")
		assert.NotContains(t, again, "result ready", "the info line is not said again")
		assert.Empty(t, f.s.State.Info)
	})

	t.Run("never landed", func(t *testing.T) {
		f, line := typed(t, false)

		*f.now = f.now.Add(confirmAfter)
		assert.True(t, f.s.landed("dispatcher", *f.now))

		rs := f.s.State.Role("dispatcher")
		require.Len(t, rs.Failed, 1, "kept as failed, not lost")
		assert.Equal(t, line, rs.Failed[0].Line)
		assert.Empty(t, closed(f))

		_, err := f.s.Retry("dispatcher")
		require.NoError(t, err)
		require.Len(t, f.s.State.Signals, 2)
		assert.Equal(t, "S-1 mrs: acme/site!12 approved", f.s.State.Signals[0])
		assert.True(t, f.s.isInfo(f.s.State.Signals[1]), "it goes back as the info line it was")

		again := f.s.deliver(*f.now)
		assert.True(t, strings.HasPrefix(again, "[zmina] S-1 mrs: acme/site!12 approved · FYI designer: T-1 result ready"), again)
		assert.Len(t, f.s.State.Acks, 1)
	})

	t.Run("alone once the wait is over", func(t *testing.T) {
		f := fixtureWith(t, crewYAML) // a dispatcher that is typed into and acks nothing
		require.NoError(t, f.s.Up("dispatcher"))
		f.status("dispatcher", "idle", *f.now)
		*f.now = f.now.Add(3 * time.Minute)

		saveResult(t, f, "designer", "T-1")
		f.s.NotifyRecords()
		assert.Empty(t, f.s.deliver(*f.now))

		*f.now = f.now.Add(infoRide)
		line := f.s.deliver(*f.now)
		assert.True(t, strings.HasPrefix(line, "[zmina] FYI designer: T-1 result ready"), line)
		assert.NotContains(t, line, "(zmina S-")
		assert.Equal(t, []string{line}, f.tm.typed["dispatcher"])
	})
}

// A post that went to a conversation that ended before it showed goes back as it waited: the info line an info line.
func TestInfoLineGivenBackStaysAnInfoLine(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	*f.now = f.now.Add(infoRide)
	first := f.s.deliver(*f.now)
	require.NotEmpty(t, first)

	// The dispatcher restarted on a fresh conversation before it read the post.
	f.rg.byPID[f.tm.up["dispatcher"]].SessionID = "sid-dispatcher-2"
	*f.now = f.now.Add(time.Minute)
	assert.Empty(t, f.s.deliver(*f.now), "back in the signals")
	require.Len(t, f.s.State.Signals, 1)
	assert.True(t, f.s.isInfo(f.s.State.Signals[0]))
	assert.Equal(t, first, f.s.deliver(*f.now), "posted again, the same line")
	assert.Empty(t, f.s.State.Acks)
	assert.Empty(t, closed(f))

	p.showAll()
	f.s.deliver(*f.now)
	assert.Len(t, closed(f), 1)
}

// info_lines: false is zmina as it was: every such line waits for its ack under an id, nothing is held back or
// dropped, and the boot line says nothing of FYI.
func TestInfoLinesOff(t *testing.T) {
	f, p := nativeFixture(t, "info_lines: false\n"+crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("designer", "T-1 is on the branch", false)
	require.NoError(t, err)
	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	require.NoError(t, f.s.Up("designer"))
	f.s.State.Role("designer").ReleaseIdle = 34 * time.Minute
	_, note := f.s.putDown("designer")
	f.s.addNotes([]string{note, "harvester: started again (keep: always)"})

	lines := strings.Split(f.s.deliver(*f.now), "\n")
	require.Len(t, lines, 2, "at once")
	assert.True(t, strings.HasPrefix(lines[0], "[zmina] S-1 designer: T-1 is on the branch · S-2 designer: T-1 result ready (worker-reported): Recorded (zmina task result T-1 --revision "), lines[0])
	assert.True(t, strings.HasSuffix(lines[0], ") (zmina S-1 S-2)"), lines[0])
	assert.Equal(t, "[zmina] released designer after 34m idle · harvester: started again (keep: always)", lines[1])
	assert.Empty(t, f.s.State.Info)
	assert.Len(t, f.s.Pending("dispatcher"), 2)

	p.showAll()
	f.s.deliver(*f.now)
	assert.Empty(t, closed(f))
	assert.Len(t, f.s.Pending("dispatcher"), 2, "seen closes nothing")

	d, err := f.s.Role("dispatcher")
	require.NoError(t, err)
	assert.NotContains(t, f.s.BootText(d), "FYI")
	assert.Contains(t, f.s.BootText(d), f.s.ackRule())
}

// The switch turned off while info lines wait: they become lines to act on.
func TestInfoLinesTurnedOffWhileOneWaits(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	require.Len(t, f.s.State.Info, 1)

	off := false
	f.s.Crew.InfoLines = &off

	line := f.s.deliver(*f.now)
	assert.True(t, strings.HasPrefix(line, "[zmina] S-1 designer: T-1 result ready"), line)
	assert.True(t, strings.HasSuffix(line, " (zmina S-1)"), line)
	assert.Len(t, f.s.Pending("dispatcher"), 1)
}

// The dispatcher's boot line says FYI lines need no ack; a seat's, which gets none, does not.
func TestBootRuleNamesInfoLines(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)

	d, err := f.s.Role("dispatcher")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(f.s.bootText(d, false), f.s.ackRule()+" "+infoRule))

	r, err := f.s.Role("designer")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(f.s.bootText(r, false), f.s.ackRule()))
	assert.NotContains(t, f.s.BootText(r), "FYI")
}

// oldState is a state.json as a zmina before the classes wrote it: a line delivered and unacked, a signal queued
// with its id, one queued without (a dispatcher that did not ack yet), a release notice.
const oldState = `{
  "version": 2,
  "ack_seq": 7,
  "acks": {
    "S-6": {"id": "S-6", "role": "dispatcher", "text": "designer: T-1 result ready (worker-reported): Recorded (zmina task result T-1 --revision abc)", "first": "2026-10-24T11:00:00Z", "last": "2026-10-24T11:00:00Z"},
    "S-7": {"id": "S-7", "role": "dispatcher", "text": "designer: T-2 result ready (worker-reported): Recorded (zmina task result T-2 --revision def)"}
  },
  "signals": [
    "S-7 designer: T-2 result ready (worker-reported): Recorded (zmina task result T-2 --revision def)",
    "designer: T-3 result ready (worker-reported): Recorded (zmina task result T-3 --revision 123)"
  ],
  "signal_counts": {"S-7 designer: T-2 result ready (worker-reported): Recorded (zmina task result T-2 --revision def)": 1},
  "notes": ["released designer after 31m idle"]
}`

// A state written before the classes reads, and every line of it is one to act on: its ids, its ack tail, said
// again, listed in the brief; its notes as they were.
func TestOldStateLinesAreActedOn(t *testing.T) {
	st, legacy, err := decode([]byte(oldState))
	require.NoError(t, err)
	require.False(t, legacy)
	assert.Empty(t, st.Info)

	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	st.Roles = f.s.State.Roles
	f.s.State = st

	assert.Contains(t, f.s.PendingBrief("dispatcher", false), "S-6 (since ")

	lines := strings.Split(f.s.deliver(*f.now), "\n")
	require.Len(t, lines, 2, "at once")
	assert.Equal(t, "[zmina] S-7 designer: T-2 result ready (worker-reported): Recorded (zmina task result T-2 --revision def) · "+
		"S-8 designer: T-3 result ready (worker-reported): Recorded (zmina task result T-3 --revision 123) (zmina S-7 S-8)", lines[0])
	assert.Equal(t, "[zmina] released designer after 31m idle", lines[1])

	p.showAll()
	f.s.deliver(*f.now)
	assert.Empty(t, closed(f))
	assert.Len(t, f.s.Pending("dispatcher"), 3, "seen closes none of them")

	*f.now = f.now.Add(f.s.Crew.RenotifyD())
	f.s.renotify(*f.now)
	again := f.s.deliver(*f.now)
	for _, id := range []string{"S-6", "S-7", "S-8"} {
		assert.Contains(t, again, id+" (still pending since ", "an old line is said again")
	}
}

// Neither the brief nor the recovery pack lists an info line, queued or delivered; the result itself stays in the
// pack as an unread result.
func TestBriefAndPackLeaveInfoLinesOut(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	f.status("dispatcher", "waiting", *f.now)

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	_, err := f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)
	require.Len(t, f.s.State.Signals, 2)

	_, pack, err := f.s.RecoveryPack()
	require.NoError(t, err)
	require.Len(t, pack.Entries["signals"], 1)
	assert.Equal(t, "S-1 mrs: acme/site!12 approved", pack.Entries["signals"][0].Text)
	assert.Len(t, pack.Entries["unread_results"], 1)

	f.status("dispatcher", "idle", *f.now)
	require.NotEmpty(t, f.s.deliver(*f.now))
	p.showAll()
	f.s.deliver(*f.now)

	brief := f.s.PendingBrief("dispatcher", false)
	assert.Contains(t, brief, "S-1 (since ")
	assert.NotContains(t, brief, "result ready")

	_, pack, err = f.s.RecoveryPack()
	require.NoError(t, err)
	require.Len(t, pack.Entries["unacked"], 1)
	assert.Contains(t, pack.Entries["unacked"][0].Text, "mrs: acme/site!12 approved")
	assert.Empty(t, pack.Entries["signals"])
}

// zmina ack of an id that waits for no ack — closed already, or never given, as an info line's — changes nothing.
func TestAckOfAClosedOrInfoLineIsANoOp(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)
	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	require.NotEmpty(t, f.s.deliver(*f.now))

	acked, warn := f.s.AckLines([]string{"S-1"}, false, "dispatcher", "dispatcher")
	assert.Equal(t, []string{"S-1"}, acked)
	assert.Empty(t, warn)

	posted := len(f.s.State.Role("dispatcher").Posted)

	for _, ids := range [][]string{{"S-1"}, {"S-2"}, {"2"}} {
		acked, warn = f.s.AckLines(ids, false, "dispatcher", "dispatcher")
		assert.Empty(t, acked)
		require.Len(t, warn, 1)
		assert.Contains(t, warn[0], "waits for no ack")
	}

	acked, warn = f.s.AckLines(nil, true, "dispatcher", "dispatcher")
	assert.Empty(t, acked)
	assert.Empty(t, warn)
	assert.Len(t, f.s.State.Role("dispatcher").Posted, posted, "the post in flight is left alone")

	p.showAll()
	f.s.deliver(*f.now)
	assert.Len(t, closed(f), 1, "the info line is closed by seen, as ever")
}
