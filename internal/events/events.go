// Package events is the shift's history: one JSON line per thing that happened, in events.jsonl next to the crew
// file. It is written by every command and the ticker, and read by 'zmina log'.
package events

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Event is one line of the log.
type Event struct {
	TS time.Time `json:"ts"`
	// Role is the role (or instance, dev#2) the event is about; empty for crew-wide events.
	Role string `json:"role,omitempty"`
	// Event is what happened: up, down, ended, stuck, idle, crash-loop, told, sent, queued, keys, task, seat, route, ...
	Event string `json:"event"`
	// Task is the task id the event concerns, when there is one.
	Task string `json:"task,omitempty"`
	// By is who did it: hand (the owner's terminal), ticker, schedule, or the role that ran the command.
	By     string `json:"by,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Picked, Rule, RouteSaid and Because are the fields of a route event, one for every task made while the crew file
	// has routes: the seat the task went to (also Role), the rule of the routes' answer (its number, 0 or 2 to 6; unset
	// when no rule matched), the seat the routes named ("" when none, or another crew's <node>/<crew>), and why it was
	// picked by hand (task new --because).
	Picked    string `json:"picked,omitempty"`
	Rule      *int   `json:"rule,omitempty"`
	RouteSaid string `json:"route_said,omitempty"`
	Because   string `json:"because,omitempty"`
}

// MaxSize is the size at which the log is rotated: events.jsonl becomes events.jsonl.1, the older rotated files
// each one number up, and the oldest goes.
var MaxSize int64 = 10 << 20

// Generations is how many rotated files are kept next to the current one (events.jsonl.1 is the newest of them,
// events.jsonl.<Generations> the oldest); the one that would be next is dropped.
var Generations = 5

// rotateWait is how long a process that finds the log full waits for another that is rotating it before it gives up
// and only appends; the next event rotates.
const rotateWait = 250 * time.Millisecond

// Log appends to and reads one events file and its rotated predecessors.
type Log struct {
	Path string
}

// Add appends one event. A failure is returned but never stops the command that logs.
//
// An append is one write to a file opened for appending, so concurrent writers never split a line, and a line goes
// where the file it was opened on is, whatever name that file has by then: rotation cannot lose it. Only the
// rotation itself is serialised, by a lock file, and the full log is looked at again once the lock is held.
func (l *Log) Add(e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}

	var rotErr error

	if fi, err := os.Stat(l.Path); err == nil && fi.Size() >= MaxSize {
		rotErr = l.rotate()
	}

	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return errors.Join(rotErr, fmt.Errorf("events: %w", err))
	}

	_, err = f.Write(append(raw, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	return errors.Join(rotErr, err)
}

// rotate moves the full log to .1 under a lock, shifting the older ones up and dropping the oldest. Whoever finds
// the lock taken for rotateWait leaves the rotation to the holder.
func (l *Log) rotate() error {
	lock, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("events: rotate: %w", err)
	}

	defer func() { _ = lock.Close() }()

	deadline := time.Now().Add(rotateWait)

	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("events: rotate: %w", err)
		}

		if time.Now().After(deadline) {
			return nil
		}

		time.Sleep(5 * time.Millisecond)
	}

	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	// Another process may have rotated while this one waited.
	if fi, err := os.Stat(l.Path); err != nil || fi.Size() < MaxSize {
		return nil
	}

	keep := max(Generations, 1)

	for _, p := range l.rotated() {
		if p.n > keep {
			_ = os.Remove(p.path)
		}
	}

	for n := keep - 1; n >= 1; n-- {
		if err := os.Rename(l.numbered(n), l.numbered(n+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("events: rotate: %w", err)
		}
	}

	if err := os.Rename(l.Path, l.numbered(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("events: rotate: %w", err)
	}

	return nil
}

func (l *Log) numbered(n int) string { return l.Path + "." + strconv.Itoa(n) }

// rotated lists the rotated files that exist, oldest (highest number) first.
func (l *Log) rotated() []rotatedFile {
	paths, _ := filepath.Glob(l.Path + ".*")

	var out []rotatedFile

	for _, p := range paths {
		if n, err := strconv.Atoi(strings.TrimPrefix(p, l.Path+".")); err == nil && n >= 1 {
			out = append(out, rotatedFile{p, n})
		}
	}

	slices.SortFunc(out, func(a, b rotatedFile) int { return b.n - a.n })

	return out
}

type rotatedFile struct {
	path string
	n    int
}

// Filter picks events to read.
type Filter struct {
	Role  string
	Task  string
	Since time.Time
	// Last keeps only the newest n events (0: all).
	Last int
}

// Read returns the events matching f, oldest first, from the rotated files and the current one. Lines that do not
// parse are skipped.
func (l *Log) Read(f Filter) ([]Event, error) {
	var out []Event

	var paths []string

	for _, p := range l.rotated() {
		paths = append(paths, p.path)
	}

	for _, path := range append(paths, l.Path) {
		evs, err := readFile(path, f)
		if err != nil {
			return nil, err
		}

		out = append(out, evs...)
	}

	if f.Last > 0 && len(out) > f.Last {
		out = out[len(out)-f.Last:]
	}

	return out, nil
}

func readFile(path string, f Filter) ([]Event, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	defer func() { _ = file.Close() }()

	var out []Event

	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}

		if (f.Role != "" && e.Role != f.Role) || (f.Task != "" && e.Task != f.Task) || e.TS.Before(f.Since) {
			continue
		}

		out = append(out, e)
	}

	return out, sc.Err()
}
