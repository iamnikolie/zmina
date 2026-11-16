package move

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
)

// Taking is the marker the node a role goes to keeps of a take of a move, from before the take's first side effect until it has the
// receipt (moves/<role>.taking.json). While it is there the take runs, or it was cut short; either way the move is not called off
// from the other node (the abort asks this node first, which answers "taking"), and the next take of the move, or the abort, takes
// away what the first made before it does anything else.
type Taking struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	From    string    `json:"from"`
	Started time.Time `json:"started"`
	// Origins are the origins (<node>/T-<n>) of the tasks the take makes: the tasks it made are found by them (with the role, the
	// move's id in the note, and an active state). Repos are the repositories, as this node's absolute paths, it keeps refs in.
	Origins []string `json:"origins,omitempty"`
	Repos   []string `json:"repos,omitempty"`
}

// OpenTaking reads the marker of a take of the role's move, false when there is none. A marker that does not read is an error: the
// move is not called off, nor a take made, until someone has looked.
func OpenTaking(l crew.Layout, role string) (Taking, bool, error) {
	var t Taking

	raw, err := os.ReadFile(l.MoveTaking(role))
	if errors.Is(err, os.ErrNotExist) {
		return t, false, nil
	}

	if err != nil {
		return t, false, err
	}

	if err := json.Unmarshal(raw, &t); err != nil {
		return Taking{}, false, fmt.Errorf("the marker of a take of %s does not read: %w", role, err)
	}

	return t, true, nil
}

// SaveTaking writes the marker.
func SaveTaking(l crew.Layout, t Taking) error {
	if err := fsx.MkdirAll(l.Moves(), 0o700); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}

	return fsx.WriteFile(l.MoveTaking(t.Role), append(raw, '\n'), 0o600)
}

// CloseTaking takes the marker away: the take has its receipt, or what it made is taken away.
func CloseTaking(l crew.Layout, role string) error {
	if err := os.Remove(l.MoveTaking(role)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// Tombstone is what a node keeps of a move that was called off, under its id (moves/aborted/<id>.json): a take of that move is
// refused, before it does anything and again before the role is placed.
type Tombstone struct {
	ID   string    `json:"id"`
	Role string    `json:"role"`
	At   time.Time `json:"at"`
}

// Aborted reports whether the move with this id was called off at this node.
func Aborted(l crew.Layout, id string) (bool, error) {
	if !ValidID(id) {
		return false, fmt.Errorf("move id %q", id)
	}

	_, err := os.Stat(l.MoveAborted(id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

// SaveAborted leaves the tombstone of the move.
func SaveAborted(l crew.Layout, t Tombstone) error {
	if !ValidID(t.ID) {
		return fmt.Errorf("move id %q", t.ID)
	}

	if err := fsx.MkdirAll(filepath.Dir(l.MoveAborted(t.ID)), 0o700); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}

	return fsx.WriteFile(l.MoveAborted(t.ID), append(raw, '\n'), 0o600)
}

// ErrBusy is the refusal of a lock another step of the move holds.
var ErrBusy = errors.New("another step of this move is running on this node")

// Lock takes the role's move lock (moves/<role>.lock): of the steps of a move on one node, only one runs at a time, so that a slow
// poll cannot write the journal after a commit closed it, two beats cannot hand the same branch over twice, and an abort never
// meets a take half-way. It waits up to wait for the lock (none: it is tried once) and returns ErrBusy when it is not free then. The
// returned func gives it back; the lock is the process's, so it is free again when the process dies.
func Lock(l crew.Layout, role string, wait time.Duration) (func(), error) {
	if err := fsx.MkdirAll(l.Moves(), 0o700); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(l.MoveLock(role), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(wait)

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()

			return nil, err
		}

		if !time.Now().Before(deadline) {
			_ = f.Close()

			return nil, ErrBusy
		}

		time.Sleep(20 * time.Millisecond)
	}
}
