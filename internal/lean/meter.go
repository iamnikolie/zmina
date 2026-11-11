package lean

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// Record is one line of the crew's reads.jsonl: a zmina read or grep call, or a guard's step. It is the trial's
// meter and the session memory's record (which file, which lines, of what size, in which session); the saving itself
// is measured from transcripts, never from these lines.
type Record struct {
	At      time.Time `json:"at"`
	Role    string    `json:"role,omitempty"`
	Session string    `json:"session,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Kind    string    `json:"kind"`           // read, grep, guard-rewrite, guard-outline
	What    string    `json:"what,omitempty"` // a guard: cat, cat-multi, sed, head, tail, Read; a read: whole, outline, symbol, range
	Path    string    `json:"path,omitempty"`
	From    int       `json:"from,omitempty"`
	To      int       `json:"to,omitempty"`
	Size    int       `json:"size,omitempty"` // the file's (or the replaced output's) characters
	MTime   int64     `json:"mtime,omitempty"`
	Out     int       `json:"out"` // characters printed
	// Bulk marks a call of a burst: more than BulkPerMinute in one session within a minute is a script sweeping
	// files, not an agent reading them; a measurement of what the reader saves leaves these out.
	Bulk bool `json:"bulk,omitempty"`
}

// BulkPerMinute is the most calls a session makes in a minute before the rest of that minute counts as bulk.
const BulkPerMinute = 30

// rate is one session's count in its current minute.
type rate struct {
	Start time.Time `json:"start"`
	N     int       `json:"n"`
}

// IsBulk counts a session's call in the rate file (one small JSON object for the crew) and says whether it is past
// BulkPerMinute in its minute. The read-modify-write takes no lock and writers share one temporary name, so hooks
// running at once can lose a count: good enough for a measurement mark, nothing more. Sessions not seen for an hour are dropped from the file. A file that cannot be read or
// written counts nothing: the meter never fails a read.
func IsBulk(path, session string, now time.Time) bool {
	if path == "" || session == "" {
		return false
	}

	rates := map[string]rate{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &rates)
	}

	for k, r := range rates {
		if now.Sub(r.Start) > time.Hour {
			delete(rates, k)
		}
	}

	r := rates[session]
	if now.Sub(r.Start) >= time.Minute || now.Before(r.Start) {
		r = rate{Start: now}
	}

	r.N++
	rates[session] = r

	if raw, err := json.Marshal(rates); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, raw, 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}

	return r.N > BulkPerMinute
}

// Append adds a record to the file; a meter that cannot write stays silent (it never fails a read). It waits a
// moment for the file's lock (path + ".lock", held while ArchiveOlder rewrites the file) and appends without it
// when that does not come: a late line is better than none.
func Append(path string, r Record) {
	if path == "" {
		return
	}

	raw, err := json.Marshal(r)
	if err != nil {
		return
	}

	if unlock := lockFile(path+".lock", appendWait); unlock != nil {
		defer unlock()
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}

	_, _ = f.Write(append(raw, '\n'))
	_ = f.Close()
}

// appendWait is how long a metered call waits for the meter file's lock; archiveWait how long the archiving of old
// lines does.
const (
	appendWait  = time.Second
	archiveWait = 10 * time.Second
)

// lockFile takes an exclusive lock on the file at path (made when it is not there), waiting up to wait. It returns
// the function that gives the lock up, or nil when the lock was not taken.
func lockFile(path string, wait time.Duration) func() {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}

	deadline := time.Now().Add(wait)

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()

			return nil
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// olderThan reports whether a line of the meter file is a record from before cutoff. A line that does not parse, or
// has no time, is not: it is kept.
func olderThan(line []byte, cutoff time.Time) bool {
	var r struct {
		At time.Time `json:"at"`
	}

	return json.Unmarshal(line, &r) == nil && !r.At.IsZero() && r.At.Before(cutoff)
}

// CountOlder is how many lines of the meter file are records from before cutoff, and how many lines it has. A file
// that is not there has none.
func CountOlder(path string, cutoff time.Time) (older, total int, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}

	if err != nil {
		return 0, 0, err
	}

	for line := range bytes.Lines(raw) {
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}

		total++

		if olderThan(line, cutoff) {
			older++
		}
	}

	return older, total, nil
}

// ArchiveOlder moves the records from before cutoff out of the meter file: they are appended to the file at archive
// and the meter file is rewritten with the rest, whole or not at all, while the file's lock is held, so a call
// metered at that moment lands in the rewritten file or waits for it. Lines that do not parse stay. The archive
// gets the lines first: a failure between the two steps leaves them in both files, never in neither.
func ArchiveOlder(path, archive string, cutoff time.Time) (moved int, err error) {
	unlock := lockFile(path+".lock", archiveWait)
	if unlock == nil {
		return 0, errors.New("the meter file is locked")
	}
	defer unlock()

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	defer func() { _ = f.Close() }()

	var gone, kept bytes.Buffer

	rd := bufio.NewReader(f)

	for {
		line, rerr := rd.ReadBytes('\n')
		if line = bytes.TrimRight(line, "\r\n"); len(bytes.TrimSpace(line)) > 0 {
			dst := &kept

			if olderThan(line, cutoff) {
				dst = &gone
				moved++
			}

			dst.Write(line)
			dst.WriteByte('\n')
		}

		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return 0, rerr
			}

			break
		}
	}

	if moved == 0 {
		return 0, nil
	}

	if err := fsx.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
		return 0, err
	}

	a, err := os.OpenFile(archive, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}

	_, err = a.Write(gone.Bytes())
	if err == nil {
		err = a.Sync()
	}

	if cerr := a.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return 0, err
	}

	return moved, fsx.WriteFile(path, kept.Bytes(), 0o600)
}
