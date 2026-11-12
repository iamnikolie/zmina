package shift

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/watch"
)

// notifying gives the crew a notify command, so that a reminder for the owner has a part outside the lock.
func notifying(f fix) { f.s.Crew.Notify = crew.Command{Argv: []string{"push"}} }

// beat is RunTick on the fixture's shift with rang as the part outside the lock. The lock is a flag: rang must run
// with it free, and what is recorded after it must not reach tmux (it is gone for those sections).
func beat(t *testing.T, f fix, rang func(Rung) (string, error)) []string {
	t.Helper()

	f.s.Rang = nil // a lock gives a fresh shift in production

	var (
		lines  []string
		held   bool
		locks  int
		tmuxed = f.s.Tmux
	)

	locked := func(fn func(s *Shift) error) error {
		locks++
		held = true

		defer func() { held = false }()

		if locks > 1 {
			f.s.Tmux = nil
			defer func() { f.s.Tmux = tmuxed }()
		}

		return fn(f.s)
	}

	err := RunTick(locked, func([]WatchRun) []watch.Result { return nil }, func(r Rung) (string, error) {
		assert.False(t, held, "the part outside the lock runs without it")

		return rang(r)
	}, func(l []string) { lines = append(lines, l...) })
	require.NoError(t, err)

	return lines
}

// outcome is a part outside the lock that counts its runs and ends as err says.
type outcome struct {
	runs []Rung
	err  error
}

func (o *outcome) run(r Rung) (string, error) {
	o.runs = append(o.runs, r)

	return "", o.err
}

func owner(t *testing.T) (fix, *alarm.Store, alarm.Alarm, *outcome) {
	t.Helper()

	f, al, _ := alarmed(t)
	notifying(f)

	a := set(t, f, al, alarm.Alarm{Kind: alarm.Remind, At: *f.now, Text: "pay the invoice"})

	return f, al, a, &outcome{err: errors.New("notify: exit status 1")}
}

func TestAlarmIsNotRungWhileItsPartOutsideTheLockFailed(t *testing.T) {
	f, al, a, out := owner(t)

	beat(t, f, out.run)
	require.Len(t, out.runs, 1)
	assert.Equal(t, Rung{ID: a.ID, Notify: "pay the invoice"}, out.runs[0])

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Set, got.State, "not rung: its effect did not go out")
	assert.True(t, got.Rang.IsZero())
	assert.Equal(t, "pay the invoice", got.Owed, "what it still owes")
	assert.Equal(t, 1, got.Tries)
	assert.Equal(t, *f.now, got.Tried)
	assert.Equal(t, "notify: exit status 1", got.TryErr)
	assert.Equal(t, "A-1", got.Ask, "what it did inside the lock stays done")
	assert.Equal(t, []string{a.ID}, ids(al.Due(*f.now)), "still due")
}

func TestAlarmIsNotRungWhileItsPartIsRunning(t *testing.T) {
	f, al, a, _ := owner(t)

	beat(t, f, func(r Rung) (string, error) {
		got, err := al.Get(r.ID)
		require.NoError(t, err)
		assert.Equal(t, alarm.Set, got.State, "set while its part runs, as a beat that dies then leaves it")
		assert.Equal(t, 1, got.Tries, "the try is counted before it starts")

		return "", nil
	})

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State)
}

func TestFailedPartIsTriedAgainAfterTheGapNotEveryBeat(t *testing.T) {
	f, al, a, out := owner(t)
	start := *f.now

	beat(t, f, out.run)
	require.Len(t, out.runs, 1)

	for *f.now = start.Add(time.Minute); f.now.Sub(start) < effectGap; *f.now = f.now.Add(time.Minute) {
		beat(t, f, out.run)
		require.Len(t, out.runs, 1, "not asked again at %s", f.now.Sub(start))
	}

	assert.Equal(t, start.Add(effectGap), *f.now)
	lines := beat(t, f, out.run)
	require.Len(t, out.runs, 2, "tried again once the gap is over")
	assert.Equal(t, out.runs[0], out.runs[1], "the same line")
	assert.Contains(t, lines, a.ID+": trying again (2 of "+fmt.Sprint(effectTries)+")")

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Set, got.State)
	assert.Equal(t, 2, got.Tries)
	assert.Equal(t, *f.now, got.Tried)

	// Back up: the third try goes out, and the alarm is rung.
	out.err = nil
	*f.now = f.now.Add(effectGap)
	beat(t, f, out.run)
	require.Len(t, out.runs, 3)

	got, err = al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State)
	assert.Equal(t, *f.now, got.Rang, "rung when it went out")
	assert.Empty(t, got.Owed)
	assert.Empty(t, got.TryErr)
	assert.Equal(t, 3, got.Tries, "how many it took stays")

	// Rung once: nothing goes out again, and the todo has the one check.
	for range 3 {
		*f.now = f.now.Add(effectGap)
		beat(t, f, out.run)
	}

	assert.Len(t, out.runs, 3, "a part that went out is not run again")
	assert.Empty(t, al.Due(*f.now))
}

func TestAPartThatWentOutIsNotRunAgain(t *testing.T) {
	f, al, a, out := owner(t)
	out.err = nil

	beat(t, f, out.run)
	require.Len(t, out.runs, 1)

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State, "rung in the beat whose part went out")
	assert.Equal(t, *f.now, got.Rang)
	assert.Equal(t, 1, got.Tries)

	for range 2 * effectTries {
		*f.now = f.now.Add(effectGap)
		beat(t, f, out.run)
	}

	assert.Len(t, out.runs, 1)
}

func TestAlarmIsGivenUpAfterTheTriesAndTheDispatcherHearsOnce(t *testing.T) {
	f, al, a, out := owner(t)

	var last []string

	for i := range effectTries {
		if i > 0 {
			*f.now = f.now.Add(effectGap)
		}

		last = beat(t, f, out.run)
	}

	assert.Len(t, out.runs, effectTries, "the cap is on tries")

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Failed, got.State)
	assert.Equal(t, effectTries, got.Tries)
	assert.Equal(t, "notify: exit status 1", got.TryErr)
	assert.Empty(t, al.Pending(), "not pending: it does not ring again")
	assert.Contains(t, strings.Join(last, "\n"), a.ID+": not delivered after "+fmt.Sprint(effectTries)+" tries")
	assert.True(t, f.ev.has("", "alarm-failed"))

	var notes []string

	for _, n := range f.s.State.Notes {
		if strings.HasPrefix(n, a.ID+": ") {
			notes = append(notes, n)
		}
	}

	require.Len(t, notes, 1, "the dispatcher gets one note")
	assert.Contains(t, notes[0], "alarm for you (pay the invoice)")
	assert.Contains(t, notes[0], fmt.Sprintf("not delivered after %d tries: notify: exit status 1", effectTries))
	assert.Contains(t, notes[0], "zmina remind ls")
	assert.Empty(t, f.tm.typed["dispatcher"], "queued under the lock, not typed there")

	// It is typed on the next beat once, and nothing tries or says it again.
	f.status("dispatcher", "idle", *f.now)
	*f.now = f.now.Add(bootPause)
	beat(t, f, out.run)

	typed := strings.Join(f.tm.typed["dispatcher"], "\n")
	assert.Equal(t, 1, strings.Count(typed, a.ID+": alarm for you"), "told once")

	for range 3 {
		*f.now = f.now.Add(effectGap)
		beat(t, f, out.run)
	}

	assert.Len(t, out.runs, effectTries, "a failed alarm is not tried again")
	assert.Equal(t, 1, strings.Count(strings.Join(f.tm.typed["dispatcher"], "\n"), a.ID+": alarm for you"))
}

// A beat that dies during the last try leaves no result: the next one finds the count spent and says so.
func TestAlarmWhoseLastTryLeftNoResultIsGivenUp(t *testing.T) {
	f, al, a, out := owner(t)
	notifying(f)

	a.Owed, a.Tries, a.Tried = "pay the invoice", effectTries, *f.now
	require.NoError(t, al.Put(a))

	*f.now = f.now.Add(effectGap)
	f.s.Tick()
	assert.Empty(t, f.s.Rang, "nothing more is tried")

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Failed, got.State)
	assert.Contains(t, got.TryErr, "is not known")
	assert.Len(t, out.runs, 0)

	var notes int

	for _, n := range f.s.State.Notes {
		if strings.HasPrefix(n, a.ID+": alarm for you") {
			notes++
		}
	}

	assert.Equal(t, 1, notes)
}

func TestARecordedPartIsNotWrittenOverAnAlarmTakenOffMeanwhile(t *testing.T) {
	f, al, a, _ := owner(t)

	beat(t, f, func(r Rung) (string, error) {
		_, err := al.TakeOff(r.ID, *f.now)
		require.NoError(t, err)

		return "", errors.New("notify: exit status 1")
	})

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Off, got.State, "off stays off")
}

// Each part is recorded in a lock of its own before the next one runs: the duplicate a crash leaves is one part.
func TestEachPartIsRecordedBeforeTheNextOneRuns(t *testing.T) {
	f, al, first, _ := owner(t)
	second := set(t, f, al, alarm.Alarm{Kind: alarm.Remind, At: *f.now, Text: "renew the domain"})

	var order []string

	beat(t, f, func(r Rung) (string, error) {
		for _, id := range []string{first.ID, second.ID} {
			got, err := al.Get(id)
			require.NoError(t, err)

			switch {
			case id == r.ID:
				assert.Equal(t, alarm.Set, got.State, "%s is not rung while its part runs", id)
			case len(order) > 0 && id == order[0]:
				assert.Equal(t, alarm.Rang, got.State, "%s was recorded before the next part ran", id)
			}
		}

		order = append(order, r.ID)

		return "", nil
	})

	assert.Equal(t, []string{first.ID, second.ID}, order)
}

func TestRecordingAPartDoesNotTouchTmux(t *testing.T) {
	f, al, a, _ := owner(t)
	f.s.Tick()

	a, err := al.Get(a.ID)
	require.NoError(t, err)
	require.NotEmpty(t, a.Owed)

	// Past the tries: the record gives the alarm up and queues the note, with no tmux to call (nil panics).
	a.Tries = effectTries
	require.NoError(t, al.Put(a))

	tm := f.s.Tmux
	f.s.Tmux = nil

	defer func() { f.s.Tmux = tm }()

	lines := f.s.RungDone(a.ID, errors.New("notify: timed out"))
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "not delivered after")
}

// A tell to another crew is a part outside the lock like the notify command: it is tried again, and given up.
func TestTellThatCouldNotBeQueuedIsTriedAgain(t *testing.T) {
	f, al, _ := alarmed(t)
	a := set(t, f, al, alarm.Alarm{Kind: alarm.Remind, To: "box/ops", At: *f.now, Text: "rotate the dump"})
	out := &outcome{err: errors.New("tell box/ops: outbox is full")}

	beat(t, f, out.run)
	require.Len(t, out.runs, 1)
	assert.Equal(t, Rung{ID: a.ID, Tell: "box/ops", Line: "[zmina alarm] rotate the dump"}, out.runs[0])

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Set, got.State)
	assert.Equal(t, "tell box/ops: outbox is full", got.TryErr)

	out.err = nil
	*f.now = f.now.Add(effectGap)
	beat(t, f, out.run)
	require.Len(t, out.runs, 2)
	assert.Equal(t, out.runs[0], out.runs[1])

	got, err = al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State)
}

// A part that cannot be saved as owed is not run: it would run again with nothing to say it had.
func TestPartIsNotRunForAnAlarmThatWasNotSaved(t *testing.T) {
	f, al, _ := alarmed(t)
	notifying(f)
	set(t, f, al, alarm.Alarm{Kind: alarm.Remind, At: *f.now, Text: "pay the invoice"})

	f.s.Alarms = failingPut{al}
	f.s.Tick()
	assert.Empty(t, f.s.Rang)
}

type failingPut struct{ *alarm.Store }

func (failingPut) Put(alarm.Alarm) error { return errors.New("disk full") }

// No notify command: a reminder has nothing outside the lock, and is rung at once, as it was.
func TestReminderWithoutANotifyCommandIsRungAtOnce(t *testing.T) {
	f, al, _ := alarmed(t)
	a := set(t, f, al, alarm.Alarm{Kind: alarm.Remind, At: *f.now, Text: "standup"})

	f.s.Tick()
	assert.Empty(t, f.s.Rang)

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State)
	assert.Equal(t, *f.now, got.Rang)
	assert.Empty(t, got.Owed)
}

// An alarm the build before this one recorded as rung is rung: nothing owes, nothing is tried.
func TestAlarmRungByTheOldBuildStaysRung(t *testing.T) {
	f, al, _ := alarmed(t)
	notifying(f)

	old := `{"id": "R-1", "kind": "remind", "at": "2026-10-24T11:00:00Z", "text": "standup", "state": "rang",
  "rang": "2026-10-24T11:00:30Z", "ask": "A-1", "created": "2026-10-24T10:00:00Z", "updated": "2026-10-24T11:00:30Z"}`
	require.NoError(t, os.MkdirAll(al.Dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(al.Dir, "R-1.json"), []byte(old), 0o600))

	for range 3 {
		*f.now = f.now.Add(effectGap)
		f.s.Tick()
		assert.Empty(t, f.s.Rang)
	}

	got, err := al.Get("R-1")
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State)
	assert.Empty(t, got.Owed)
	assert.Zero(t, got.Tries)
	assert.Empty(t, al.Pending())
}

// An alarm the old build set and left pending rings the new way: it owes its part, and is rung when it goes out.
func TestAlarmSetByTheOldBuildRingsTheNewWay(t *testing.T) {
	f, al, _ := alarmed(t)
	notifying(f)

	old := `{"id": "R-1", "kind": "remind", "at": "2026-10-24T11:00:00Z", "text": "standup", "state": "set",
  "created": "2026-10-24T10:00:00Z", "updated": "2026-10-24T10:00:00Z"}`
	require.NoError(t, os.MkdirAll(al.Dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(al.Dir, "R-1.json"), []byte(old), 0o600))

	out := &outcome{}
	beat(t, f, out.run)
	require.Len(t, out.runs, 1)

	got, err := al.Get("R-1")
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State)
}

func ids(as []alarm.Alarm) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.ID)
	}

	return out
}

// What an alarm did inside the lock is done once: the tries that follow hand over its line only, and the todo keeps
// the one check, however many of them it takes.
func TestTriesAfterTheFirstDoNotPutTheReminderOnTheTodoAgain(t *testing.T) {
	f, al, as := alarmed(t)
	notifying(f)
	set(t, f, al, alarm.Alarm{Kind: alarm.Remind, At: *f.now, Text: "pay the invoice"})

	out := &outcome{err: errors.New("notify: exit status 1")}

	for range effectTries + 2 {
		beat(t, f, out.run)
		*f.now = f.now.Add(effectGap)
	}

	require.Len(t, out.runs, effectTries)
	all, _, _ := as.Scan()
	assert.Len(t, all, 1, "one check on the todo")
}

// An alarm that went off and still owes its line is not moved: another time would bring the line back, not the alarm.
func TestAnAlarmThatOwesItsLineIsNotMoved(t *testing.T) {
	f, al, a, out := owner(t)

	beat(t, f, out.run)

	_, err := al.Move(a.ID, f.now.Add(24*time.Hour), "tomorrow", *f.now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already went off")

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.False(t, got.At.After(*f.now), "its time stays")
}
