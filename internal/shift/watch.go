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
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Every     string    `json:"every"`
	Enabled   bool      `json:"enabled"`
	LastRun   time.Time `json:"last_run,omitzero"`
	LastOK    bool      `json:"last_ok"`
	LastError string    `json:"last_error,omitempty"`
	Failures  int       `json:"failures,omitempty"`
	NextRun   time.Time `json:"next_run,omitzero"`
	Running   bool      `json:"running,omitempty"`
	Keys      int       `json:"keys"`
	Pending   int       `json:"pending,omitempty"`
	Recent    []string  `json:"recent,omitempty"`
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
	now := s.Now()

	var runs []WatchRun

	for _, name := range s.Crew.WatchNames() {
		w := s.Crew.Watches[name]
		if !w.On() {
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

// PeekWatch reads what a dry run needs, marking nothing.
func (s *Shift) PeekWatch(name string) (WatchRun, error) {
	w, err := s.Crew.Watch(name)
	if err != nil {
		return WatchRun{}, err
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
	st := s.watchState(name)
	role := "watch:" + name

	// A run that is no longer the current reservation (it outlived its timeout and another took over), or whose
	// watch was changed or removed while it ran, is not applied: its keys would overwrite newer ones.
	if st.Running == nil || st.Running.Token != run.Token {
		s.log(role, "watch-stale", "", "result of a superseded run discarded")

		return []string{name + ": result of a superseded run discarded"}
	}

	st.Running = nil

	if cur, err := s.Crew.Watch(name); err != nil || cur.Fingerprint() != run.Config {
		s.log(role, "watch-stale", "", "the watch changed while it ran; result discarded")

		return []string{name + ": the watch changed while it ran; result discarded"}
	}

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

	cfg := watch.Config{Quiet: w.QuietD(), ForgetAfter: w.ForgetAfterD(), Baseline: w.Baselines(), Info: w.Info}
	if baseline {
		st.Baselined, cfg.Baseline = false, true
	}

	outs := watch.Apply(st, res.Events, cfg, now, func(e watch.Event) error {
		_, err := s.Note(name, e.Line(), false)

		return err
	})
	st.Cache = res.Cache

	counts := map[watch.Fate]int{}

	for _, o := range outs {
		counts[o.Fate]++

		switch o.Fate {
		case watch.Noted:
			st.Remember(now.Format("01-02 15:04") + " noted " + o.Event.Line())
			s.log(role, "watch-noted", "", "key="+o.Event.Key+" "+o.Event.Line())
		case watch.Dropped:
			st.Remember(now.Format("01-02 15:04") + " dropped " + o.Event.Key + " (" + o.Reason + ")")
			s.log(role, "watch-dropped", "", "key="+o.Event.Key+" reason="+o.Reason)
			did = append(did, name+": dropped "+o.Event.Key+" ("+o.Reason+")")
		case watch.QueueFull:
			did = append(did, name+": not queued, retried next run: "+o.Event.Key+": "+o.Reason)
		}
	}

	summary := fmt.Sprintf("%d events", len(res.Events))
	for _, f := range []watch.Fate{watch.Noted, watch.Dup, watch.Quiet, watch.Baseline, watch.InfoOff,
		watch.QueueFull, watch.Dropped} {
		if counts[f] > 0 {
			summary += fmt.Sprintf(", %d %s", counts[f], f)
		}
	}

	s.log(role, "watch-run", "", summary)

	// A run that found nothing new stays out of the tick log; the event log keeps every run.
	if len(outs) > counts[watch.Dup] {
		did = append(did, name+": "+summary)
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

	var out []WatchView

	for _, name := range s.Crew.WatchNames() {
		w := s.Crew.Watches[name]
		v := WatchView{Name: name, Kind: w.Kind, Every: w.Every, Enabled: w.On()}

		if st := s.State.Watch[name]; st != nil {
			v.LastRun, v.LastOK, v.LastError, v.Failures = st.LastRun, st.LastOK, st.LastError, st.Failures
			v.Keys, v.Pending, v.Recent = len(st.Keys), len(st.Pending), st.Recent
			v.Running = st.Running.Live(now, w.TimeoutD())
		}

		if v.Enabled {
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
func (s *Shift) PruneWatches() {
	for name := range s.State.Watch {
		if _, ok := s.Crew.Watches[name]; !ok {
			delete(s.State.Watch, name)
		}
	}
}
