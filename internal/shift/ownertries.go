package shift

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// ownerTry is the failed tries of one queued line that is not the head of its role's queue: the owner's own line, taken through
// a hold (deliverInboxOnce). The tries the role's state counts (Attempts, TriedAt) are the head's, and the head's they stay, so
// this line's are kept by the line itself, in a small file beside the delivery cards (delivery/<role>.owner-tries) and not in
// state.json: a field there would move the state's schema for a record that only says when a line may be tried again. The
// same spacing (countedGap) and the same count (typeTries, then the failed list) apply to it as to a head line, since the line
// is tried as one: its tries are put in the counters while it is the head, and taken out again when it is not.
type ownerTry struct {
	// Line is the line these tries are of; a record of another line, or of one no longer queued, is no one's tries.
	Line     string    `json:"line"`
	Attempts int       `json:"attempts"`
	TriedAt  time.Time `json:"tried_at"`
}

// ownerTried is the tries counted for the owner's line of a role: none for a line another record is of, or that is no longer
// queued. It only reads, so a look (Waiting, which the courier asks without the lock) may use it; a record that no line wants
// is removed by the delivery, which writes (setOwnerTried).
func (s *Shift) ownerTried(name, line string) (attempts int, at time.Time) {
	if !hookRole(name) {
		return 0, time.Time{}
	}

	rs, ok := s.State.Roles[name]
	if !ok {
		return 0, time.Time{}
	}

	var rec ownerTry
	if !readRecord(s.Crew.Layout().DeliveryOwnerTries(name), &rec) || rec.Line != line || !slices.Contains(rs.Inbox, line) {
		return 0, time.Time{}
	}

	return rec.Attempts, rec.TriedAt
}

// ownerTriedLately reports whether the owner's line failed less than countedGap ago, as triedLately does for a head line.
func (s *Shift) ownerTriedLately(name, line string, now time.Time) bool {
	attempts, at := s.ownerTried(name, line)

	return attempts > 0 && within(now, at, countedGap)
}

// setOwnerTried keeps the tries counted for the owner's line of a role; none removes the record. A record that cannot be written
// is a failed try that is forgotten (the line is tried again at the next pass, which is the old behaviour) and is not an error
// of the delivery.
func (s *Shift) setOwnerTried(name, line string, attempts int, at time.Time) {
	if !hookRole(name) {
		return
	}

	l := s.Crew.Layout()
	path := l.DeliveryOwnerTries(name)

	if attempts <= 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && s.Warn != nil {
			s.Warn(err)
		}

		return
	}

	data, err := json.Marshal(ownerTry{Line: line, Attempts: attempts, TriedAt: at.UTC()})
	if err == nil {
		err = fsx.MkdirAll(l.Delivery(), 0o700)
	}

	if err == nil {
		err = fsx.WriteFile(path, append(data, '\n'), 0o600)
	}

	if err != nil && s.Warn != nil {
		s.Warn(err)
	}
}
