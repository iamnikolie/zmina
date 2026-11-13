package cmd

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/plugin"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// The plugins' delivery. A beat that finds the crew has plugins starts `zmina plugin deliver` in a process of its own and does
// not wait for it (as the daily lessons run is started): a plugin may take its whole deadline, and the beat must never wait on
// one. That process reads what is open — the todo, the questions on the roles' screens — under no state lock, and for each
// plugin whose delivery is not already going (a lock of its own, so a run that outlasts a beat is not met by the next) plans
// what it is owed (plugin.Make), runs the events one at a time, each in a process group under its deadline, and keeps what it
// did in the plugin's told file. Plugins are served in parallel, each by its own goroutine.

// pluginsBeat starts the delivery after the beat. A crew with no plugin starts nothing.
func (a *app) pluginsBeat(now time.Time) {
	c, err := a.loadCrew()
	if err != nil || len(c.Plugins) == 0 {
		return
	}

	spawn := a.spawn
	if spawn == nil {
		spawn = spawnDetached
	}

	_ = spawn([]string{"--crew", c.Path, "plugin", "deliver"})
}

// pluginWorld is what is open now, read once for every plugin of a beat.
type pluginWorld struct {
	crew *crew.Crew
	// things are what a plugin may be told of; whole says they are all of it.
	things  []plugin.Thing
	whole   bool
	items   map[string]todoItem
	dialogs map[string]plugin.DialogFacts
	// ownerActive is when the owner was last at the machine, read the first time it is asked.
	ownerActive func() time.Time
}

// pluginWorld reads what is open: each item on the owner's todo, and each question on a role's screen that its choose
// command can answer (the one the screen shows, as chooseDialog reads it).
func (a *app) pluginWorld(c *crew.Crew, now time.Time) (*pluginWorld, error) {
	w := &pluginWorld{crew: c, items: map[string]todoItem{}, dialogs: map[string]plugin.DialogFacts{}}

	err := a.readState(func(s *shift.Shift) error {
		views := s.Views()
		items, problems := todo(c, now, views)
		w.whole = len(problems) == 0

		for _, it := range items {
			key := plugin.ItemKey(it.ID)
			w.items[key] = it
			w.things = append(w.things, plugin.Thing{Key: key, ID: it.ID, Class: plugin.ItemThing, Opened: it.Created, Role: it.Role,
				Blocking: it.Blocks != "", Reminder: it.Kind == "later" || it.byAlarm})
		}

		for _, v := range views {
			// A role tmux could not be asked about may have a question up: nothing is judged ended by its absence.
			if v.State == shift.PresenceUnknown {
				w.whole = false

				continue
			}

			facts, ok, known := standingDialog(s, c, v)
			if !known {
				w.whole = false
			}

			if !ok {
				continue
			}

			key := plugin.DialogKey(plugin.DialogID(facts.Role, facts.ID))
			w.dialogs[key] = facts
			w.things = append(w.things, plugin.Thing{Key: key, ID: plugin.DialogID(facts.Role, facts.ID), Class: plugin.DialogThing,
				Role: facts.Role})
		}

		owner := c.Layout().PluginOwner()
		w.ownerActive = sync.OnceValue(func() time.Time {
			at := plugin.ReadOwner(owner).Active

			for _, v := range views {
				if v.State == shift.PresenceUp {
					if t := s.OwnerLine(v.Role); t.After(at) {
						at = t
					}
				}
			}

			return at
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return w, nil
}

// standingDialog is the question a role's screen shows that a plugin can be told of: the dialog its choose command would
// press into (the one on the screen, with the fingerprint it names), while the session says it waits. known is false when the
// screen could not be read.
func standingDialog(s *shift.Shift, c *crew.Crew, v shift.View) (plugin.DialogFacts, bool, bool) {
	if v.State != shift.PresenceUp || !v.Prompt || v.Dialog == nil || v.Status != "waiting" {
		return plugin.DialogFacts{}, false, true
	}

	screen, err := s.Tmux.Capture(v.Role, 80)
	if err != nil {
		return plugin.DialogFacts{}, false, false
	}

	d := s.ParseDialog(screen)
	if d == nil || d.ID != v.Dialog.ID {
		// Only the file's words, or a screen that moved on between the two looks: nothing a choose could answer.
		return plugin.DialogFacts{}, false, true
	}

	f := plugin.DialogFacts{Role: v.Role, ID: d.ID, Question: cmp.Or(v.Dialog.Question, d.Question)}

	for _, o := range d.Options {
		f.Options = append(f.Options, plugin.Option{N: o.N, Text: o.Label, Detail: o.Detail})
	}

	if len(v.Tasks) > 0 {
		f.Task = v.Tasks[0]
		if t, err := taskStore(c).Get(f.Task); err == nil {
			f.TaskTitle = t.Headline()
		}
	}

	return f, true, true
}

// itemFacts is what an event of a todo item says of it.
func (w *pluginWorld) itemFacts(it todoItem) plugin.Item {
	c := w.crew

	ref := it.Task
	if t, err := taskStore(c).Get(it.Task); it.Task != "" && err == nil && t.Headline() != "" {
		ref += " " + t.Headline()
	}

	why := ""

	switch {
	case it.Blocks != "" && ref != "":
		why = "holds up " + it.Blocks + " on " + ref
	case it.Blocks != "":
		why = "holds up " + it.Blocks
	case ref != "":
		why = "about " + ref
	}

	text := it.Text
	if it.Note != "" {
		text += "\n\n" + it.Note
	}

	return plugin.Item{ID: it.ID, Role: it.Role, Title: cmp.Or(it.Title, clip(oneLine(it.Text), 80)), Text: text, Why: why,
		Reason: it.Reason, Rec: it.Rec, Options: it.Options, Opened: it.Created, Blocking: it.Blocks != "",
		Check: it.Kind == "check", Later: it.Kind == "later"}
}

// closureOf is how a thing a plugin was told of ended, read from where it lives: the ask, the task, the parked item, or
// the owner's mark of what he chose of a dialog. Anything it cannot place is gone.
func closureOf(c *crew.Crew, e plugin.Entry, at time.Time) plugin.Closure {
	cl := plugin.Closure{ID: e.ID, For: e.Kind, How: plugin.HowGone, At: at}

	if e.Kind == plugin.Dialog {
		if ch, ok := plugin.ReadOwner(c.Layout().PluginOwner()).Chosen[e.ID]; ok && ch.At.After(e.Told) {
			cl.How, cl.By, cl.Text, cl.At = plugin.HowChosen, ch.By, ch.Text, ch.At
		}

		return cl
	}

	switch {
	case strings.HasPrefix(e.ID, "A-"):
		ask, err := askStore(c).Get(e.ID)
		if err != nil {
			return cl
		}

		cl.By, cl.At = ask.ClosedBy, ask.Updated

		switch ask.State {
		case asks.Answered:
			cl.How, cl.Text = plugin.HowAnswered, ask.Answer
		case asks.Yes:
			cl.How, cl.Text = plugin.HowAnswered, ask.Rec
		case asks.Done:
			cl.How, cl.Text = plugin.HowDone, ask.Answer
		case asks.Parked, asks.Ignored:
			cl.How = plugin.HowDropped
		default:
			cl.By = ""
		}
	case strings.HasPrefix(e.ID, "T-"):
		t, err := taskStore(c).Get(e.ID)

		switch {
		case err != nil:
		case t.Ignored.After(t.Updated):
			cl.How, cl.By, cl.At = plugin.HowDropped, t.AnsweredBy, t.Ignored
		case t.Answered.After(t.Updated):
			cl.How, cl.By, cl.At = plugin.HowAnswered, t.AnsweredBy, t.Answered
		}
	case strings.HasPrefix(e.ID, "L-"):
		switch it, err := laterStore(c).Get(e.ID); {
		case err != nil:
		case it.State == later.Done || it.State == later.Taken:
			cl.How = plugin.HowDone
		case it.State == later.Dropped:
			cl.How = plugin.HowDropped
		}
	}

	return cl
}

// lockPlugin takes a plugin's delivery lock without waiting; false when another run holds it. The release is the caller's.
func lockPlugin(c *crew.Crew, name string) (release func(), ok bool) {
	if fsx.MkdirAll(c.Layout().Plugins(), 0o700) != nil {
		return nil, false
	}

	f, err := os.OpenFile(c.Layout().PluginLock(name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}

	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()

		return nil, false
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

// pluginsDeliver is the delivery itself: every plugin of the crew, in parallel, each one's events one at a time. It returns
// the lines for the output; what could not be done is a line, never an error that stops the others.
func (a *app) pluginsDeliver(now time.Time) error {
	c, err := a.loadCrew()
	if err != nil || len(c.Plugins) == 0 {
		return err
	}

	type job struct {
		pl      crew.Plugin
		release func()
	}

	var jobs []job

	for _, pl := range c.Plugins {
		if release, ok := lockPlugin(c, pl.Name); ok {
			jobs = append(jobs, job{pl, release})
		}
	}

	if len(jobs) == 0 {
		return nil
	}

	w, err := a.pluginWorld(c, now)
	if err != nil {
		for _, j := range jobs {
			j.release()
		}

		return err
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		lines []string
	)

	for _, j := range jobs {
		wg.Add(1)

		go func() {
			defer wg.Done()
			defer j.release()

			got := a.pluginDeliverOne(c, j.pl, w, now)

			mu.Lock()
			lines = append(lines, got...)
			mu.Unlock()
		}()
	}

	wg.Wait()
	a.printTick(lines)

	return nil
}

// pluginDeliverOne serves one plugin: it notes what is open, plans (plugin.Make), and runs the events one at a time until
// one fails — a plugin that fails is not asked again this beat — keeping the plugin's told file after each. now is the
// beat's moment, for what is due; what happened is dated by the clock.
func (a *app) pluginDeliverOne(c *crew.Crew, pl crew.Plugin, w *pluginWorld, now time.Time) []string {
	path := c.Layout().PluginTold(pl.Name)

	// What happened is dated by the beat's clock carried on by the time the delivery has taken: the beat's moment, and then the
	// real time, which is the same clock in a beat, and a moment a test chooses.
	began := time.Now()
	clock := func() time.Time { return now.Add(time.Since(began)) }

	var lines []string

	say := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf("plugin %s: ", pl.Name)+fmt.Sprintf(format, args...))
	}

	st, lerr := plugin.Load(path)
	if lerr != nil {
		say("%v", lerr)
	}

	st.Observe(w.things, now, w.whole)

	plan := plugin.Make(plugin.Inputs{Plugin: pl, State: st, Things: w.things, Whole: w.whole, Now: now, OwnerActive: w.ownerActive})

	for _, key := range plan.Cancel {
		st.Cancel(key)
	}

	for _, key := range plan.Settle {
		if e := st.Told[key]; e != nil {
			e.Closed = now
		}
	}

	save := func() {
		st.Prune(clock())

		if err := st.Save(path); err != nil {
			say("not saved: %v", err)
		}
	}

	prog, perr := plugin.Resolve(pl, c.Layout().Dir)
	b := plugin.Builder{Crew: c.Path, Plugin: pl.Name, Clean: shift.QuoteN}

	for _, d := range plan.Send {
		line, ok := w.line(b, d, now)
		if !ok {
			continue
		}

		var res plugin.Result

		if perr != nil {
			res = plugin.Result{Code: -1, Err: perr}
		} else {
			res = plugin.Exec(plugin.Spec{Program: prog, Dir: c.Dir, Stdin: line, Timeout: pl.TimeoutD(),
				Env: plugin.Env(os.Environ(), pl.Name, d.Kind, c.Path)})
		}

		at := clock()

		if res.OK() {
			st.Succeed(d, at)
			pluginLog(c, "plugin-told", pl.Name, d, "", res)
			say("%s %s told", d.Kind, d.Entry.ID)
			save()

			continue
		}

		reason := res.Reason(pl.TimeoutD())

		if st.Fail(d, string(line), at, reason) {
			pluginLog(c, "plugin-failed", pl.Name, d, fmt.Sprintf("given up after %d tries: %s", plugin.MaxTries, reason), res)
			say("%s %s given up after %d tries: %s", d.Kind, d.Entry.ID, plugin.MaxTries, reason)
		} else {
			say("%s %s failed, tried again later: %s", d.Kind, d.Entry.ID, reason)
		}

		break
	}

	save()

	return lines
}

// line is the event of a due send as it goes on the plugin's stdin: the one a failed try kept, or one built now from what is
// open (an answered from where the thing lives).
func (w *pluginWorld) line(b plugin.Builder, d plugin.Due, now time.Time) ([]byte, bool) {
	switch {
	case d.Retry != nil:
		return []byte(d.Retry.Line), true
	case d.Kind == plugin.Answered:
		return b.Answered(closureOf(w.crew, d.Entry, now)).Line(), true
	case d.Kind == plugin.Dialog:
		f, ok := w.dialogs[d.Key]

		return b.Dialog(f).Line(), ok
	}

	it, ok := w.items[d.Key]
	if !ok {
		return nil, false
	}

	return b.Item(d.Kind, w.itemFacts(it)).Line(), true
}

// pluginLog puts what happened to an event in the event log: a delivery (plugin-told) or the giving up (plugin-failed), with
// what the plugin printed, each stream cut to plugin.OutputMax and cleaned as any quotation: it is data.
func pluginLog(c *crew.Crew, event, name string, d plugin.Due, note string, res plugin.Result) {
	detail := fmt.Sprintf("%s %s %s", name, d.Kind, d.Entry.ID)

	if note != "" {
		detail += ": " + note
	}

	for _, out := range []struct{ label, text string }{{"stdout", res.Stdout}, {"stderr", res.Stderr}} {
		if strings.TrimSpace(out.text) != "" {
			detail += fmt.Sprintf(" | %s: %s", out.label, shift.QuoteN(out.text, plugin.OutputMax))
		}
	}

	_ = eventLog(c).Add(events.Event{TS: time.Now(), Event: event, By: "ticker", Task: taskRefOf(d.Entry.ID), Detail: detail})
}

// taskRefOf is the id when it is a task's.
func taskRefOf(id string) string {
	if task.Normalize(id) == id && strings.HasPrefix(id, "T-") {
		return id
	}

	return ""
}

// touchOwner notes that the owner answered in a terminal just now, for the crews that have plugins to hold a push by.
func touchOwner(s *shift.Shift) {
	if len(s.Crew.Plugins) == 0 {
		return
	}

	now := s.Now()
	_ = plugin.UpdateOwner(s.Crew.Layout().PluginOwner(), now, func(o *plugin.Owner) { o.Active = now })
}
