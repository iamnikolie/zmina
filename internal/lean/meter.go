package lean

import (
	"encoding/json"
	"os"
	"time"
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

// Append adds a record to the file; a meter that cannot write stays silent (it never fails a read).
func Append(path string, r Record) {
	if path == "" {
		return
	}

	raw, err := json.Marshal(r)
	if err != nil {
		return
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}

	_, _ = f.Write(append(raw, '\n'))
	_ = f.Close()
}
