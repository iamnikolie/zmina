package shift

import (
	"slices"
	"time"
)

// RotationView is a role's newest rotation as a look shows it (zmina ls, the cockpit): the one that is open, else how the last one
// ended. It is read from the role's record on every look and is no part of the state; a compact or a clear is no rotation to
// look at, and a role whose record is missing or does not read has none.
type RotationView struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Open says the rotation is not closed; Phase is then where it stands (prepared, switching, started), and At since when
	// (its start, else when it was prepared). A closed one has the Outcome (resumed, rebuilt, lost), At the time it closed,
	// and Then what became of a lost one: "rebuilt" (its conversation accepted its tasks late) or "aborted" (the role was put back
	// on its old conversation).
	Open    bool      `json:"open,omitempty"`
	Phase   string    `json:"phase,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Then    string    `json:"then,omitempty"`
	At      time.Time `json:"at"`
}

// rotationView is the role's rotation for a look: its newest that is not a compact or a clear, from the end of its record, read
// once by the beat's cached reader (rotationsOf); nothing is written.
func (s *Shift) rotationView(name string) *RotationView {
	rots := slices.DeleteFunc(slices.Clone(s.rotationsOf(name)), Rotation.Trim)
	if len(rots) == 0 {
		return nil
	}

	r := rots[len(rots)-1]
	v := &RotationView{ID: r.ID, Kind: r.Kind, Open: r.Open(), Phase: r.Phase}

	if r.Open() {
		v.At = r.Prepared
		if !r.Started.IsZero() {
			v.At = r.Started
		}

		return v
	}

	v.Phase, v.Outcome, v.At = "", r.Closed.Outcome, r.Closed.At

	switch {
	case r.LateRebuilt():
		v.Then = "rebuilt"
	case r.Closed.Outcome == OutcomeLost && r.Aborted() != nil:
		v.Then = "aborted"
	}

	return v
}
