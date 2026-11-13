package plugin

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func item(id string, opened time.Time, blocking bool) Thing {
	return Thing{Key: ItemKey(id), ID: id, Class: ItemThing, Opened: opened, Blocking: blocking}
}

func dialog(role, fp string) Thing {
	id := DialogID(role, fp)

	return Thing{Key: DialogKey(id), ID: id, Class: DialogThing, Role: role}
}

func plug(on ...string) crew.Plugin {
	return crew.Plugin{Name: "pager", On: on, QuietWhenActive: "off"}
}

func kinds(p Plan) []string {
	var out []string
	for _, d := range p.Send {
		out = append(out, d.Kind+" "+d.Entry.ID)
	}

	return out
}

// started is a state the plugin was first looked at some time ago.
func started(at time.Time) *State {
	st := &State{V: Version}
	st.Observe(nil, at, true)
	st.Since = at

	return st
}

// An item goes once: a plan after it was told sends nothing; a blocking item goes as blocking instead of as ask, never as both.
func TestPlanOnceAndBlockingInsteadOfAsk(t *testing.T) {
	st := started(t0)
	things := []Thing{item("A-1", t0.Add(time.Minute), false), item("A-2", t0.Add(2*time.Minute), true)}
	now := t0.Add(3 * time.Minute)

	plan := Make(Inputs{Plugin: plug(), State: st, Things: things, Whole: true, Now: now})
	assert.Equal(t, []string{"blocking A-2", "ask A-1"}, kinds(plan), "a blocking item first, then oldest first; one event each")

	for _, d := range plan.Send {
		st.Succeed(d, now)
	}

	assert.Empty(t, Make(Inputs{Plugin: plug(), State: st, Things: things, Whole: true, Now: now.Add(time.Hour)}).Send, "once")

	// A plugin that takes only ask is told of a blocking item as an ask; one that takes only blocking never of a plain ask.
	st2 := started(t0)
	assert.Equal(t, []string{"ask A-1", "ask A-2"}, kinds(Make(Inputs{Plugin: plug("ask"), State: st2, Things: things, Whole: true, Now: now})))
	assert.Equal(t, []string{"blocking A-2"}, kinds(Make(Inputs{Plugin: plug("blocking"), State: started(t0), Things: things, Whole: true, Now: now})))
	assert.Empty(t, Make(Inputs{Plugin: plug("answered"), State: started(t0), Things: things, Whole: true, Now: now}).Send)
}

// An item open before the plugin was first looked at is not new to it, unless it holds a role up.
func TestPlanOldItemsAreNotNew(t *testing.T) {
	st := &State{V: Version}
	old := t0.Add(-time.Hour)
	things := []Thing{item("A-1", old, false), item("A-2", old, true), item("A-3", t0.Add(-30*time.Second), false)}

	st.Observe(things, t0, true)
	assert.Equal(t, t0.Add(-time.Minute), st.Since, "the beat before the first look still counts as new")

	assert.Equal(t, []string{"blocking A-2", "ask A-3"}, kinds(Make(Inputs{Plugin: plug(), State: st, Things: things, Whole: true, Now: t0})))
}

// A reminder goes as reminder to a plugin that takes it, else as an ask like any other.
func TestPlanReminder(t *testing.T) {
	r := item("A-9", t0.Add(time.Minute), false)
	r.Reminder = true
	now := t0.Add(2 * time.Minute)

	assert.Equal(t, []string{"reminder A-9"}, kinds(Make(Inputs{Plugin: plug("reminder", "ask"), State: started(t0), Things: []Thing{r}, Whole: true, Now: now})))
	assert.Equal(t, []string{"ask A-9"}, kinds(Make(Inputs{Plugin: plug(), State: started(t0), Things: []Thing{r}, Whole: true, Now: now})))
	assert.Empty(t, Make(Inputs{Plugin: plug("blocking"), State: started(t0), Things: []Thing{r}, Whole: true, Now: now}).Send)
}

// While the owner was at the machine within the span, the push is held; it goes when the span passes if the item is still
// open, and never if it was answered meanwhile. An answered is never held.
func TestPlanQuietWhenActive(t *testing.T) {
	pl := plug()
	pl.QuietWhenActive = "5m"

	st := started(t0)
	a1 := item("A-1", t0.Add(time.Minute), false)
	active := t0.Add(2 * time.Minute)
	asked := 0
	owner := func() time.Time { asked++; return active }

	in := Inputs{Plugin: pl, State: st, Things: []Thing{a1}, Whole: true, OwnerActive: owner}

	in.Now = t0.Add(3 * time.Minute)
	assert.Empty(t, Make(in).Send, "the owner typed a minute ago: held")

	in.Now = t0.Add(6*time.Minute + 59*time.Second)
	assert.Empty(t, Make(in).Send, "still inside the span")

	in.Now = t0.Add(7 * time.Minute)
	assert.Equal(t, []string{"ask A-1"}, kinds(Make(in)), "the span has passed and it is still open")

	// Answered at the keyboard inside the span: it never pages.
	in.Things, in.Now = nil, t0.Add(8*time.Minute)
	assert.Empty(t, Make(in).Send)

	// The answered of a thing told of goes at once, quiet or not.
	told := started(t0)
	told.Succeed(Due{Key: a1.Key, Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask}}, t0.Add(time.Minute))
	in = Inputs{Plugin: pl, State: told, Things: nil, Whole: true, Now: t0.Add(3 * time.Minute), OwnerActive: func() time.Time { return t0.Add(3 * time.Minute) }}
	assert.Equal(t, []string{"answered A-1"}, kinds(Make(in)))

	// The owner is asked about only when it can matter.
	asked = 0
	Make(Inputs{Plugin: pl, State: started(t0), Things: nil, Whole: true, Now: t0, OwnerActive: owner})
	assert.Zero(t, asked, "nothing to send, nothing asked")

	// off: never held; unknown activity: never held.
	off := plug()
	assert.Equal(t, []string{"ask A-1"}, kinds(Make(Inputs{Plugin: off, State: started(t0), Things: []Thing{a1}, Whole: true, Now: t0.Add(3 * time.Minute), OwnerActive: owner})))
	assert.Equal(t, []string{"ask A-1"}, kinds(Make(Inputs{Plugin: pl, State: started(t0), Things: []Thing{a1}, Whole: true, Now: t0.Add(3 * time.Minute),
		OwnerActive: func() time.Time { return time.Time{} }})))
}

// A question on a role's screen is told only once it has stood dialog_after (a beat's jitter forgiven), and not at all if it
// went before; one that comes back stands anew.
func TestPlanDialogStoodAWhile(t *testing.T) {
	pl := plug()
	st := started(t0)
	d := dialog("dev", "0123456789abcdef")
	in := Inputs{Plugin: pl, State: st, Things: []Thing{d}, Whole: true}

	in.Now = t0
	st.Observe(in.Things, in.Now, true)
	assert.Empty(t, Make(in).Send, "just seen")

	in.Now = t0.Add(30 * time.Second)
	st.Observe(in.Things, in.Now, true)
	assert.Empty(t, Make(in).Send)

	in.Now = t0.Add(54 * time.Second)
	assert.Empty(t, Make(in).Send, "not yet the 60s less the jitter")

	in.Now = t0.Add(60 * time.Second)
	st.Observe(in.Things, in.Now, true)
	plan := Make(in)
	assert.Equal(t, []string{"dialog dev:0123456789abcdef"}, kinds(plan))
	st.Succeed(plan.Send[0], in.Now)

	// Gone before it was told: forgotten, so a second one stands anew.
	st2 := started(t0)
	st2.Observe([]Thing{d}, t0, true)
	st2.Observe(nil, t0.Add(20*time.Second), true)
	assert.Empty(t, st2.Seen)
	st2.Observe([]Thing{d}, t0.Add(70*time.Second), true)
	assert.Empty(t, Make(Inputs{Plugin: pl, State: st2, Things: []Thing{d}, Whole: true, Now: t0.Add(70 * time.Second)}).Send)

	// A look that may have missed something forgets nothing.
	st3 := started(t0)
	st3.Observe([]Thing{d}, t0, true)
	st3.Observe(nil, t0.Add(20*time.Second), false)
	assert.Contains(t, st3.Seen, d.Key)

	// dialog_after is the plugin's own; a plugin that does not take dialogs is never told.
	quick := plug()
	quick.DialogAfter = "0s"
	st4 := started(t0)
	st4.Observe([]Thing{d}, t0, true)
	assert.Equal(t, []string{"dialog dev:0123456789abcdef"}, kinds(Make(Inputs{Plugin: quick, State: st4, Things: []Thing{d}, Whole: true, Now: t0})))
	assert.Empty(t, Make(Inputs{Plugin: plug("ask"), State: st4, Things: []Thing{d}, Whole: true, Now: t0.Add(time.Hour)}).Send)
}

// The same question on a role's screen again, after it was told and closed, is told again once it has stood; one the plugin
// was never told of (every try failed) does not stand in its way.
func TestPlanADialogThatComesBack(t *testing.T) {
	pl := plug()
	d := dialog("dev", "0123456789abcdef")
	st := started(t0)

	st.Observe([]Thing{d}, t0, true)
	plan := Make(Inputs{Plugin: pl, State: st, Things: []Thing{d}, Whole: true, Now: t0.Add(time.Minute)})
	require.Equal(t, []string{"dialog dev:0123456789abcdef"}, kinds(plan))
	st.Succeed(plan.Send[0], t0.Add(time.Minute))

	// It leaves the screen: the plugin hears so.
	st.Observe(nil, t0.Add(2*time.Minute), true)
	plan = Make(Inputs{Plugin: pl, State: st, Things: nil, Whole: true, Now: t0.Add(2 * time.Minute)})
	require.Equal(t, []string{"answered dev:0123456789abcdef"}, kinds(plan))
	st.Succeed(plan.Send[0], t0.Add(2*time.Minute))

	// It comes back: standing anew, it is not told at the first sight, and is told once it has stood.
	back := t0.Add(10 * time.Minute)
	st.Observe([]Thing{d}, back, true)
	assert.Empty(t, Make(Inputs{Plugin: pl, State: st, Things: []Thing{d}, Whole: true, Now: back}).Send)

	back = back.Add(time.Minute)
	st.Observe([]Thing{d}, back, true)
	assert.Equal(t, []string{"dialog dev:0123456789abcdef"}, kinds(Make(Inputs{Plugin: pl, State: st, Things: []Thing{d}, Whole: true, Now: back})))

	// Dropped, gone, back: not held against.
	dropped := started(t0)
	dropped.Told = map[string]*Entry{}
	dropped.Told[d.Key] = &Entry{ID: d.ID, Kind: Dialog, Dropped: true, Told: t0}
	dropped.Observe(nil, t0.Add(time.Minute), true)
	assert.NotContains(t, dropped.Told, d.Key)
}

// answered goes only to a plugin that was told of the thing, once, and only when the plugin takes it; a plugin that does not
// take it has the entry closed quietly; a thing the plugin gave up on is never closed to it.
func TestPlanAnsweredRouting(t *testing.T) {
	a1, a2 := item("A-1", t0.Add(time.Minute), false), item("A-2", t0.Add(time.Minute), false)
	now := t0.Add(5 * time.Minute)

	st := started(t0)
	st.Succeed(Due{Key: a1.Key, Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask}}, now)
	// A-2 was never told (held, or the plugin gave up), and it is closed.
	st.Told[a2.Key] = &Entry{ID: "A-2", Kind: Ask, Dropped: true, Told: t0}

	plan := Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: true, Now: now})
	assert.Equal(t, []string{"answered A-1"}, kinds(plan), "only what the plugin was told of")
	assert.Empty(t, plan.Settle)

	st.Succeed(plan.Send[0], now)
	assert.Empty(t, Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: true, Now: now.Add(time.Hour)}).Send, "once")
	assert.False(t, st.Told[a1.Key].Closed.IsZero())

	// A plugin that does not take answered: closed quietly.
	quiet := started(t0)
	quiet.Succeed(Due{Key: a1.Key, Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask}}, now)
	plan = Make(Inputs{Plugin: plug("ask"), State: quiet, Things: nil, Whole: true, Now: now})
	assert.Empty(t, plan.Send)
	assert.Equal(t, []string{a1.Key}, plan.Settle)

	// Still open: nothing. A look that may have missed something judges nothing ended.
	open := started(t0)
	open.Succeed(Due{Key: a1.Key, Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask}}, now)
	assert.Empty(t, Make(Inputs{Plugin: plug(), State: open, Things: []Thing{a1}, Whole: true, Now: now}).Send)
	assert.Empty(t, Make(Inputs{Plugin: plug(), State: open, Things: nil, Whole: false, Now: now}).Send)
}

// A task that waits on the owner a second time has the same id: the first is closed before the second is told. A task whose
// record changes while it waits is the same thing, and is not told again.
func TestPlanAWaitingTaskAskedAgainIsNew(t *testing.T) {
	st := started(t0)
	first := item("T-5", t0.Add(time.Minute), true)
	now := t0.Add(2 * time.Minute)

	plan := Make(Inputs{Plugin: plug(), State: st, Things: []Thing{first}, Whole: true, Now: now})
	st.Succeed(plan.Send[0], now)

	changed := item("T-5", t0.Add(3*time.Minute), true) // the task was updated while it waited
	assert.Empty(t, Make(Inputs{Plugin: plug(), State: st, Things: []Thing{changed}, Whole: true, Now: t0.Add(4 * time.Minute)}).Send)

	// Answered: it leaves the todo, and the plugin hears so.
	plan = Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: true, Now: t0.Add(5 * time.Minute)})
	require.Equal(t, []string{"answered T-5"}, kinds(plan))
	st.Succeed(plan.Send[0], t0.Add(5*time.Minute))

	// It waits again, with the same id: a new question.
	second := item("T-5", t0.Add(time.Hour), true)
	plan = Make(Inputs{Plugin: plug(), State: st, Things: []Thing{second}, Whole: true, Now: t0.Add(61 * time.Minute)})
	assert.Equal(t, []string{"blocking T-5"}, kinds(plan))

	// Closing and asking again in one beat (the closing waited for a retry): the closing goes first, the new one after.
	st = started(t0)
	st.Succeed(Due{Key: first.Key, Kind: Blocking, Entry: Entry{ID: "T-5", Kind: Blocking}}, now)
	st.Told[first.Key].Closed = now
	plan = Make(Inputs{Plugin: plug(), State: st, Things: []Thing{second}, Whole: true, Now: t0.Add(61 * time.Minute)})
	assert.Equal(t, []string{"blocking T-5"}, kinds(plan), "a closed entry does not stand in the way of the next")
}

// A failed event is tried again after 1, 5 and 15 minutes, as it was built, then dropped for good; the plugin is never
// told of a dropped thing, nor of its end.
func TestPlanRetryBackoffAndDrop(t *testing.T) {
	st := started(t0)
	a1 := item("A-1", t0.Add(time.Minute), false)
	pl := plug()
	now := t0.Add(2 * time.Minute)
	in := func(at time.Time, things ...Thing) Inputs {
		return Inputs{Plugin: pl, State: st, Things: things, Whole: true, Now: at}
	}

	plan := Make(in(now, a1))
	require.Len(t, plan.Send, 1)
	assert.Nil(t, plan.Send[0].Retry)

	line := `{"v":1,"event":"ask"}` + "\n"
	assert.False(t, st.Fail(plan.Send[0], line, now, "exit 1"))
	assert.Equal(t, time.Minute, st.Queue[0].Next.Sub(now))
	assert.Equal(t, "exit 1", st.Failed.Err)

	assert.Empty(t, Make(in(now.Add(59*time.Second), a1)).Send, "waiting out the first minute; the item is not sent again as new")

	at := now.Add(time.Minute)
	plan = Make(in(at, a1))
	require.Len(t, plan.Send, 1)
	require.NotNil(t, plan.Send[0].Retry)
	assert.Equal(t, line, plan.Send[0].Retry.Line, "as it was built")
	assert.False(t, st.Fail(plan.Send[0], line, at, "timed out"))
	assert.Equal(t, 5*time.Minute, st.Queue[0].Next.Sub(at))

	at = at.Add(5 * time.Minute)
	plan = Make(in(at, a1))
	require.Len(t, plan.Send, 1)
	assert.False(t, st.Fail(plan.Send[0], line, at, "timed out"))
	assert.Equal(t, 15*time.Minute, st.Queue[0].Next.Sub(at))
	assert.Equal(t, 3, st.Queue[0].Tries)

	at = at.Add(15 * time.Minute)
	plan = Make(in(at, a1))
	require.Len(t, plan.Send, 1)
	assert.True(t, st.Fail(plan.Send[0], line, at, "exit 1"), "the fourth failure drops it")
	assert.Empty(t, st.Queue)
	assert.True(t, st.Told[a1.Key].Dropped)

	assert.Empty(t, Make(in(at.Add(time.Hour), a1)).Send, "never sent again")
	assert.Empty(t, Make(in(at.Add(time.Hour))).Send, "and never told of its end")
	assert.Equal(t, 0, st.Open())

	// Success after a failure takes it off the queue and tells it.
	st = started(t0)
	plan = Make(in(now, a1))
	st.Fail(plan.Send[0], line, now, "exit 1")
	plan = Make(in(now.Add(time.Minute), a1))
	st.Succeed(plan.Send[0], now.Add(time.Minute))
	assert.Empty(t, st.Queue)
	assert.Equal(t, 1, st.Open())
	assert.True(t, st.Last.OK)
}

// An event waiting for a retry whose item was closed meanwhile is never sent: the plugin was not told, and has nothing to
// close. An answered that waits is sent whatever.
func TestPlanRetryOfWhatEndedIsCancelled(t *testing.T) {
	st := started(t0)
	a1 := item("A-1", t0.Add(time.Minute), false)
	now := t0.Add(2 * time.Minute)

	plan := Make(Inputs{Plugin: plug(), State: st, Things: []Thing{a1}, Whole: true, Now: now})
	st.Fail(plan.Send[0], "line\n", now, "exit 1")

	later := now.Add(2 * time.Minute)
	plan = Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: true, Now: later})
	assert.Empty(t, plan.Send)
	assert.Equal(t, []string{a1.Key}, plan.Cancel)
	st.Cancel(plan.Cancel[0])
	assert.Empty(t, st.Queue)
	assert.Empty(t, st.Told)

	// A look that may have missed it cancels nothing.
	st = started(t0)
	st.Fail(Due{Key: a1.Key, Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask}}, "line\n", now, "exit 1")
	plan = Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: false, Now: later})
	assert.Len(t, plan.Send, 1, "the retry goes: nothing says it ended")

	// A closing that failed waits and goes again, though the thing is long gone.
	st = started(t0)
	st.Succeed(Due{Key: a1.Key, Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask}}, now)
	plan = Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: true, Now: later})
	require.Equal(t, []string{"answered A-1"}, kinds(plan))
	st.Fail(plan.Send[0], "closing\n", later, "exit 1")
	assert.Empty(t, Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: true, Now: later.Add(30 * time.Second)}).Send, "waits, and is not planned twice")

	plan = Make(Inputs{Plugin: plug(), State: st, Things: nil, Whole: true, Now: later.Add(time.Minute)})
	require.Len(t, plan.Send, 1)
	assert.Equal(t, "answered", plan.Send[0].Kind)
	assert.Equal(t, "closing\n", plan.Send[0].Retry.Line)
	st.Succeed(plan.Send[0], later.Add(time.Minute))
	assert.Empty(t, st.Queue)
	assert.False(t, st.Told[a1.Key].Closed.IsZero())

	// An answered that fails to the end is given up, and closed.
	st = started(t0)
	st.Succeed(Due{Key: a1.Key, Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask}}, now)
	d := Due{Key: a1.Key, Kind: Answered, Entry: *st.Told[a1.Key]}

	for range MaxTries - 1 {
		assert.False(t, st.Fail(d, "closing\n", later, "exit 1"))
	}

	assert.True(t, st.Fail(d, "closing\n", later, "exit 1"))
	assert.False(t, st.Told[a1.Key].Closed.IsZero())
	assert.Empty(t, st.Queue)
}

// At most ten events a beat go to a plugin; the rest wait for the next.
func TestPlanAtMostTenAndTheRestNextBeat(t *testing.T) {
	st := started(t0)

	var things []Thing

	for i := range 14 {
		things = append(things, item("A-"+string(rune('a'+i)), t0.Add(time.Duration(i+1)*time.Second), false))
	}

	now := t0.Add(time.Minute)
	plan := Make(Inputs{Plugin: plug(), State: st, Things: things, Whole: true, Now: now})
	require.Len(t, plan.Send, MaxPerBeat)

	for _, d := range plan.Send {
		st.Succeed(d, now)
	}

	plan = Make(Inputs{Plugin: plug(), State: st, Things: things, Whole: true, Now: now.Add(time.Minute)})
	assert.Len(t, plan.Send, 4)
}

// The told file round trips, a file that does not read is a plugin told of nothing, and old entries are pruned after a week.
func TestLedgerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins", "pager.told.json")

	st, err := Load(path)
	require.NoError(t, err, "no file: a plugin told of nothing")
	assert.Empty(t, st.Told)

	st.Observe([]Thing{dialog("dev", "ab")}, t0, true)
	st.Succeed(Due{Key: "A-1@x", Kind: Ask, Entry: Entry{ID: "A-1", Kind: Ask, Opened: t0}}, t0)
	st.Succeed(Due{Key: "A-2@x", Kind: Ask, Entry: Entry{ID: "A-2", Kind: Ask}}, t0)
	st.Told["A-2@x"].Closed = t0
	st.Fail(Due{Key: "A-3@x", Kind: Ask, Entry: Entry{ID: "A-3", Kind: Ask}}, "line\n", t0, "exit 1")
	require.NoError(t, st.Save(path))

	back, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, st.Told["A-1@x"].ID, back.Told["A-1@x"].ID)
	assert.Len(t, back.Queue, 1)
	assert.Equal(t, "exit 1", back.Failed.Err)
	assert.Contains(t, back.Seen, DialogKey("dev:ab"))

	back.Prune(t0.Add(Keep - time.Hour))
	assert.Contains(t, back.Told, "A-2@x")
	back.Prune(t0.Add(Keep + time.Hour))
	assert.NotContains(t, back.Told, "A-2@x", "closed for a week: forgotten")
	assert.Contains(t, back.Told, "A-1@x", "an open one stays")
	assert.NotContains(t, back.Seen, DialogKey("dev:ab"))

	require.NoError(t, osWrite(path, "{not json"))

	bad, err := Load(path)
	require.Error(t, err)
	assert.Empty(t, bad.Told)
	assert.Equal(t, Version, bad.V)
}

func TestBackoff(t *testing.T) {
	assert.Equal(t, []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute},
		[]time.Duration{Backoff(1), Backoff(2), Backoff(3), Backoff(9)})
	assert.Equal(t, time.Minute, Backoff(0))
}

func TestOwnerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins", "owner.json")
	assert.True(t, ReadOwner(path).Active.IsZero(), "no file: no mark")

	require.NoError(t, UpdateOwner(path, t0, func(o *Owner) {
		o.Active = t0
		o.Chosen = map[string]Chosen{"dev:ab": {By: "owner", N: 2, Text: "No", At: t0}}
	}))

	o := ReadOwner(path)
	assert.True(t, o.Active.Equal(t0))
	assert.Equal(t, 2, o.Chosen["dev:ab"].N)

	require.NoError(t, UpdateOwner(path, t0.Add(3*24*time.Hour), func(o *Owner) { o.Active = t0.Add(3 * 24 * time.Hour) }))
	assert.Empty(t, ReadOwner(path).Chosen, "a choice is remembered for two days")

	require.NoError(t, osWrite(path, "nope"))
	assert.True(t, ReadOwner(path).Active.IsZero())
}
