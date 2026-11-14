package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
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
	// Read as the other beats' chores read it (diskBeat): a beat that finds no plugins says nothing and notes nothing, every minute.
	c, err := crew.Load(a.crewPath())
	if err != nil || len(c.Plugins) == 0 {
		return
	}

	spawn := a.spawn
	if spawn == nil {
		spawn = spawnDetached
	}

	_ = spawn([]string{"--crew", c.Path, "plugin", "deliver", "--beat"})
}

// pluginWorld is what is open now, read once for every plugin of a beat.
type pluginWorld struct {
	crew *crew.Crew
	// things are what a plugin may be told of; gaps is what the look could not read (it judges nothing of those).
	things  []plugin.Thing
	gaps    plugin.Gaps
	items   map[string]todoItem
	dialogs map[string]plugin.DialogFacts
	// ownerActive is when the owner was last at the machine, read the first time it is asked.
	ownerActive func() time.Time
	// dialogStill reads a role's screen again and says whether the question the facts are of is still on it, in the same wait.
	dialogStill func(f plugin.DialogFacts) bool
}

// pluginWorld reads what is open: each item on the owner's todo, each question on a role's screen that its choose
// command can answer (the one the screen shows, as chooseDialog reads it), and each question a role's last turn ended on in plain
// words (standingTurn).
func (a *app) pluginWorld(c *crew.Crew, now time.Time) (*pluginWorld, error) {
	w := &pluginWorld{crew: c, items: map[string]todoItem{}, dialogs: map[string]plugin.DialogFacts{}}

	err := a.readState(func(s *shift.Shift) error {
		views := s.Views()
		items, _ := todo(c, now, views)
		w.gaps = readGaps(c)

		for _, it := range items {
			key := plugin.ItemKey(it.ID)
			w.items[key] = it
			w.things = append(w.things, plugin.Thing{Key: key, ID: it.ID, Class: plugin.ItemThing, Opened: it.Created, Role: it.Role,
				Blocking: it.Blocks != "", Reminder: it.Kind == "later" || it.byAlarm})
		}

		for _, v := range views {
			// A role tmux could not be asked about may have a question up: nothing is judged ended by its absence.
			if v.State == shift.PresenceUnknown {
				w.gaps.Roles[v.Role] = true

				continue
			}

			facts, ok, known := standingDialog(s, c, v)
			if !known {
				w.gaps.Roles[v.Role] = true
			}

			// No dialog on its screen: a role idle since a turn that ended on a question to the owner is awaited all the same (a
			// standing dialog wins, and a role whose screen could not be read is judged by neither).
			if !ok && known {
				var turnKnown bool

				facts, ok, turnKnown = standingTurn(c, v)
				if !turnKnown {
					w.gaps.Roles[v.Role] = true
				}
			}

			if !ok {
				continue
			}

			key := plugin.DialogKey(plugin.DialogID(facts.Role, facts.ID))
			w.dialogs[key] = facts
			w.things = append(w.things, plugin.Thing{Key: key, ID: plugin.DialogID(facts.Role, facts.ID), Class: plugin.DialogThing,
				Role: facts.Role, Opened: facts.Since})
		}

		// Read again right before a question is told, as an item is: one answered, or asked again, since the look is not paged.
		w.dialogStill = func(f plugin.DialogFacts) bool {
			v, _, ok := s.Look(f.Role)
			if !ok {
				return false
			}

			// A turn's question is on no screen: the record is read again, and the role must be idle since that very turn.
			if f.Prompt == plugin.PromptTurn {
				now, standing, known := standingTurn(c, v)

				return known && standing && now.ID == f.ID && now.Since.Equal(f.Since)
			}

			now, standing, known := standingDialog(s, c, v)

			return known && standing && now.ID == f.ID && v.Since.Equal(f.Since)
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

// readGaps is what the files of the todo's sources did not read: an ask or a task whose file does not read is named by its id,
// the parked list as a whole when one of its files does not. What a gap hides is not judged ended, and is not forgotten.
func readGaps(c *crew.Crew) plugin.Gaps {
	g := plugin.Gaps{Items: map[string]bool{}, Roles: map[string]bool{}}

	if bad, _ := askStore(c).Unreadable(); len(bad) > 0 {
		for name := range bad {
			g.Items[strings.TrimSuffix(name, ".json")] = true
		}
	}

	if bad, _ := taskStore(c).Unreadable(); len(bad) > 0 {
		for name := range bad {
			g.Items[strings.TrimSuffix(name, ".json")] = true
		}
	}

	if _, problems, _ := laterStore(c).Scan(); len(problems) > 0 {
		g.Items[plugin.LaterAll] = true
	}

	return g
}

// isOpen reports whether the thing of a ledger key is open in this look.
func (w *pluginWorld) isOpen(key string) bool {
	if _, ok := w.items[key]; ok {
		return true
	}

	_, ok := w.dialogs[key]

	return ok
}

// itemOpen reads an item again, right before it is told: an ask answered since the look, a task that has left the todo, a parked
// item put back, is not paged. A file that does not read is not a reason to hold it back.
func itemOpen(c *crew.Crew, id string) bool {
	switch {
	case strings.HasPrefix(id, "A-"):
		a, err := askStore(c).Get(id)

		return err != nil || a.Open()
	case strings.HasPrefix(id, "T-"):
		t, err := taskStore(c).Get(id)

		return err != nil || t.State == task.Waiting && !c.OnOthers(t.On) && !t.Answered.After(t.Updated) && !t.Ignored.After(t.Updated)
	case strings.HasPrefix(id, "L-"):
		it, err := laterStore(c).Get(id)

		return err != nil || it.State == later.Parked && !it.Back.IsZero()
	}

	return true
}

// standingDialog is the question a role's screen shows that a plugin can be told of: the dialog its choose command would press
// into (the one on the screen, with the fingerprint it names, read again here), while the session says it waits. A permission
// prompt is told only with the call its hook recorded: a plugin that is shown "Do you want to proceed?" and no command would be
// asked to approve blind. A prompt the session does not say what it waits on is not told either. known is false when the screen
// could not be read.
func standingDialog(s *shift.Shift, c *crew.Crew, v shift.View) (plugin.DialogFacts, bool, bool) {
	// A wait with no moment it began is no wait a one-shot mark can be of: nothing is told of it. (A session file with no time says 0,
	// which is 1970, not the zero time.)
	if v.State != shift.PresenceUp || !v.Prompt || v.Dialog == nil || v.Status != "waiting" || v.Since.UnixMilli() <= 0 {
		return plugin.DialogFacts{}, false, true
	}

	prompt := ""

	switch v.WaitingFor {
	case shift.WaitPermission:
		if v.Dialog.Subject == "" {
			return plugin.DialogFacts{}, false, true
		}

		prompt = "permission"
	case shift.WaitQuestion:
		// Only the question its hook recorded for this wait and the screen shows: a permission prompt can follow an answered question
		// under the same words, the session file not written again.
		if !v.Dialog.Asked {
			return plugin.DialogFacts{}, false, true
		}

		prompt = "question"
	default:
		return plugin.DialogFacts{}, false, true
	}

	screen, err := s.Tmux.Capture(v.Role, 80)
	if err != nil {
		return plugin.DialogFacts{}, false, false
	}

	d := s.DialogOn(v.Role, screen)
	if d == nil || d.ID != v.Dialog.ID {
		// Only the file's words, or a screen that moved on between the two looks: nothing a choose could answer.
		return plugin.DialogFacts{}, false, true
	}

	if prompt == "question" && s.PermissionHinted(v.Role, v.Since, screen) {
		return plugin.DialogFacts{}, false, true
	}

	f := plugin.DialogFacts{Role: v.Role, ID: d.ID, Question: cmp.Or(v.Dialog.Question, d.Question), Prompt: prompt, Tool: d.Tool,
		Subject: d.Subject, SubjectCut: d.SubjectCut, SubjectChecked: d.SubjectChecked, Multi: d.Multi || v.Dialog.Multi, Since: v.Since}

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

// turnAskWindow is how far before a turn's end an item of the role's own on the owner's todo still says it asked properly when it is
// not known when the turn before it ended (the first turn, or a record a start dropped): the ask is put on the list in the turn that
// asks, or a little before.
const turnAskWindow = 15 * time.Minute

// turnAsked reports whether a role put a question on the owner's todo in the turn that ended on the question the record holds: an ask it
// made, or a task it set waiting on the owner, whether it still waits there or the owner has answered it, done it, parked it or set it
// aside since (a plugin that was told of the item has had its card). That turn began after the one before it ended (rec.Prev), so an
// item made after that is of it, and one made before is of an earlier turn, which says nothing of this one's words; where the earlier
// turn's end is not known the item must be from turnAskWindow before the record on. A waiting task has no time it began to wait but
// the time its file was updated, so a later edit of it (a note, a state kept) counts as made in the turn that edited it. A file that
// does not read says nothing.
func turnAsked(c *crew.Crew, role string, rec *shift.TurnRecord) bool {
	since, ok := rec.Prev, !rec.Prev.IsZero()
	if !ok {
		since = rec.At.Add(-turnAskWindow)
	}

	asked, _, _ := askStore(c).Scan()

	for _, a := range asked {
		if a.Role == role && a.Created.After(since) {
			return true
		}
	}

	tasks, _, _ := taskStore(c).Scan()

	for _, t := range tasks {
		if t.Role == role && t.State == task.Waiting && !c.OnOthers(t.On) && t.Updated.After(since) {
			return true
		}
	}

	return false
}

// standingTurn is the question a role's last turn ended on, in plain words and with nothing on the owner's todo, that a plugin can
// be told of: the record its Stop hook kept, while the role is up and idle at its input line (no dialog, no prompt) in the session
// the record is of, and went idle within moments of the hook (shift.TurnRecord.Stands) — a role that took another turn since is
// not awaiting it. A record with no text is a turn that asked nothing, and is no thing. Not when the role put something of its own on
// the todo in that turn (turnAsked), open or closed: it asked, and a card exists. known is false when the record is there and does not
// read, and nothing is then judged ended.
func standingTurn(c *crew.Crew, v shift.View) (plugin.DialogFacts, bool, bool) {
	if v.State != shift.PresenceUp || v.Status != "idle" || v.Prompt || v.Dialog != nil {
		return plugin.DialogFacts{}, false, true
	}

	rec, err := shift.ReadTurn(c.Layout(), v.Role)
	if err != nil {
		return plugin.DialogFacts{}, false, false
	}

	if rec == nil || rec.Text == "" || !rec.Stands(v) {
		return plugin.DialogFacts{}, false, true
	}

	if turnAsked(c, v.Role, rec) {
		return plugin.DialogFacts{}, false, true
	}

	f := plugin.DialogFacts{Role: v.Role, ID: rec.ID, Question: rec.Text, Prompt: plugin.PromptTurn, Since: rec.At}

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

	return plugin.Item{ID: it.ID, Role: it.Role, Title: cmp.Or(it.Title, clip(oneLine(it.Text), 80)), Text: it.Text, Note: it.Note, Why: why,
		Reason: it.Reason, Rec: it.Rec, Options: it.Options, Opened: it.Created, Blocking: it.Blocks != "",
		Check: it.Kind == "check", Later: it.Kind == "later"}
}

// closureOf is how a thing a plugin was told of ended, read from where it lives: the ask, the task, the parked item, or
// the owner's mark of what he chose of a dialog. Anything it cannot place is gone.
func closureOf(c *crew.Crew, e plugin.Entry, at time.Time) plugin.Closure {
	cl := plugin.Closure{ID: e.ID, For: e.Kind, How: plugin.HowGone, At: at}

	if e.Kind == plugin.Dialog {
		// What was chosen is under the id the plugin was told (a plugin's press), or under the dialog's own (a hand's, the cockpit's).
		// A choice of no number is the owner's words to a question a role's turn ended on: it was answered, not chosen.
		chosen := plugin.ReadOwner(c.Layout().PluginOwner()).Chosen
		ch, ok := chosen[e.ID]

		if !ok || !ch.At.After(e.Told) {
			ch, ok = chosen[dialogOfEvent(e.ID)]
		}

		if ok && ch.At.After(e.Told) {
			cl.How, cl.By, cl.Text, cl.At = plugin.HowChosen, ch.By, ch.Text, ch.At

			if ch.N == 0 {
				cl.How = plugin.HowAnswered
			}
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

// dialogOfEvent is the dialog's own id (role:fingerprint) of the id an event carries (role:fingerprint:nonce); the one given when
// it has no nonce.
func dialogOfEvent(id string) string {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 3 {
		return id
	}

	return parts[0] + ":" + parts[1]
}

// fingerprintOfEvent is the dialog's fingerprint of the id an event carries (role:fingerprint:nonce).
func fingerprintOfEvent(id string) string {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 2 {
		return ""
	}

	return parts[1]
}

// nonceOfEvent is the nonce of an event's dialog id, empty when it has none.
func nonceOfEvent(id string) string {
	if parts := strings.SplitN(id, ":", 3); len(parts) == 3 {
		return parts[2]
	}

	return ""
}

// lockPlugin takes a plugin's delivery lock without waiting; false when another run holds it. The release is the caller's.
func lockPlugin(c *crew.Crew, name string) (release func(), ok bool) {
	return plugin.Lock(c.Layout().PluginLock(name), time.Now())
}

// pluginsDeliver is the delivery itself: every plugin of the crew, in parallel, each one's events one at a time. It returns
// the lines for the output; what could not be done is a line, never an error that stops the others.
func (a *app) pluginsDeliver(now time.Time, caller string) error {
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

			got := a.pluginDeliverOne(c, j.pl, w, now, caller)

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
func (a *app) pluginDeliverOne(c *crew.Crew, pl crew.Plugin, w *pluginWorld, now time.Time, caller string) []string {
	path := c.Layout().PluginTold(pl.Name)

	// What happened is dated by the beat's clock carried on by the time the delivery has taken: the beat's moment, and then the
	// real time, which is the same clock in a beat, and a moment a test chooses.
	began := time.Now()
	clock := func() time.Time { return now.Add(time.Since(began)) }

	var lines []string

	say := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf("plugin %s: ", pl.Name)+fmt.Sprintf(format, args...))
	}

	// The delivery is a process with no terminal: what is not an event told or given up on, and may need looking at, goes to the
	// event log too, bounded.
	problem := func(format string, args ...any) {
		say(format, args...)
		pluginProblem(c, caller, pl.Name, fmt.Sprintf(format, args...))
	}

	st, lerr := plugin.Load(path)
	if lerr != nil {
		problem("%v", lerr)
	}

	st.Observe(w.things, now, w.gaps)
	st.Partial = w.gaps.Words()

	plan := plugin.Make(plugin.Inputs{Plugin: pl, State: st, Things: w.things, Gaps: w.gaps, Now: now, OwnerActive: w.ownerActive})

	armed := plugin.Armed{Path: c.Layout().PluginArmed(pl.Name), Lock: c.Layout().PluginArmedLock(pl.Name)}

	// Whatever closes a card disarms its mark: a card of a question that is gone presses nothing, however long it stays on a phone.
	disarm := func(e plugin.Entry) {
		if e.Kind == plugin.Dialog {
			_ = armed.Disarm(nonceOfEvent(e.ID), clock())
		}
	}

	for _, key := range plan.Cancel {
		if e, ok := st.QueuedEntry(key); ok {
			disarm(e)
		}

		st.Cancel(key)
	}

	for _, key := range plan.Settle {
		if e := st.Told[key]; e != nil {
			disarm(*e)
			e.Closed = now
		}
	}

	save := func() {
		st.Prune(clock(), w.isOpen)

		if err := st.Save(path); err != nil {
			problem("not saved: %v", err)
		}
	}

	prog, perr := plugin.Resolve(pl, c.Layout().Dir)
	b := plugin.Builder{Crew: c.Path, Plugin: pl.Name, Clean: shift.QuoteN}

	// A program that does not resolve is a mistake of the setup, not of an event: nothing is run, no try is spent and nothing is
	// dropped; what waits goes once the program is there. It is said once, and doctor and plugin ls say it as long as it lasts.
	if perr != nil {
		if st.Problem != perr.Error() {
			st.Problem = perr.Error()
			problem("%v: nothing is told to it until it is there", perr)
		}

		plan.Send = nil
	} else {
		st.Problem = ""
	}

	taken, armFailed := false, false

	for _, d := range plan.Send {
		// What was answered since the look is not paged; nor is a question that is gone from the screen, or asked again.
		if d.Kind != plugin.Answered && d.Kind != plugin.Dialog && !itemOpen(c, d.Entry.ID) {
			st.Cancel(d.Key)

			continue
		}

		// A question is read again right before it is told, and a card that failed and waits for its retry is held to the same: if the
		// question was answered and asked again meanwhile, the card is of a wait that is over, and a fresh one is told of this.
		if d.Kind == plugin.Dialog {
			f, ok := w.dialogs[d.Key]
			stale := !ok || !w.dialogStill(f)

			if !stale && d.Retry != nil {
				stale = !f.Since.Equal(d.Entry.Opened) || fingerprintOfEvent(d.Entry.ID) != f.ID
			}

			if stale {
				if d.Retry != nil {
					disarm(d.Entry)
					st.Cancel(d.Key)
				}

				continue
			}
		}

		// The end of a card is known: its mark is disarmed before the plugin hears of it.
		if d.Kind == plugin.Answered {
			disarm(d.Entry)
		}

		line, entryID, err := w.line(b, d, now, armed, clock())

		switch {
		case errors.Is(err, errNotInLook):
			// It left the look between the plan and now: the next beat judges it.
			continue
		case err != nil:
			// Said once, as a program that is not there is: a disk that cannot keep the marks says it every beat otherwise.
			armFailed = true

			if msg := fmt.Sprintf("%s %s could not be armed for a one-shot press (%v): not told", d.Kind, d.Entry.ID, err); st.Arming != msg {
				st.Arming = msg
				problem("%s", msg)
			}

			continue
		}

		d.Entry.ID = entryID

		res := plugin.Exec(plugin.Spec{Program: prog, Dir: c.Dir, Stdin: line, Timeout: pl.TimeoutD(),
			Env: plugin.Env(os.Environ(), pl.Name, d.Kind, c.Path)})

		at := clock()

		if res.OK() {
			st.Succeed(d, at)
			taken = true
			pluginLog(c, caller, "plugin-told", pl.Name, d, "", res)
			say("%s %s told", d.Kind, d.Entry.ID)
			save()

			continue
		}

		reason := res.Reason(pl.TimeoutD())

		if st.Fail(d, string(line), at, reason) {
			if d.Kind == plugin.Dialog {
				// Never told: its mark is no one's.
				_ = armed.Disarm(nonceOfEvent(d.Entry.ID), at)
			}

			pluginLog(c, caller, "plugin-failed", pl.Name, d, fmt.Sprintf("given up after %d tries: %s", plugin.MaxTries, reason), res)
			say("%s %s given up after %d tries: %s", d.Kind, d.Entry.ID, plugin.MaxTries, reason)
		} else {
			problem("%s %s failed, tried again later: %s", d.Kind, d.Entry.ID, reason)
		}

		break
	}

	if !armFailed {
		st.Arming = ""
	}

	// A plugin that took an event is up: what it was given up on while it was not, and is still open, gets one more go.
	if taken {
		st.Revive(w.isOpen)
	}

	save()

	return lines
}

// errNotInLook says a thing a plan was made of is no longer among what the look found.
var errNotInLook = errors.New("not in the look")

// line is the event of a due send as it goes on the plugin's stdin, and the id it carries: the one a failed try kept, or one built
// now from what is open (an answered from where the thing lives). A dialog event is armed for one press before it goes: its nonce
// is in its id, and the first press made with it, in the wait it was told in, disarms it (the answer to a turn's question is the
// press of that one). The error is errNotInLook for a thing that is not in the look, else why the mark could not be written.
func (w *pluginWorld) line(b plugin.Builder, d plugin.Due, now time.Time, armed plugin.Armed, at time.Time) ([]byte, string, error) {
	switch {
	case d.Retry != nil:
		return []byte(d.Retry.Line), d.Entry.ID, nil
	case d.Kind == plugin.Answered:
		return b.Answered(closureOf(w.crew, d.Entry, now)).Line(), d.Entry.ID, nil
	case d.Kind == plugin.Dialog:
		f, ok := w.dialogs[d.Key]
		if !ok {
			return nil, d.Entry.ID, errNotInLook
		}

		f.Nonce = plugin.NewNonce()

		// A turn's question is armed as any dialog is: the one answer given with its mark (send --by --dialog) spends it, and only the
		// plugin that was told it has it.
		if err := armed.Arm(f.Nonce, plugin.DialogID(f.Role, f.ID), f.Since, at); err != nil {
			return nil, d.Entry.ID, err
		}

		ev := b.Dialog(f)

		return ev.Line(), ev.ID, nil
	}

	it, ok := w.items[d.Key]
	if !ok {
		return nil, d.Entry.ID, errNotInLook
	}

	return b.Item(d.Kind, w.itemFacts(it)).Line(), d.Entry.ID, nil
}

// pluginLog puts what happened to an event in the event log: a delivery (plugin-told) or the giving up (plugin-failed), with
// what the plugin printed, each stream cut to plugin.OutputMax and cleaned as any quotation: it is data.
func pluginLog(c *crew.Crew, caller, event, name string, d plugin.Due, note string, res plugin.Result) {
	detail := fmt.Sprintf("%s %s %s", name, d.Kind, d.Entry.ID)

	if note != "" {
		detail += ": " + note
	}

	for _, out := range []struct{ label, text string }{{"stdout", res.Stdout}, {"stderr", res.Stderr}} {
		if strings.TrimSpace(out.text) != "" {
			detail += fmt.Sprintf(" | %s: %s", out.label, shift.QuoteN(out.text, plugin.OutputMax))
		}
	}

	_ = eventLog(c).Add(events.Event{TS: time.Now(), Event: event, By: caller, Task: taskRefOf(d.Entry.ID), Detail: detail})
}

// pluginProblem puts a line of the delivery that needs looking at in the event log, as the caller it really is, cut.
func pluginProblem(c *crew.Crew, caller, name, line string) {
	_ = eventLog(c).Add(events.Event{TS: time.Now(), Event: "plugin-error", By: caller,
		Detail: name + ": " + shift.QuoteN(line, pluginProblemMax)})
}

// pluginProblemMax bounds a line of the delivery in the event log.
const pluginProblemMax = 500

// taskRefOf is the id when it is a task's.
func taskRefOf(id string) string {
	if task.Normalize(id) == id && strings.HasPrefix(id, "T-") {
		return id
	}

	return ""
}

// touchOwner notes that the owner answered in a terminal just now, for the crews that have plugins to hold a push by.
func touchOwner(s *shift.Shift) {
	// A role that says --from hand is no owner at the machine: it could hold the owner's pushes for as long as it liked.
	if len(s.Crew.Plugins) == 0 || os.Getenv("ZMINA_ROLE") != "" {
		return
	}

	now := s.Now()
	_ = plugin.UpdateOwner(s.Crew.Layout().PluginOwner(), now, func(o *plugin.Owner) { o.Active = now })
}
