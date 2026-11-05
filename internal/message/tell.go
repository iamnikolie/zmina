package message

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/iamnikolie/zmina/internal/mesh"
)

// TellStages are what the destination of a plain line reports back, in order: queued for its dispatcher, in the
// dispatcher's conversation, acknowledged by the dispatcher.
var TellStages = []string{"queued_dispatcher", "submitted", "read"}

// TellStageRank is a stage's place in TellStages, -1 for another word.
func TellStageRank(stage string) int { return slices.Index(TellStages, stage) }

// TellMarker is the key a plain line's stages go under in crew state: "tell/" and its envelope id (for a line from
// a crew of the same node, the inbox id it was taken under).
func TellMarker(id string) string { return "tell/" + id }

// TellKeep is how long a plain line's receipt record is kept.
const TellKeep = 30 * 24 * time.Hour

// ErrUnknownTell is a receipt for a line this crew holds no record of (sent by an older zmina, or pruned).
var ErrUnknownTell = errors.New("no record of this line")

// ErrBadTellReceipt is a receipt that does not match the line it names, or does not read.
var ErrBadTellReceipt = errors.New("tell receipt refused")

// TellReceipt is one stage of a plain line, sent by the crew that took it to the crew that sent it.
type TellReceipt struct {
	ID        string    `json:"id"`
	Origin    string    `json:"origin"`
	Recipient string    `json:"recipient"`
	Stage     string    `json:"stage"`
	At        time.Time `json:"at"`
}

// Validate checks a receipt's own fields; ApplyTellReceipt checks it against the line it names.
func (r TellReceipt) Validate() error {
	if !validTellID(r.ID) {
		return fmt.Errorf("invalid line id")
	}

	if TellStageRank(r.Stage) < 0 {
		return fmt.Errorf("unknown receipt stage %q", r.Stage)
	}

	if r.At.IsZero() {
		return fmt.Errorf("receipt without a time")
	}

	for _, a := range []string{r.Origin, r.Recipient} {
		if addr, err := mesh.ParseAddress(a); err != nil || addr.Crew == "" {
			return fmt.Errorf("receipt needs explicit origin and recipient crews")
		}
	}

	return nil
}

func validTellID(id string) bool {
	return len(id) <= 200 && mesh.ValidID(id) && !strings.ContainsFunc(id, unicode.IsControl)
}

// TellIn is a plain line this crew took, kept to send its receipts back to Origin. Intents are the receipt
// envelopes by stage, committed before they are queued.
type TellIn struct {
	Version   int               `json:"version"`
	ID        string            `json:"id"`
	Origin    string            `json:"origin"`
	Recipient string            `json:"recipient"`
	Sent      time.Time         `json:"sent,omitzero"`
	At        time.Time         `json:"at"`
	Intents   map[string]Intent `json:"intents,omitempty"`
}

// TellOut is a plain line this crew sent and what its destination reported of it: Stages at the destination's
// times, Seen when each receipt came here. Recipient is the crew that answered (To may name a node only).
type TellOut struct {
	Version   int                  `json:"version"`
	ID        string               `json:"id"`
	From      string               `json:"from"`
	To        string               `json:"to"`
	Recipient string               `json:"recipient,omitempty"`
	Head      string               `json:"head"`
	Sent      time.Time            `json:"sent"`
	Stage     string               `json:"stage,omitempty"`
	Stages    map[string]time.Time `json:"stages,omitempty"`
	Seen      map[string]time.Time `json:"seen,omitempty"`
}

// TellStore keeps one crew's plain line records: in/ for lines taken, out/ for lines sent.
type TellStore struct{ Dir string }

func tellFile(id string) string { return Hash(id)[:32] + ".json" }

func (s TellStore) path(side, id string) string { return filepath.Join(s.Dir, side, tellFile(id)) }

// LoadIn reads the record of a line taken.
func (s TellStore) LoadIn(id string) (TellIn, error) {
	var t TellIn
	if !validTellID(id) {
		return t, fmt.Errorf("invalid line id")
	}

	if err := readTell(s.path("in", id), &t); err != nil {
		return t, err
	}

	if t.ID != id {
		return t, fmt.Errorf("invalid line record")
	}

	if t.Intents == nil {
		t.Intents = map[string]Intent{}
	}

	return t, nil
}

// SaveIn writes the record of a line taken.
func (s TellStore) SaveIn(t TellIn) error {
	if !validTellID(t.ID) {
		return fmt.Errorf("invalid line id")
	}

	t.Version = 1

	return writeAtomic(filepath.Join(s.Dir, "in"), tellFile(t.ID), t)
}

// LoadOut reads the record of a line sent.
func (s TellStore) LoadOut(id string) (TellOut, error) {
	var t TellOut
	if !validTellID(id) {
		return t, fmt.Errorf("invalid line id")
	}

	if err := readTell(s.path("out", id), &t); err != nil {
		return t, err
	}

	if t.ID != id {
		return t, fmt.Errorf("invalid line record")
	}

	return t, nil
}

// SaveOut writes the record of a line sent.
func (s TellStore) SaveOut(t TellOut) error {
	if !validTellID(t.ID) {
		return fmt.Errorf("invalid line id")
	}

	t.Version = 1

	return writeAtomic(filepath.Join(s.Dir, "out"), tellFile(t.ID), t)
}

// AllIn are the records of lines taken, oldest first; a record that does not read is skipped.
func (s TellStore) AllIn() ([]TellIn, error) {
	var out []TellIn

	err := eachTell(filepath.Join(s.Dir, "in"), func(raw []byte) {
		var t TellIn
		if json.Unmarshal(raw, &t) == nil && validTellID(t.ID) {
			if t.Intents == nil {
				t.Intents = map[string]Intent{}
			}

			out = append(out, t)
		}
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })

	return out, err
}

// AllOut are the records of lines sent, oldest first; a record that does not read is skipped.
func (s TellStore) AllOut() ([]TellOut, error) {
	var out []TellOut

	err := eachTell(filepath.Join(s.Dir, "out"), func(raw []byte) {
		var t TellOut
		if json.Unmarshal(raw, &t) == nil && validTellID(t.ID) {
			out = append(out, t)
		}
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sent.Before(out[j].Sent) })

	return out, err
}

// RemoveIn drops the record of a line taken.
func (s TellStore) RemoveIn(id string) error {
	err := os.Remove(s.path("in", id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

// PruneOut drops the records of lines sent before now-keep.
func (s TellStore) PruneOut(now time.Time, keep time.Duration) error {
	all, err := s.AllOut()
	for _, t := range all {
		if now.Sub(t.Sent) > keep {
			if rerr := os.Remove(s.path("out", t.ID)); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}

	return err
}

// ApplyTellReceipt records a receipt that came to local from from: it must name a line local sent, come from the
// crew the line went to, and its stage only adds (a stage seen once keeps its first time; Stage only rises). A
// receipt that does not match is ErrBadTellReceipt; one for a line not on record, ErrUnknownTell.
func (s TellStore) ApplyTellReceipt(from, local string, r TellReceipt, now time.Time) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrBadTellReceipt, err)
	}

	t, err := s.LoadOut(r.ID)
	if errors.Is(err, os.ErrNotExist) {
		return ErrUnknownTell
	}

	if err != nil {
		return err
	}

	recipient, err := tellRecipient(t, from, local, r)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadTellReceipt, err)
	}

	t.Recipient = recipient

	if t.Stages == nil {
		t.Stages = map[string]time.Time{}
	}

	if t.Seen == nil {
		t.Seen = map[string]time.Time{}
	}

	if _, ok := t.Stages[r.Stage]; !ok {
		t.Stages[r.Stage] = r.At.UTC()
		t.Seen[r.Stage] = now.UTC()
	}

	if TellStageRank(r.Stage) > TellStageRank(t.Stage) {
		t.Stage = r.Stage
	}

	return s.SaveOut(t)
}

// tellRecipient checks a receipt against the line it names and returns the crew that took the line.
func tellRecipient(t TellOut, from, local string, r TellReceipt) (string, error) {
	if r.Origin != t.From || local != t.From {
		return "", fmt.Errorf("receipt for a line %s did not send", local)
	}

	if r.Recipient != from {
		return "", fmt.Errorf("receipt from %s names %s as the recipient", from, r.Recipient)
	}

	if t.Recipient != "" {
		if t.Recipient != from {
			return "", fmt.Errorf("receipt from %s for a line %s took", from, t.Recipient)
		}

		return from, nil
	}

	to, err := mesh.ParseAddress(t.To)
	if err != nil {
		return "", err
	}

	src, err := mesh.ParseAddress(from)
	if err != nil {
		return "", err
	}

	if src.Node != to.Node || (to.Crew != "" && to.Crew != src.Crew) {
		return "", fmt.Errorf("receipt from %s for a line sent to %s", from, t.To)
	}

	return from, nil
}

func readTell(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	return json.Unmarshal(raw, v)
}

func eachTell(dir string, fn func(raw []byte)) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}

	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return err
		}

		fn(raw)
	}

	return nil
}
