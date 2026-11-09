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
