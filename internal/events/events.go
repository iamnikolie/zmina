// Package events is the shift's history: one JSON line per thing that happened, in events.jsonl next to the crew
// file. It is written by every command and the ticker, and read by 'zmina log'.
package events

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Event is one line of the log.
type Event struct {
	TS time.Time `json:"ts"`
	// Role is the role (or instance, dev#2) the event is about; empty for crew-wide events.
	Role string `json:"role,omitempty"`
	// Event is what happened: up, down, ended, stuck, idle, crash-loop, told, sent, queued, keys, task, seat, ...
	Event string `json:"event"`
	// Task is the task id the event concerns, when there is one.
	Task string `json:"task,omitempty"`
	// By is who did it: hand (the owner's terminal), ticker, schedule, or the role that ran the command.
	By     string `json:"by,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// MaxSize is the size at which the log is rotated: events.jsonl becomes events.jsonl.1 (replacing the older one).
var MaxSize int64 = 10 << 20

// Log appends to and reads one events file and its one rotated predecessor.
type Log struct {
	Path string
}

// Add appends one event. A failure is returned but never stops the command that logs.
func (l *Log) Add(e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}

	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}

	if fi, err := os.Stat(l.Path); err == nil && fi.Size() >= MaxSize {
		if err := os.Rename(l.Path, l.Path+".1"); err != nil {
			return fmt.Errorf("events: rotate: %w", err)
		}
	}

	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}

	_, err = f.Write(append(raw, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	return err
}

// Filter picks events to read.
type Filter struct {
	Role  string
	Task  string
	Since time.Time
	// Last keeps only the newest n events (0: all).
	Last int
}

// Read returns the events matching f, oldest first, from the rotated file and the current one. Lines that do not
// parse are skipped.
func (l *Log) Read(f Filter) ([]Event, error) {
	var out []Event

	for _, path := range []string{l.Path + ".1", l.Path} {
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
