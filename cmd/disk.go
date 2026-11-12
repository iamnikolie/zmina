package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/shift"
)

// A full disk does not stop zmina, it stops what writes to it: Claude Code leaves empty or cut-short session files and
// a role then shows as "up unknown". Doctor and the beat look at the free space of the filesystem that holds the crew's
// directory and of the one that holds the Claude Code config (the namespace), and say so before it is gone.

const (
	// A filesystem is low under the smaller of an amount and a share of its size: the amount so that a big disk is not
	// called low at a tenth free (that is hundreds of GiB), the share so that a small one (a 4 GiB volume) is not low
	// from the first byte. Warn under 5 GiB or 10%, fail (and tell the dispatcher) under 1 GiB or 2%.
	diskWarnBytes uint64 = 5 << 30
	diskFailBytes uint64 = 1 << 30
	diskWarnShare uint64 = 10 // percent
	diskFailShare uint64 = 2  // percent

	// diskNoteEvery is how often the dispatcher hears again of the same filesystem while it stays under the fail line.
	diskNoteEvery = 6 * time.Hour
)

// diskSpace is what a filesystem has: the bytes free to an ordinary user and its whole size. ID names the filesystem
// (two paths with the same ID are one), empty when the system does not say.
type diskSpace struct {
	Free, Total uint64
	ID          string
}

// diskFree reads a path's filesystem; a test replaces it, so no check needs a disk of its own state.
var diskFree = statDisk

// diskLimits are the free-space lines of a filesystem of total bytes (0: not known, only the amounts count).
func diskLimits(total uint64) (warn, fail uint64) {
	warn, fail = diskWarnBytes, diskFailBytes

	if total > 0 {
		warn = min(warn, shareOf(total, diskWarnShare))
		fail = min(fail, shareOf(total, diskFailShare))
	}

	return warn, fail
}

// shareOf is percent of total, without losing the bytes a division by 100 drops first.
func shareOf(total uint64, percent uint64) uint64 { return total/100*percent + total%100*percent/100 }

// diskLevel is ok, warn or fail for free bytes of a filesystem of total bytes: under a line, not at it.
func diskLevel(free, total uint64) string {
	warn, fail := diskLimits(total)

	switch {
	case free < fail:
		return "fail"
	case free < warn:
		return "warn"
	default:
		return "ok"
	}
}

// diskSize is a byte count the way a person reads it: 640 MiB, 4.2 GiB.
func diskSize(n uint64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	size, suffix := float64(n), ""

	for _, s := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		size /= unit
		suffix = s

		if size < unit {
			break
		}
	}

	if size >= 100 {
		return fmt.Sprintf("%.0f %s", size, suffix)
	}

	return fmt.Sprintf("%.1f %s", size, suffix)
}

// diskReading is one filesystem as doctor and the beat see it.
type diskReading struct {
	Path  string // a directory on it
	What  string // what lives there: the crew's directory, the Claude Code config, or both
	Space diskSpace
	Level string
	Err   error // the free space could not be read
}

// diskReadings measures the filesystem of the crew's directory and, when a Claude role runs, the one of its config
// directory (the namespace); one filesystem that holds both is one reading.
func diskReadings(c *crew.Crew, claude bool) []diskReading {
	home := diskReading{Path: c.StateDir(), What: "zmina home"}
	home.Space, home.Err = diskFree(home.Path)

	out := []diskReading{home}

	if !claude || c.Namespace == "" {
		return withLevels(out)
	}

	conf := diskReading{Path: c.Namespace, What: "Claude Code config"}
	conf.Space, conf.Err = diskFree(conf.Path)

	if home.Err == nil && conf.Err == nil && home.Space.ID != "" && home.Space.ID == conf.Space.ID {
		out[0].What = "zmina home and Claude Code config " + conf.Path

		return withLevels(out)
	}

	return withLevels(append(out, conf))
}

func withLevels(rs []diskReading) []diskReading {
	for i := range rs {
		if rs[i].Err == nil {
			rs[i].Level = diskLevel(rs[i].Space.Free, rs[i].Space.Total)
		}
	}

	return rs
}

// hasClaudeRole reports whether a role of the crew runs Claude Code.
func hasClaudeRole(c *crew.Crew) bool {
	for _, r := range c.Roles {
		if !r.IsCodex() {
			return true
		}
	}

	return false
}

// text is the reading as doctor says it: what, where, how much is left and, past a line, which one and what follows.
func (r diskReading) text() string {
	if r.Err != nil {
		return fmt.Sprintf("cannot read the free space under %s (%s): %v", r.Path, r.What, r.Err)
	}

	out := fmt.Sprintf("%s (%s): %s free of %s", r.Path, r.What, diskSize(r.Space.Free), diskSize(r.Space.Total))
	warn, fail := diskLimits(r.Space.Total)

	switch r.Level {
	case "warn":
		out += fmt.Sprintf(" · under %s: free some space before it fills", diskSize(warn))
	case "fail":
		out += fmt.Sprintf(" · under %s: files written now can come out empty or cut short (a role's session file, "+
			"the state); free space, then restart a role that shows up unknown", diskSize(fail))
	}

	return out
}

// checkDisk is doctor's look at the free space (see diskReadings), before the checks a full disk would explain.
func checkDisk(c *crew.Crew, add func(name, level, format string, args ...any)) {
	for _, r := range diskReadings(c, hasClaudeRole(c)) {
		level := r.Level
		if r.Err != nil {
			level = "warn"
		}

		add("disk", level, "%s", r.text())
	}
}

// diskTold is disk.json next to the crew file: when the dispatcher was last told of each filesystem that is under the
// fail line, by the path that was measured. Its own small file, like versions.json, not a field of the state.
type diskTold map[string]time.Time

func diskToldPath(dir string) string { return filepath.Join(dir, "disk.json") }

func readDiskTold(dir string) diskTold {
	told := diskTold{}

	if raw, err := os.ReadFile(diskToldPath(dir)); err == nil {
		_ = json.Unmarshal(raw, &told)
	}

	if told == nil {
		told = diskTold{}
	}

	return told
}

func writeDiskTold(dir string, told diskTold) error {
	raw, err := json.Marshal(told)
	if err != nil {
		return err
	}

	return fsx.WriteFile(diskToldPath(dir), append(raw, '\n'), 0o600)
}

// diskMemo is what this process told, for when disk.json cannot be written (the disk is full): the beat then says it
// at most once in this process rather than at every try, and does not loop.
var diskMemo = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

func diskMemoTold(path string, now time.Time) bool {
	diskMemo.Lock()
	defer diskMemo.Unlock()

	at, ok := diskMemo.at[path]

	return ok && now.Sub(at) < diskNoteEvery && !at.After(now)
}

func diskMemoSet(path string, now time.Time) {
	diskMemo.Lock()
	defer diskMemo.Unlock()

	diskMemo.at[path] = now
}

// diskDue is the filesystems under the fail line that the dispatcher has not heard of within diskNoteEvery, and the
// record to keep: what was told still within the span, and now for each of these. A record older than the span is
// dropped, so it never grows.
func diskDue(low []diskReading, told diskTold, now time.Time) (due []diskReading, next diskTold) {
	next = diskTold{}

	for path, at := range told {
		if now.Sub(at) < diskNoteEvery && !at.After(now) {
			next[path] = at
		}
	}

	for _, r := range low {
		if _, was := next[r.Path]; was || diskMemoTold(r.Path, now) {
			continue
		}

		due = append(due, r)
		next[r.Path] = now
	}

	return due, next
}

// diskNote is the health note for a filesystem under the fail line.
func diskNote(r diskReading) string {
	_, fail := diskLimits(r.Space.Total)

	return fmt.Sprintf("disk: the filesystem of %s (%s) has %s free of %s, under %s: files written now can come out "+
		"empty or cut short (a role's session file, the state); free space, then restart a role that shows up unknown "+
		"(zmina doctor names it)", r.Path, r.What, diskSize(r.Space.Free), diskSize(r.Space.Total), diskSize(fail))
}

// diskBeat tells the dispatcher when a filesystem of the crew is under the fail line: which one and how much is left,
// not more than once in diskNoteEvery for the same one while it stays low (disk.json keeps when; no note when it
// recovers). It runs after the beat's locked phases; the note goes through the health-note queue like the other beats',
// and the owner reads it from the dispatcher.
//
// The note is queued first and recorded after: a note that could not be queued (the state cannot be written either
// when the disk is full) is tried again on the next beat. A record that cannot be written leaves the beat without a
// record: it says each filesystem once in this process, then returns, and never loops.
func (a *app) diskBeat(now time.Time) {
	c, err := crew.Load(a.crewPath())
	if err != nil {
		return
	}

	var low []diskReading

	for _, r := range diskReadings(c, hasClaudeRole(c)) {
		if r.Err == nil && r.Level == "fail" {
			low = append(low, r)
		}
	}

	dir := c.StateDir()
	told := readDiskTold(dir)

	due, next := diskDue(low, told, now)
	if len(due) == 0 {
		if !maps.EqualFunc(told, next, time.Time.Equal) {
			_ = writeDiskTold(dir, next)
		}

		return
	}

	notes := make([]string, 0, len(due))
	for _, r := range due {
		notes = append(notes, diskNote(r))
	}

	if err := a.withState(func(s *shift.Shift) error {
		for _, n := range notes {
			s.Log("", "disk-low", "", n)
		}

		s.AddNotes(notes)

		return nil
	}); err != nil {
		return
	}

	for _, r := range due {
		diskMemoSet(r.Path, now)
	}

	_ = writeDiskTold(dir, next)

	a.printTick(notes)
}
