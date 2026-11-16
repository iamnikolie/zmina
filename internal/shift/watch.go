package shift

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/watch"
)

// failNoteAfter failed runs in a row, the dispatcher hears that a watch is failing.
const failNoteAfter = 3

// WatchRun is one run of a watch: begun under the lock, its source run outside it, finished under it again.
type WatchRun struct {
	Watch *crew.Watch
	Env   watch.Env
	// Started is when the run began; PID marks it as this process's. Token is its reservation, Config the watch's
	// configuration when it began: a result is applied only to a reservation that is current and unchanged.
	Started time.Time
	PID     int
	Token   string
	Config  string
}

// WatchView is what is known of one watch, for watch ls.
type WatchView struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Every   string `json:"every"`
	Enabled bool   `json:"enabled"`
	// Place is the node that runs the watch in a crew over nodes; Away says it is not this one.
	Place     string    `json:"place,omitempty"`
	Away      bool      `json:"away,omitempty"`
	LastRun   time.Time `json:"last_run,omitzero"`
	LastOK    bool      `json:"last_ok"`
	LastError string    `json:"last_error,omitempty"`
	Failures  int       `json:"failures,omitempty"`
	NextRun   time.Time `json:"next_run,omitzero"`
	Running   bool      `json:"running,omitempty"`
	Keys      int       `json:"keys"`
	// Pending is the events the quiet window holds (HELD).
	Pending int `json:"pending,omitempty"`
	// Unacked is the keys whose notes wait on an ack (PENDING), Waiting the keys themselves.
	Unacked int          `json:"unacked,omitempty"`
	Waiting []WaitingKey `json:"waiting,omitempty"`
	Recent  []string     `json:"recent,omitempty"`
}

// watchState is the state of a watch, made on first use.
func (s *Shift) watchState(name string) *watch.State {
	st := s.State.Watch[name]
	if st == nil {
		st = watch.NewState()
		s.State.Watch[name] = st
	}

	return st
}

// DueWatches begins the runs of the watches that are on and due (the last run older than every), skipping one whose
// previous run still goes. A run that came due several times while the machine slept runs once.
func (s *Shift) DueWatches() []WatchRun {
	// A machine that runs nothing of a crew over nodes runs none of its detectors either.
	if !s.Crew.Runs() {
		return nil
	}

	now := s.Now()

	var runs []WatchRun

	for _, name := range s.Crew.WatchNames() {
		w := s.Crew.Watches[name]
		if !w.On() || !s.Crew.WatchHere(w) {
			continue
		}

		st := s.State.Watch[name]
		if st != nil && now.Sub(st.LastRun) < w.EveryD() {
			continue
		}

		if run, err := s.BeginWatch(name); err == nil {
			runs = append(runs, run)
		}
	}

	return runs
}

// BeginWatch marks a watch as running and reads what its source needs: the stored fingerprints and its cache.
func (s *Shift) BeginWatch(name string) (WatchRun, error) {
	w, err := s.Crew.Watch(name)
	if err != nil {
		return WatchRun{}, err
	}

	// By hand too: a run here would take the keys into this node's state and note them to this node's dispatcher.
	if !s.Crew.WatchHere(w) {
		return WatchRun{}, s.watchAway(w)
	}

	now := s.Now()
	st := s.watchState(name)

	if st.Running.Live(now, w.TimeoutD()) {
		return WatchRun{}, fmt.Errorf("watch %s: a run is going since %s (pid %d)", name,
			st.Running.Since.Format(time.TimeOnly), st.Running.PID)
	}

	run := WatchRun{Watch: w, Started: now, PID: os.Getpid(), Env: s.watchEnv(name, st),
		Token: fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano()), Config: w.Fingerprint()}
	st.Running = &watch.Running{PID: run.PID, Since: now, Token: run.Token}

	return run, nil
}

// watchAway is the refusal of a run of a watch that another node runs.
func (s *Shift) watchAway(w *crew.Watch) error {
	if s.Crew.Here == "" || !s.Crew.HasNode(s.Crew.Here) {
		return fmt.Errorf("watch %s: this machine is no node of the crew file, and a crew over nodes runs nothing here", w.Name)
	}

	node := s.Crew.WatchPlace(w)

	switch {
	case !s.Crew.HasNode(node):
		return fmt.Errorf("watch %s: place %q is no node of the crew file: it runs on no node", w.Name, w.Place)
	case !s.Crew.WatchHeard(w):
		return fmt.Errorf("watch %s: place %s holds no role of the crew, so no dispatcher runs there: it runs on no node",
			w.Name, node)
	}

	return fmt.Errorf("watch %s is placed on %s, not on this node (%s): run it there", w.Name, node, s.Crew.Here)
}

// PeekWatch reads what a dry run needs, marking nothing.
func (s *Shift) PeekWatch(name string) (WatchRun, error) {
	w, err := s.Crew.Watch(name)
	if err != nil {
		return WatchRun{}, err
	}

	// A dry run runs the source too, and says fates against this node's state.
	if !s.Crew.WatchHere(w) {
		return WatchRun{}, s.watchAway(w)
	}

	return WatchRun{Watch: w, Started: s.Now(), Env: s.watchEnv(name, s.State.Watch[name])}, nil
}

func (s *Shift) watchEnv(name string, st *watch.State) watch.Env {
	env := watch.Env{CrewPath: s.Crew.Path, Dir: s.Crew.Layout().Watch(name), Seen: st.Seen()}
	if st != nil {
		env.Cache = st.Cache
	}

	return env
}

// FinishWatch takes a run's result under the lock. A failed run changes no key; three in a row tell the
// dispatcher once, and the first success after that tells it the watch recovered. A successful run is diffed
// (watch.Apply): news goes to the dispatcher's notes as "<watch>: <text> <url>". With baseline, everything the run
// reports is recorded and nothing noted. It returns lines for the tick log.
func (s *Shift) FinishWatch(run WatchRun, res watch.Result, baseline bool) []string {
	w := run.Watch
	name := w.Name
	now := s.Now()
	role := "watch:" + name
	st := s.State.Watch[name]
	cur, curErr := s.Crew.Watch(name)

	// A result that is not applied leaves the state as it is and makes none: the state of a watch that went to another node, or left
	// the file, is the prune's to drop (it runs before a command does, so it may be gone already), and an empty one made here would
	// be saved and kept. Only the reservation this run took is given back.
	discard := func(why string) []string {
		if st != nil && st.Running != nil && st.Running.Token == run.Token {
			st.Running = nil
		}

		s.log(role, "watch-stale", "", why)

		return []string{name + ": " + why}
	}

	// The file placed the watch on another node while it ran: this node's run is over, whatever the state says (a prune that ran
	// first has cleared the reservation or dropped the state).
	if curErr == nil && s.Crew.WatchAway(cur) {
		return discard("the watch was placed on another node while it ran; result discarded")
	}

	// A run that is no longer the current reservation (it outlived its timeout and another took over), or whose
	// watch was changed or removed while it ran, is not applied: its keys would overwrite newer ones.
	if st == nil || st.Running == nil || st.Running.Token != run.Token {
		return discard("result of a superseded run discarded")
	}

	st.Running = nil

	// Neither this node's watch nor another node's: its place is no node of the file or one no dispatcher hears, or this machine's
	// node name could not be read while it ran.
	if curErr == nil && !s.Crew.WatchHere(cur) {
		return discard("the watch is no longer one this node can run (its place is no node, or a node with no dispatcher to hear it, " +
			"or this machine's node name could not be read); result discarded")
	}

	if curErr != nil || cur.Fingerprint() != run.Config {
		return discard("the watch changed while it ran; result discarded")
	}

	// How the run before this one ended, for what the log keeps of a run that found nothing new.
	prevRun, prevOK := st.LastRun, st.LastOK
	st.LastRun = run.Started

	var did []string

	for _, l := range res.Stderr {
		did = append(did, name+": "+l)
	}

	if res.Err != nil {
		st.Failures++
		st.LastOK, st.LastError = false, res.Err.Error()
		s.log(role, "watch-failed", "", fmt.Sprintf("run %d: %s", st.Failures, res.Err))
		did = append(did, fmt.Sprintf("%s: failed (%d in a row): %s", name, st.Failures, res.Err))

		if st.Failures >= failNoteAfter && !st.FailNoted {
			st.FailNoted = true
			s.healthNote(name, fmt.Sprintf("failing for %d runs: %s", st.Failures, res.Err))
		}

		return did
	}

	if st.FailNoted {
		s.healthNote(name, "recovered")
	}

	st.Failures, st.FailNoted, st.LastOK, st.LastError = 0, false, true, ""

	cfg := watch.Config{Quiet: w.QuietD(), ForgetAfter: w.ForgetAfterD(), Baseline: w.Baselines(), Info: w.Info,
		Renotify: w.RenotifyD()}
	if baseline {
		st.Baselined, cfg.Baseline = false, true
	}

	// A note the dispatcher turned into a task is acked before the run decides what to say again.
	if len(st.Waiting()) > 0 {
		for _, a := range ackByTasks(name, st, s.allTasks(), now) {
			s.log(role, "watch-ack", "", "key="+a)
		}
	}

	outs := watch.Apply(st, res.Events, cfg, now, func(e watch.Event) (string, error) {
		line := e.Line()
		if !e.Since.IsZero() {
			if s.stillQueued(st.Keys[e.Key]) {
				return "", watch.ErrStillQueued
			}

			line = s.renoteLine(name, e)
		}

		note, _, err := s.note(name, line, false, false)

		return note, err
	})
	st.Cache = res.Cache

	counts := map[watch.Fate]int{}

	for _, o := range outs {
		counts[o.Fate]++

		switch o.Fate {
		case watch.Noted:
			st.Remember(now.Format("01-02 15:04") + " noted " + o.Event.Line())
			s.log(role, "watch-noted", "", "key="+o.Event.Key+" "+o.Event.Line())
		case watch.Renoted:
			st.Remember(now.Format("01-02 15:04") + " said again " + o.Event.Key)
			s.log(role, "watch-renoted", "", "key="+o.Event.Key+" pending since "+o.Event.Since.Format(time.RFC3339))
		case watch.Dropped:
			st.Remember(now.Format("01-02 15:04") + " dropped " + o.Event.Key + " (" + o.Reason + ")")
			s.log(role, "watch-dropped", "", "key="+o.Event.Key+" reason="+o.Reason)
			did = append(did, name+": dropped "+o.Event.Key+" ("+o.Reason+")")
		case watch.QueueFull:
			did = append(did, name+": not queued, retried next run: "+o.Event.Key+": "+o.Reason)
		}
	}

	summary := fmt.Sprintf("%d events", len(res.Events))
	for _, f := range []watch.Fate{watch.Noted, watch.Renoted, watch.Dup, watch.Quiet, watch.Baseline, watch.InfoOff,
		watch.QueueFull, watch.Dropped} {
		if counts[f] > 0 {
			summary += fmt.Sprintf(", %d %s", counts[f], f)
		}
	}

	// A run that found something new is logged, and stays in the tick log. One that found nothing new is logged
	// once a clock hour, and after a run that failed or the first one: a watch that runs every few minutes and
	// finds nothing would otherwise be most of the event log. watch ls has the time of every last run.
	fresh := len(outs) > counts[watch.Dup]

	switch {
	case fresh:
		s.log(role, "watch-run", "", summary)
		did = append(did, name+": "+summary)
	case prevRun.IsZero() || !prevOK || !sameHour(prevRun, run.Started):
		s.log(role, "watch-run", "", summary+" (nothing new; such runs are logged once an hour)")
	}

	return did
}

// healthNote tells the dispatcher of the watch itself, not of work it found.
func (s *Shift) healthNote(name, text string) {
	if _, err := s.Note(name, text, false); err != nil {
		s.log("watch:"+name, "watch-failed", "", "note: "+err.Error())
	}
}

// ForgetWatch drops stored fingerprints of a watch, one key or all, so its next run notes them again.
func (s *Shift) ForgetWatch(name, key string) (int, error) {
	if _, err := s.Crew.Watch(name); err != nil {
		return 0, err
	}

	// Forgetting here would change nothing on the node that runs the watch, and say that it did.
	if w, err := s.Crew.Watch(name); err == nil && !s.Crew.WatchHere(w) {
		return 0, s.watchAway(w)
	}

	st := s.State.Watch[name]
	if st == nil {
		return 0, nil
	}

	n := st.Forget(key)
	s.log("watch:"+name, "watch-forget", "", strings.TrimSpace("key="+key))

	return n, nil
}

// WatchViews lists the watches.
func (s *Shift) WatchViews() []WatchView {
	now := s.Now()
	tasks := s.allTasks()

	var out []WatchView

	for _, name := range s.Crew.WatchNames() {
		w := s.Crew.Watches[name]
		v := WatchView{Name: name, Kind: w.Kind, Every: w.Every, Enabled: w.On(), Place: s.Crew.WatchPlace(w),
			Away: !s.Crew.WatchHere(w)}

		if st := s.State.Watch[name]; st != nil {
			v.LastRun, v.LastOK, v.LastError, v.Failures = st.LastRun, st.LastOK, st.LastError, st.Failures
			v.Keys, v.Pending, v.Recent = len(st.Keys), len(st.Pending), st.Recent
			v.Waiting = waitingKeys(w, st, tasks)
			v.Unacked = len(v.Waiting)
			v.Running = st.Running.Live(now, w.TimeoutD())
		}

		if v.Enabled && !v.Away {
			v.NextRun = now
			if !v.LastRun.IsZero() && v.LastRun.Add(w.EveryD()).After(now) {
				v.NextRun = v.LastRun.Add(w.EveryD())
			}
		}

		out = append(out, v)
	}

	return out
}

// PruneWatches drops the state of watches no longer in the crew.
// What a node kept of a watch that the file now places on another node goes too, but for the keys whose notes still
// wait on an ack: those were said to this node's dispatcher, and so they stay, listed and ackable here, until the last is
// acked (the other node's first run records them without a word with baseline, and notes them again without it). The
// renotify of a key is said only inside a run of its watch, which is another node's now. A key whose note a task carries
// is acked already and is recorded as acked here (ackByTasks), so the state goes once nothing is left that waits.
// Nothing is dropped on a machine that is no node of the file: a node name that cannot be read for a moment must not
// cost a watch what it knows.
func (s *Shift) PruneWatches() {
	for name, st := range s.State.Watch {
		w, ok := s.Crew.Watches[name]
		if !ok {
			delete(s.State.Watch, name)

			continue
		}

		if st == nil || !s.Crew.WatchAway(w) {
			continue
		}

		// A key whose note the dispatcher turned into a task is acked, as the run that would have said it again acks it
		// (FinishWatch): that run is another node's now, so nothing else here would ever record it, and the state would be kept
		// for a wait that is over. What is left waiting is judged after that, and the acks stay in the state that is kept.
		if len(st.Waiting()) > 0 {
			for _, a := range ackByTasks(name, st, s.allTasks(), s.Now()) {
				s.log("watch:"+name, "watch-ack", "", "key="+a)
			}
		}

		if len(st.Waiting()) == 0 {
			delete(s.State.Watch, name)

			continue
		}

		// Its runs are over here: no failure of them is news any more, and nothing held will be noted.
		st.Pending, st.Running = nil, nil
		st.Failures, st.FailNoted, st.LastError, st.LastOK = 0, false, "", true
	}
}

// sameHour reports whether a and b are in one hour of the clock.
func sameHour(a, b time.Time) bool { return a.Truncate(time.Hour).Equal(b.Truncate(time.Hour)) }
