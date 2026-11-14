package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
)

// courierBy is who the events of the courier are by, its own start and stop and what its passes do: the beat's own
// are by ticker.
const courierBy = "courier"

const (
	// courierPoll is how often the courier looks at state.json (one stat) when no flag says otherwise.
	courierPoll = 250 * time.Millisecond
	// courierMaxLife is how long a courier lives when no flag says otherwise: a supervisor starts the next, so a
	// courier never outlives what it was built from for long.
	courierMaxLife = 6 * time.Hour
	// roleBackoffMin and roleBackoffMax bound the wait before a role that is waiting but could not take its line is
	// tried again; it doubles from the first to the second.
	roleBackoffMin = time.Second
	roleBackoffMax = 15 * time.Second
	// errBackoffMin and errBackoffMax bound the wait, for the whole courier, after a pass that failed.
	// A pass that failed may have given a line its state does not show (a save that failed), and the next gives it
	// again: so the first wait is the beat's minute, not less, and the courier never repeats a line faster than the
	// beat alone would.
	errBackoffMin = time.Minute
	errBackoffMax = 5 * time.Minute
	// slowFactor is how many times the time a try took a role waits at least, when the try changed nothing: a tmux
	// that answers slowly (each call may take its whole timeout, under the state lock) must not have the courier
	// hold the lock most of the time. slowWaitMax bounds that wait.
	slowFactor  = 10
	slowWaitMax = 5 * time.Minute
	// spinRun is how many passes in a row for one role may each change the state before the next change is taken for
	// no progress (see roleWait): every change a pass makes is a line given, a try counted or a record settled, none of
	// which can go on for long, so a run this long is a bug that would otherwise rewrite state.json at the poll's pace.
	spinRun = 8
	// gatedLook is how often the courier looks again while a role that is up has something queued that Waiting leaves
	// out for a time (a slash command just typed, a try that failed a moment ago, info lines still riding): such a gate
	// ends with no change of state.json, so nothing else would call for the look before the beat. A look is a read of
	// the state without the lock.
	gatedLook = 5 * time.Second
)

// fileSig is what a stat says about a file: if it is there, when it was written, how big it is, which inode it is and
// when that last changed. state.json is replaced whole by a rename, so a save of it is always a different inode and a
// new time, however small the change.
type fileSig struct {
	present bool
	mtime   int64
	size    int64
	inode   uint64
	ctime   int64
}

// statSig stats path. A file that is not there is a signature of its own, not an error: it may be written later.
func statSig(path string) (fileSig, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileSig{}, nil
	}

	if err != nil {
		return fileSig{}, err
	}

	sig := fileSig{present: true, mtime: st.ModTime().UnixNano(), size: st.Size(), inode: fileInode(st)}
	if changed, _, ok := fileChange(st); ok {
		sig.ctime = changed.UnixNano()
	}

	return sig, nil
}

// courierPass is what one pass of the courier found.
type courierPass struct {
	// LockHeld is that the state lock was held by another zmina: the pass did not look at anything.
	LockHeld bool
	// Waiting are the roles Waiting named when the pass looked, whether or not they were tried.
	Waiting []string
	// Tried are the roles the pass asked and what it did for each.
	Tried []shift.RoleDelivery
	// Gated is that a role that is up has something queued that Waiting does not name yet (see gatedLook).
	Gated bool
}

// roleWait is what the courier keeps of a role that is waiting: when it may be tried next, and the wait the next try
// that changes nothing will double.
type roleWait struct {
	delay time.Duration
	due   time.Time
	// run is the passes in a row that each changed the state, up to spinRun.
	run int
}

// courierLoop is the courier's loop, with everything outside it handed in: the clock, the sleep between looks, the
// stats, the crew file's read, one pass and the event log. A run of it asks tmux, the registry and the state lock
// nothing but through Pass.
type courierLoop struct {
	// Poll is the time between looks; MaxLife how long the loop lives (0: as long as it is let).
	Poll, MaxLife time.Duration

	Now func() time.Time
	// Sleep waits d, or until ctx ends and then returns its error.
	Sleep func(ctx context.Context, d time.Duration) error
	// State, Crew and Exe stat state.json, the crew file and the zmina executable; Exe is nil when the executable is
	// not known, and then not watched.
	State, Crew, Exe func() (fileSig, error)
	// Reload reads the crew file (and builds what a pass needs from it); it is called once at the start and again
	// whenever the crew file's stat changed. A failed reload leaves what the last good one built.
	Reload func() error
	// Pass runs one pass over the roles try says may be tried now.
	Pass func(try func(role string) bool) (courierPass, error)
	// Event records an event; Warn says something to the person who runs the courier.
	Event func(event, detail string)
	Warn  func(msg string)
}

// Run looks at state.json every Poll and runs a pass when it changed since the last look, when a role's wait is over,
// or when the crew file was read again. Between those it does one stat of the state, one of the crew file and one of
// the executable, and nothing else: no lock, no state read, no tmux, no crew file parse.
//
// It returns nil for every end that is no fault (the executable changed on disk, the life is spent, ctx ended) and an
// error wrapping shift.ErrStateFuture for a state a newer zmina wrote, which it will not read again. Each end is
// logged once, as courier-stop with its reason.
func (l *courierLoop) Run(ctx context.Context) error {
	start := l.Now()

	// The stats go before what they stand for is read: a change in between is seen at the next look.
	crewSig, _ := l.Crew()

	// An executable that cannot be stat'ed at the start is not watched.
	exe, exeSig := l.Exe, fileSig{}

	if exe != nil {
		var err error

		if exeSig, err = exe(); err != nil {
			exe = nil
		}
	}

	if err := l.Reload(); err != nil {
		return err
	}

	l.Event("courier-start", fmt.Sprintf("poll %s, max life %s", l.Poll, lifeText(l.MaxLife)))

	reason := "stopped"
	defer func() { l.Event("courier-stop", reason) }()

	var (
		lastState fileSig
		looked    bool
		// again is that a pass is owed whatever the state's stat says: the crew file was read again.
		again    bool
		waits    = map[string]*roleWait{}
		errDelay time.Duration
		errUntil time.Time
		// gatedDue is when to look again for a queue a time gate holds (gatedLook); zero when there is none.
		gatedDue time.Time
	)

	for {
		now := l.Now()

		switch {
		case ctx.Err() != nil:
			reason = "interrupted"

			return nil
		case exeChanged(exe, exeSig):
			reason = "the zmina executable changed on disk"

			return nil
		case l.MaxLife > 0 && now.Sub(start) >= l.MaxLife:
			reason = "max life " + l.MaxLife.String() + " reached"

			return nil
		}

		// A crew file that cannot be read (an edit half made) leaves the courier on the last one it could; it is not
		// tried again until the file changes again, and said once.
		if cur, err := l.Crew(); err == nil && cur != crewSig {
			crewSig = cur

			if err := l.Reload(); err != nil {
				l.Warn("the crew file changed and is not read, the courier keeps the one before: " + err.Error())
			} else {
				again = true
			}
		}

		cur, statErr := l.State()
		gated := !gatedDue.IsZero() && !now.Before(gatedDue)

		if (statErr != nil || !looked || cur != lastState || again || gated || anyDue(waits, now)) &&
			!now.Before(errUntil) {
			res, err := l.Pass(func(role string) bool { return waits[role] == nil || !now.Before(waits[role].due) })

			switch {
			case errors.Is(err, shift.ErrStateFuture):
				reason = "state written by a newer zmina"

				return err
			case err != nil:
				// What a pass that failed did is not on disk: the next gives the same line again, so the whole courier
				// waits, longer each time.
				errDelay = doubled(errDelay, errBackoffMin, errBackoffMax)
				errUntil = l.Now().Add(errDelay)
				l.Warn(fmt.Sprintf("%v (next try in %s)", err, errDelay))
			case res.LockHeld:
				// Nothing was looked at: what called for the pass still does, and the next look tries again.
			default:
				errDelay, errUntil = 0, time.Time{}
				lastState, looked, again = cur, statErr == nil, false

				gatedDue = time.Time{}
				if res.Gated {
					gatedDue = l.Now().Add(gatedLook)
				}

				settle(waits, res, l.Now())
			}
		}

		if err := l.Sleep(ctx, l.Poll); err != nil {
			reason = "interrupted"

			return nil
		}
	}
}

// exeChanged reports whether the zmina executable is no longer the file the courier started from: replaced (a new
// inode, time or size), or gone for the moment a build takes to put the new one in its place. A stat that fails for any
// other reason says nothing.
func exeChanged(stat func() (fileSig, error), at fileSig) bool {
	if stat == nil {
		return false
	}

	cur, err := stat()

	return err == nil && cur != at
}

// anyDue reports whether a role's wait is over.
func anyDue(waits map[string]*roleWait, now time.Time) bool {
	for _, w := range waits {
		if !w.due.IsZero() && !now.Before(w.due) {
			return true
		}
	}

	return false
}

// settle takes what a pass found into the waits. A role the pass tried and changed nothing for waits twice as long as
// it did last, from roleBackoffMin to roleBackoffMax. A role it changed the state for is let go at once: it made
// progress (a line went, a try was counted), so it may be tried again at the next look, and its wait starts over —
// except past spinRun changes in a row, which wait like a role that changed nothing. A role Waiting no longer names
// has nothing left to wait for: its wait is forgotten, so it starts at roleBackoffMin the next time. A change of
// state.json that is no role's own never touches a wait.
func settle(waits map[string]*roleWait, res courierPass, now time.Time) {
	for role := range waits {
		if !slices.Contains(res.Waiting, role) {
			delete(waits, role)
		}
	}

	for _, t := range res.Tried {
		w := waits[t.Role]
		if w == nil {
			w = &roleWait{}
			waits[t.Role] = w
		}

		if t.Changed {
			w.run = min(w.run+1, spinRun)
		} else {
			w.run = 0
		}

		if t.Changed && w.run < spinRun {
			w.delay, w.due = 0, time.Time{}

			continue
		}

		w.delay = doubled(w.delay, roleBackoffMin, roleBackoffMax)
		w.due = now.Add(max(w.delay, min(slowFactor*t.Took, slowWaitMax)))
	}
}

// doubled is twice cur, at least lo and at most hi.
func doubled(cur, lo, hi time.Duration) time.Duration {
	if cur < lo {
		return lo
	}

	return min(cur*2, hi)
}

func lifeText(d time.Duration) string {
	if d <= 0 {
		return "none"
	}

	return d.String()
}

// courierCommand is zmina _courier: a resident loop that repeats the delivery pass (zmina tick --deliver) the moment
// state.json changes, so a line that waits for a role goes when the role can take it, not at the next minute's beat.
// It runs in the foreground until it is ended. The ticker's install puts a service beside the ticker that starts it
// again whenever it ends (unless the crew file says courier: off); started by hand it is the same process, and the
// one that holds courier.lock is the only one that runs.
func (a *app) courierCommand() *cobra.Command {
	var poll, maxLife time.Duration

	c := &cobra.Command{
		Use:    "_courier",
		Short:  "Repeat the delivery pass whenever the state changes, until ended (the ticker's service keeps it running)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if poll <= 0 {
				return errors.New("--poll must be above zero")
			}

			if maxLife < 0 {
				return errors.New("--max-life must not be negative (0 is no limit)")
			}

			parent := cmd.Context()
			if parent == nil {
				parent = context.Background()
			}

			ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()

			// The first signal ends the courier once the pass it is in is done; a second, while that pass still
			// waits for tmux, is not swallowed.
			go func() {
				<-ctx.Done()
				stop()
			}()

			return a.courier(ctx, poll, maxLife)
		},
	}

	c.Flags().DurationVar(&poll, "poll", courierPoll, "how often state.json is looked at")
	c.Flags().DurationVar(&maxLife, "max-life", courierMaxLife, "how long the courier lives, then ends cleanly (0: no limit)")
	_ = c.Flags().MarkHidden("poll")
	_ = c.Flags().MarkHidden("max-life")

	return c
}

// courier runs the loop over this crew until it ends. A second courier for the same crew finds the first one's lock
// and ends at once with a plain message and exit 0: it is no failure that what was asked for is already there, and a
// supervisor that starts the courier again must not count it as one.
func (a *app) courier(ctx context.Context, poll, maxLife time.Duration) error {
	path, err := filepath.Abs(a.crewPath())
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return noCrewFile(&crew.NoFileError{Path: path})
	}

	dir := filepath.Dir(path)

	release, err := lockCourier(dir)

	var running *courierRunning
	if errors.As(err, &running) {
		fmt.Fprintln(a.out, running)

		return nil
	}

	if err != nil {
		return err
	}
	defer release()

	loop := a.newCourierLoop(path, poll, maxLife)

	return loop.Run(ctx)
}

// courierRunning is a courier that is already running for the crew.
type courierRunning struct{ pid int }

func (e *courierRunning) Error() string {
	if e.pid > 0 {
		return "courier: already running for this crew (pid " + strconv.Itoa(e.pid) + "); nothing to do"
	}

	return "courier: already running for this crew; nothing to do"
}

// lockCourier takes courier.lock in dir without waiting, for as long as the courier lives: the system lets it go when
// the process ends, however it ends. The file names the pid that holds it, for the message of a second courier.
func lockCourier(dir string) (release func(), err error) {
	path := crew.Layout{Dir: dir}.CourierLock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("courier: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		held := &courierRunning{}
		if raw, rerr := os.ReadFile(path); rerr == nil {
			held.pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}

		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, held
		}

		return nil, fmt.Errorf("courier: lock: %w", err)
	}

	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// courierLock is what a look at courier.lock found.
type courierLock struct {
	// Held is that a courier holds the lock now.
	Held bool
	// PID is the pid the holder wrote into the file, for a message; 0 when there is none to read.
	PID int
	// Since is when the holder wrote it (the file's time of change: at its start, and never again while it lives); zero when
	// nobody holds the lock.
	Since time.Time
}

// probeCourierLock says whether a courier holds the lock at path without taking it for longer than the call: one
// non-blocking shared lock, let go at once. It never makes, writes, truncates or removes the file, so a file that is not
// there is a lock nobody holds, and the holder's pid is read only to be named. The one cost is that a courier that tries
// for the lock in the instant the probe holds it finds it taken and ends with "already running"; its supervisor
// starts it again.
func probeCourierLock(path string) (courierLock, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return courierLock{}, nil
	}

	if err != nil {
		return courierLock{}, err
	}

	defer func() { _ = f.Close() }()

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

		return courierLock{}, nil
	}

	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return courierLock{}, fmt.Errorf("lock: %w", err)
	}

	held := courierLock{Held: true}

	if raw, err := os.ReadFile(path); err == nil {
		held.PID, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}

	if st, err := f.Stat(); err == nil {
		held.Since = st.ModTime()
	}

	return held, nil
}

// newCourierLoop is the loop over the real clock, files and shift of the crew file at path. The shift is built when the
// crew file is read (at the start, and again when it changes) and each pass works on a copy of it, so a pass never
// parses the crew file and never carries what another pass left in a shift (a failed save, the notes it told).
func (a *app) newCourierLoop(path string, poll, maxLife time.Duration) *courierLoop {
	layout := crew.Layout{Dir: filepath.Dir(path)}
	log := &events.Log{Path: layout.Events()}

	var (
		base   *shift.Shift
		warned = map[string]bool{}
		logErr bool
	)

	// A warning is said once however many passes meet it: the shift's own warns come from every pass.
	warn := func(err error) {
		if text := err.Error(); !warned[text] {
			if len(warned) >= 256 {
				clear(warned)
			}

			warned[text] = true

			fmt.Fprintln(a.err, "zmina: warning:", err)
		}
	}

	l := &courierLoop{
		Poll:    poll,
		MaxLife: maxLife,
		Now:     time.Now,
		Sleep:   sleepCtx,
		State:   func() (fileSig, error) { return statSig(layout.State()) },
		Crew:    func() (fileSig, error) { return statSig(path) },
		Warn:    func(msg string) { fmt.Fprintln(a.err, "courier:", msg) },
	}

	if exe, err := os.Executable(); err == nil {
		l.Exe = func() (fileSig, error) { return statSig(exe) }
	}

	l.Reload = func() error {
		s, err := a.newShift()
		if err != nil {
			return err
		}

		s.Warn = warn
		base = s

		return nil
	}

	l.Event = func(event, detail string) {
		now := time.Now()
		if err := log.Add(events.Event{TS: now, Event: event, By: courierBy, Detail: detail}); err != nil && !logErr {
			logErr = true

			warn(fmt.Errorf("event log: %w", err))
		}

		fmt.Fprintln(a.out, now.Format(time.DateTime), event, detail)
	}

	l.Pass = func(try func(role string) bool) (courierPass, error) { return a.courierDeliver(base, try) }

	return l
}

// sleepCtx waits d, or until ctx ends and then returns its error.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// courierDeliver is one pass of the courier over the roles try allows: zmina tick --deliver (deliverPass) for some of
// the waiting roles. It takes the state lock the way deliverPass does, without waiting: a lock another zmina holds ends it
// at once, with nothing written or logged and no tmux asked (the state was read, without the lock). It reads the state first without the lock (state.json is replaced
// whole, so a read never sees half of it) and takes the lock only when a role that may be tried is waiting, so the
// changes of the state that call for nothing cost the beat and the commands no wait. Under the lock it asks the roles
// that are due, saves only a state that changed, and starts no role and runs nothing of the beat.
func (a *app) courierDeliver(base *shift.Shift, try func(role string) bool) (courierPass, error) {
	dir := base.Crew.StateDir()

	// A pass works on a copy: what a pass leaves in a shift (a failed save's mark, the roles it released) is its own.
	peek := *base

	st, err := shift.Read(dir)
	if err != nil {
		return courierPass{}, err
	}

	peek.State = st

	if waiting := peek.Waiting(); !slices.ContainsFunc(waiting, try) {
		return courierPass{Waiting: waiting, Gated: queueGated(&peek, waiting)}, nil
	}

	// The pass's own copy takes the lock too: what taking it logs and notes stays the pass's.
	s := *base
	s.By = courierBy

	l, err := shift.TryLockAs(dir, shift.LockOpts{Verb: a.verb, Log: s.LogEvent})
	if errors.Is(err, shift.ErrLockHeld) {
		return courierPass{LockHeld: true}, nil
	}

	if err != nil {
		return courierPass{}, err
	}

	// As deliverPass: the pass leaves CommittedMessageStages and the watches' pruning to the beat on purpose.
	s.State, s.Flush, s.Mark, s.By = l.State, l.Flush, l.Mark, courierBy

	out := courierPass{Waiting: s.Waiting()}
	due := slices.DeleteFunc(slices.Clone(out.Waiting), func(role string) bool { return !try(role) })

	out.Tried, err = s.DeliverRoles(due)
	out.Gated = queueGated(&s, s.Waiting())
	if err != nil || !slices.ContainsFunc(out.Tried, func(d shift.RoleDelivery) bool { return d.Changed }) {
		l.Release()

		return out, err
	}

	if err := l.Save(); err != nil {
		return out, fmt.Errorf("not saved: %w", err)
	}

	return out, nil
}

// queueGated reports whether a role that is up has something queued that waiting does not name: a line of its own,
// or, for the dispatcher, a signal or a note. Waiting leaves such a role out only for a time (see gatedLook).
func queueGated(s *shift.Shift, waiting []string) bool {
	for _, name := range s.Names() {
		rs := s.State.Roles[name]
		if rs == nil || !rs.Up || slices.Contains(waiting, name) {
			continue
		}

		if len(rs.Inbox) > 0 {
			return true
		}

		// Info lines still waiting for a line to go with are not due: nothing to look again for.
		if name == s.Crew.Dispatcher && len(s.State.Signals)+len(s.State.Notes) > 0 && !s.RidingOnly() {
			return true
		}
	}

	return false
}
