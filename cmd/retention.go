package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/lean"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

const (
	// retentionEvery is how often the ticker runs the pass, counted from the start of the last one.
	retentionEvery = 24 * time.Hour
	// tickLogMax is the size past which the pass cuts tick.log, and courier.log with it, down to its last tickLogKeep
	// bytes.
	tickLogMax  = 5 << 20
	tickLogKeep = 1 << 20
)

// retentionTimeout is the most one pass may take. What it did not reach waits for the next one.
var retentionTimeout = time.Minute

var taskDirName = regexp.MustCompile(`^T-[0-9]+$`)

// retentionStamp is retention.json next to the crew file: when the last pass began and what it moved. The ticker
// runs a pass when a day has passed since At. It is its own small file, not a field of the state, so the pass needs
// no state lock.
type retentionStamp struct {
	At    time.Time      `json:"at"`
	Moved map[string]int `json:"moved,omitempty"`
}

func retentionStampPath(dir string) string { return filepath.Join(dir, "retention.json") }

func readRetentionStamp(dir string) retentionStamp {
	var st retentionStamp
	if raw, err := os.ReadFile(retentionStampPath(dir)); err == nil {
		_ = json.Unmarshal(raw, &st)
	}

	return st
}

func (st retentionStamp) due(now time.Time) bool {
	return st.At.IsZero() || now.Sub(st.At) >= retentionEvery || st.At.After(now)
}

func writeRetentionStamp(dir string, st retentionStamp) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}

	return fsx.WriteFile(retentionStampPath(dir), append(raw, '\n'), 0o600)
}

// errRetentionBusy is a pass that finds another one running.
var errRetentionBusy = errors.New("another retention pass is running")

// lockRetention takes retention.lock without waiting; the pass holds it for its whole run, so two beats that overlap,
// or a hand run in a beat, do not run it twice.
func lockRetention(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "retention.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errRetentionBusy
		}

		return nil, err
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// retentionBeat is the ticker's daily pass: finished work older than the crew's retention span (30 days, or the key's
// days) is moved to archive/retention/<yyyy-mm>/, tick.log and courier.log are cut to their tails and old meter
// lines are archived. It runs after the beat's locked phases and holds no state lock; the lock it takes is its own
// file's. It fails nothing: a problem is a line in the beat's output, and the next pass is tomorrow's.
func (a *app) retentionBeat(now time.Time) {
	path := a.crewPath()
	if !readRetentionStamp(filepath.Dir(path)).due(now) {
		return
	}

	c, err := crew.Load(path)
	if err != nil || !c.RetentionOn() {
		return
	}

	lines, err := a.retentionRun(c, now, "ticker", false, false)
	if err != nil && !errors.Is(err, errRetentionBusy) {
		lines = append(lines, "retention: "+err.Error())
	}

	a.printTick(lines)
}

// retentionRun is one pass: with dry nothing is moved and each thing that would be is a line; otherwise what it did
// is counted, logged as one event and written into the stamp. The lines it returns are for the ticker's output (what
// was moved, what went wrong) or, for a dry or hand run, for the owner. A pass by the ticker (hand false) runs only
// when it is due, checked again under the lock.
func (a *app) retentionRun(c *crew.Crew, now time.Time, by string, hand, dry bool) ([]string, error) {
	dir := c.StateDir()

	if !dry {
		unlock, err := lockRetention(dir)
		if err != nil {
			return nil, err
		}
		defer unlock()

		if !hand && !readRetentionStamp(dir).due(now) {
			return nil, nil
		}

		if err := writeRetentionStamp(dir, retentionStamp{At: now}); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), retentionTimeout)
	defer cancel()

	p := &retentionPass{ctx: ctx, c: c, dir: dir, now: now, cutoff: now.Add(-c.RetentionSpan()), dry: dry, moved: map[string]int{}}
	p.run()

	summary := p.summary()

	var lines []string

	switch {
	case dry || hand:
		lines = append(lines, p.said...)
		lines = append(lines, summary)
	case len(p.moved) > 0 || len(p.problems) > 0:
		lines = append(lines, summary)
	}

	lines = append(lines, p.problems...)

	if !dry {
		_ = writeRetentionStamp(dir, retentionStamp{At: now, Moved: p.moved})
		_ = eventLog(c).Add(events.Event{TS: now, Event: "retention", By: by, Detail: summary})
	}

	return lines, nil
}

// retentionPass is one run over the stores that only grow.
type retentionPass struct {
	ctx    context.Context
	c      *crew.Crew
	dir    string
	now    time.Time
	cutoff time.Time
	dry    bool

	moved    map[string]int // by what: handoffs, results, ... counted in directories, files or lines; tick.log and courier.log 1 when cut
	said     []string       // each thing moved (or, dry, that would be)
	problems []string       // what went wrong, and what was left out and why
	stopped  bool           // the deadline came

	tasks      map[string]task.Task
	unreadable int
	protected  map[string]map[string]bool // results/checkpoints directory -> task ids something open still points at
	state      []byte                     // state.json as it is, to see whether it still names a file; nil: could not read
}

func (p *retentionPass) run() {
	p.readTasks()
	p.readState()

	steps := []func(){
		func() { p.taskDirs(handoff.PacketsDir, false) },
		func() { p.taskDirs("results", true) },
		func() { p.taskDirs("checkpoints", true) },
		func() { p.requests(handoff.ReleasesDir) },
		func() { p.requests(handoff.RestartsDir) },
		p.messages,
		p.deliveryCards,
		p.reads,
		func() { p.cutLog("tick.log", p.c.Layout().TickLog()) },
		func() { p.cutLog("courier.log", p.c.Layout().CourierLog()) },
	}

	for _, step := range steps {
		if p.ctx.Err() != nil {
			p.stopped = true

			break
		}

		step()
	}

	if p.stopped {
		p.problems = append(p.problems, fmt.Sprintf("retention: stopped at its %s deadline, the rest waits for the next pass",
			retentionTimeout))
	}
}

// summary is the one line of what the pass moved: the event's detail and the ticker's line.
func (p *retentionPass) summary() string {
	if len(p.moved) == 0 {
		if p.dry {
			return "retention: nothing to move"
		}

		return "retention: nothing to move (older than " + days(p.c.RetentionSpan()) + ")"
	}

	verb := "moved"
	if p.dry {
		verb = "would move"
	}

	return "retention: " + verb + " " + movedText(p.moved)
}

// movedText names what a pass moved: handoffs 12, results 3, reads.jsonl 340 lines, tick.log cut.
func movedText(moved map[string]int) string {
	var parts []string

	for _, k := range slices.Sorted(maps.Keys(moved)) {
		switch k {
		case "tick.log", "courier.log":
			parts = append(parts, k+" cut")
		case "reads.jsonl":
			parts = append(parts, fmt.Sprintf("reads.jsonl %d lines", moved[k]))
		default:
			parts = append(parts, fmt.Sprintf("%s %d", k, moved[k]))
		}
	}

	return strings.Join(parts, ", ")
}

// readTasks reads the ledger: which tasks are closed and when, and the records the others still point at.
func (p *retentionPass) readTasks() {
	store := taskStore(p.c)

	tasks, _, err := store.Scan()
	if err != nil {
		p.problems = append(p.problems, "retention: tasks: "+err.Error())

		return
	}

	bad, err := store.Unreadable()
	if err != nil {
		p.problems = append(p.problems, "retention: tasks: "+err.Error())

		return
	}

	p.unreadable = len(bad)
	p.tasks = map[string]task.Task{}
	p.protected = map[string]map[string]bool{"results": {}, "checkpoints": {}}

	for _, t := range tasks {
		p.tasks[t.ID] = t

		if p.closedBefore(t) {
			continue
		}

		// A task that is open, waiting, parked or too recently closed keeps what it points at, wherever it lies: its
		// own records, and the ones it was carried from.
		for _, id := range t.RecordSources("result") {
			p.protected["results"][id] = true
		}

		for _, id := range t.RecordSources("checkpoint") {
			p.protected["checkpoints"][id] = true
		}
	}
}

// closedBefore reports whether t was closed (done or dropped; a parked task is not closed, it comes back) before the
// cutoff.
func (p *retentionPass) closedBefore(t task.Task) bool {
	if t.State != task.Done && t.State != task.Dropped {
		return false
	}

	at := t.Updated
	if t.Closed != nil && t.Closed.At.After(at) {
		at = t.Closed.At
	}

	return !at.IsZero() && at.Before(p.cutoff)
}

func (p *retentionPass) readState() {
	if raw, err := os.ReadFile(p.c.Layout().State()); err == nil {
		p.state = raw
	}
}

// archiveRoot is where this pass puts what it moves: archive/retention/<yyyy-mm>/ in the crew's directory.
func (p *retentionPass) archiveRoot() string {
	return filepath.Join(p.dir, record.ArchiveRoot, p.now.UTC().Format("2006-01"))
}

func (p *retentionPass) say(format string, args ...any) {
	p.said = append(p.said, fmt.Sprintf(format, args...))
}

// taskDirs archives the directories named by task of a store (handoffs/T-n, results/T-n, checkpoints/T-n) whose task
// was closed before the cutoff, or that no task names and whose files are all older than it. A directory a task
// that is not closed points at stays, and so does the one of a task whose file does not read. With stub, a note
// saying where the files went is left in the directory they came from.
func (p *retentionPass) taskDirs(kind string, stub bool) {
	if p.tasks == nil {
		return // the ledger did not read, and readTasks said so
	}

	if len(p.tasks) == 0 && p.unreadable == 0 {
		return // no ledger to tell closed from open by
	}

	if p.protected[kind] != nil && p.unreadable > 0 {
		p.problems = append(p.problems, fmt.Sprintf("retention: %s left alone: %d task files do not read, and any of them may point at one",
			kind, p.unreadable))

		return
	}

	root := filepath.Join(p.dir, kind)

	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.problems = append(p.problems, "retention: "+kind+": "+err.Error())
		}

		return
	}

	for _, e := range entries {
		if p.ctx.Err() != nil {
			p.stopped = true

			return
		}

		id := e.Name()
		if !e.IsDir() || !taskDirName.MatchString(id) || p.protected[kind][id] {
			continue
		}

		src := filepath.Join(root, id)

		files, newest := recordFiles(src)
		if len(files) == 0 {
			continue
		}

		why, ok := p.finishedTask(id, newest)
		if !ok {
			continue
		}

		p.say("%s/%s (%d files, %s)", kind, id, len(files), why)

		if p.dry {
			p.moved[kind]++

			continue
		}

		dst := filepath.Join(p.archiveRoot(), kind, id)
		if err := moveFiles(src, dst, files); err != nil {
			p.problems = append(p.problems, fmt.Sprintf("retention: %s/%s: %v", kind, id, err))

			continue
		}

		p.moved[kind]++

		if stub {
			note := fmt.Sprintf("Moved by zmina retention on %s to %s; zmina reads them there.\n",
				p.now.UTC().Format("2006-01-02"), filepath.ToSlash(relTo(p.dir, dst)))
			_ = fsx.WriteFile(filepath.Join(src, record.StubFile), []byte(note), 0o600)
		}
	}
}

// finishedTask says whether the files of task id (the newest of them last written at newest) are finished with.
func (p *retentionPass) finishedTask(id string, newest time.Time) (string, bool) {
	if t, ok := p.tasks[id]; ok {
		if !p.closedBefore(t) {
			return "", false
		}

		return "task " + string(t.State) + " " + t.Updated.Format("2006-01-02"), true
	}

	if newest.Before(p.cutoff) {
		return "no such task, files from " + newest.Format("2006-01-02"), true
	}

	return "", false
}

// recordFiles are the .json files in dir and when the newest of them was written.
func recordFiles(dir string) ([]string, time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, time.Time{}
	}

	var (
		files  []string
		newest time.Time
	)

	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		files = append(files, e.Name())

		if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}

	return files, newest
}

// moveFiles renames the named files of src into dst, which is made; a file already there is replaced (the same
// content, or an older copy of it).
func moveFiles(src, dst string, files []string) error {
	if err := fsx.MkdirAll(dst, 0o700); err != nil {
		return err
	}

	for _, f := range files {
		if err := os.Rename(filepath.Join(src, f), filepath.Join(dst, f)); err != nil {
			return err
		}
	}

	return errors.Join(fsx.SyncDir(dst), fsx.SyncDir(src))
}

func relTo(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil {
		return rel
	}

	return path
}

// requests archives the release and restart requests (files of their own, not tied to a task) older than the
// cutoff, unless state.json still names one.
func (p *retentionPass) requests(kind string) {
	root := filepath.Join(p.dir, kind)

	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.problems = append(p.problems, "retention: "+kind+": "+err.Error())
		}

		return
	}

	if p.state == nil {
		p.problems = append(p.problems, "retention: "+kind+" left alone: state.json did not read")

		return
	}

	for _, e := range entries {
		if p.ctx.Err() != nil {
			p.stopped = true

			return
		}

		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			continue
		}

		info, err := e.Info()
		if err != nil || !info.ModTime().Before(p.cutoff) {
			continue
		}

		if bytes.Contains(p.state, []byte(strings.TrimSuffix(name, ".json"))) {
			continue // a queued or typed line of a role still names it
		}

		p.say("%s/%s (from %s)", kind, name, info.ModTime().Format("2006-01-02"))

		if !p.dry {
			if err := moveFiles(root, filepath.Join(p.archiveRoot(), kind), []string{name}); err != nil {
				p.problems = append(p.problems, fmt.Sprintf("retention: %s/%s: %v", kind, name, err))

				continue
			}
		}

		p.moved[kind]++
	}
}

// messages archives the report records that are finished (read by their destination or superseded, everything
// queued) and untouched since the cutoff.
func (p *retentionPass) messages() {
	store := message.Store{Dir: filepath.Join(p.dir, "messages")}

	ids, err := store.Finished(p.cutoff)
	if err != nil {
		p.problems = append(p.problems, "retention: messages: "+err.Error())

		return
	}

	for _, id := range ids {
		if p.ctx.Err() != nil {
			p.stopped = true

			return
		}

		p.say("messages/%s", id)

		if !p.dry {
			if err := store.Archive(id); err != nil {
				p.problems = append(p.problems, fmt.Sprintf("retention: messages/%s: %v", id, err))

				continue
			}
		}

		p.moved["messages"]++
	}
}

// reads archives the read meter's lines from before the cutoff.
func (p *retentionPass) reads() {
	path := p.c.Layout().Reads()

	older, total, err := lean.CountOlder(path, p.cutoff)
	if err != nil {
		p.problems = append(p.problems, "retention: reads.jsonl: "+err.Error())

		return
	}

	if older == 0 {
		return
	}

	p.say("reads.jsonl (%d of %d lines older than %s)", older, total, p.cutoff.Format("2006-01-02"))

	if p.dry {
		p.moved["reads.jsonl"] = older

		return
	}

	moved, err := lean.ArchiveOlder(path, filepath.Join(p.archiveRoot(), "reads.jsonl"), p.cutoff)
	if err != nil {
		p.problems = append(p.problems, "retention: reads.jsonl: "+err.Error())

		return
	}

	if moved > 0 {
		p.moved["reads.jsonl"] = moved
	}
}

// deliveryCards drops the delivery cards past their keep and over the cap from the files of roles that no delivery has
// written to lately (a write prunes too), the card files moved aside as corrupt once they are over a week old, and the
// file of a role that is no longer in the crew once every card in it is past its keep. The delivery writes those files
// under the state lock, so the pass takes it, without waiting: a lock another zmina holds leaves them to tomorrow's
// pass. Nothing of the state is read or written.
func (p *retentionPass) deliveryCards() {
	if !p.dry {
		l, err := shift.TryLockAs(p.dir, shift.LockOpts{Verb: "retention"})
		if errors.Is(err, shift.ErrLockHeld) {
			return
		}

		if err != nil {
			p.problems = append(p.problems, "retention: delivery cards: "+err.Error())

			return
		}

		defer l.Release()
	}

	// A copy of a role (dev#2) is in the crew while its role is.
	inCrew := func(role string) bool {
		base, _, _ := shift.SplitInstance(role)
		_, ok := p.c.Roles[base]

		return ok
	}

	n, err := shift.PruneCards(p.c.Layout(), p.now, inCrew, p.dry)
	if err != nil {
		p.problems = append(p.problems, "retention: delivery cards: "+err.Error())
	}

	if n > 0 {
		p.say("delivery cards (%d cards, fetches or set-aside files past their keep or over the cap)", n)
		p.moved["delivery cards"] = n
	}
}

// cutLog cuts a service's log (tick.log, courier.log) to its last tickLogKeep bytes once it passes tickLogMax. The
// service's output goes there through its own append-mode file, so the file is cut in place, not replaced.
func (p *retentionPass) cutLog(name, path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= tickLogMax {
		return
	}

	p.say("%s (%s, keeps its last %s)", name, sizeText(info.Size()), sizeText(tickLogKeep))

	if p.dry {
		p.moved[name] = 1

		return
	}

	if err := trimTail(path, tickLogKeep, "[zmina retention "+p.now.UTC().Format("2006-01-02")+": older lines of this log were cut]\n"); err != nil {
		p.problems = append(p.problems, "retention: "+name+": "+err.Error())

		return
	}

	p.moved[name] = 1
}

// trimTail leaves of the file at path its last keep bytes from a line start, after the note line. The file is opened
// for appending and cut in place: whoever holds it open for writing (the ticker's own output) goes on appending to the
// same file, and a line written between the read and the cut lands after the tail it was meant to follow.
func trimTail(path string, keep int64, note string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	off := max(info.Size()-keep, 0)
	tail := make([]byte, info.Size()-off)

	if _, err := f.ReadAt(tail, off); err != nil {
		return err
	}

	if off > 0 {
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		}
	}

	if err := f.Truncate(0); err != nil {
		return err
	}

	_, err = f.Write(append([]byte(note), tail...))

	return err
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}

	return fmt.Sprintf("%d B", n)
}

func (a *app) retentionCommand() *cobra.Command {
	var dry bool

	c := &cobra.Command{
		Use:   "retention",
		Short: "Move finished work older than the retention span to archive/, as the ticker does once a day (--dry-run lists it)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.loadCrew()
			if err != nil {
				return err
			}

			lines, err := a.retentionRun(c, time.Now(), by(), true, dry)
			for _, l := range lines {
				fmt.Fprintln(a.out, l)
			}

			return err
		},
	}

	c.Flags().BoolVar(&dry, "dry-run", false, "list what would be moved and move nothing")

	return c
}
