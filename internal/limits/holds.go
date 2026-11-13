package limits

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// HoldsFile is the file in the limits directory that keeps what the thresholds told and the holds that are on. It is
// written by the beat alone, whole or not at all, and read by whoever has to know whether a plan is held: the delivery
// (the beat's, the courier's), `zmina ls`, `zmina doctor`. It is not part of state.json on purpose: a state field moves
// the state's version, and an older build would then refuse the state.
const HoldsFile = "holds.json"

const (
	bookVersion = 1
	// bookMax bounds what is read of the file.
	bookMax = 1 << 20
	// toldKeep is how long a period's record is kept past its reset: a day, so a reading that lags the clock never meets
	// a period it has forgotten.
	toldKeep = 24 * time.Hour
	// toldKeepNoReset is how long the record of a window that gave no reset time is kept.
	toldKeepNoReset = 7 * 24 * time.Hour
)

// Hold is one window of one plan whose roles get nothing until it resets: the window was at Used percent, from Since, and
// its reset time is Reset. Ask is the owner's todo item the hold opened when it held the dispatcher too (empty otherwise).
type Hold struct {
	Plan   Plan      `json:"plan"`
	Window string    `json:"window"`
	Reset  time.Time `json:"reset"`
	Since  time.Time `json:"since"`
	Used   float64   `json:"used"`
	Ask    string    `json:"ask,omitempty"`
}

// Active reports whether the hold is on at now: until its window's reset time, and not after. The delivery asks this and
// nothing else, so a hold ends by the clock even when no beat has run to say so.
func (h Hold) Active(now time.Time) bool { return now.Before(h.Reset) }

// Label is the hold's window in words: "claude 5h window".
func (h Hold) Label() string { return string(h.Plan) + " " + WindowLabel(h.Window) }

// Told is what a period was told: the moments the warn and the checkpoint went. Reset is the period's own, kept for pruning.
type Told struct {
	Reset      time.Time `json:"reset,omitzero"`
	Warn       time.Time `json:"warn,omitzero"`
	Checkpoint time.Time `json:"checkpoint,omitzero"`
}

// Book is the holds file: the periods told, and the holds that are on.
type Book struct {
	Version int             `json:"v"`
	Told    map[string]Told `json:"told,omitempty"`
	Holds   []Hold          `json:"holds,omitempty"`
}

// Key names a period: the plan, the window and the window's reset time to the minute (a reset that moves by seconds is
// the same period, one that moves further is a new one). A window that gives no reset time has the one period "none".
func Key(plan Plan, window string, resets *time.Time) string {
	if resets == nil || resets.IsZero() {
		return fmt.Sprintf("%s|%s|none", plan, window)
	}

	return fmt.Sprintf("%s|%s|%d", plan, window, minute(*resets))
}

// minute is a moment rounded to the minute, in epoch minutes.
func minute(t time.Time) int64 { return t.Round(time.Minute).Unix() / 60 }

// Active are the holds that are on at now, the plan's own order kept.
func (b Book) Active(now time.Time) []Hold {
	var out []Hold

	for _, h := range b.Holds {
		if h.Active(now) {
			out = append(out, h)
		}
	}

	return out
}

// HeldUntil is the hold that keeps a plan's roles longest at now: of several windows at once, the one that resets last.
func (b Book) HeldUntil(plan Plan, now time.Time) (Hold, bool) {
	var best Hold

	found := false

	for _, h := range b.Holds {
		if h.Plan == plan && h.Active(now) && (!found || h.Reset.After(best.Reset)) {
			best, found = h, true
		}
	}

	return best, found
}

// holding reports whether a hold of the period is on the book (active or not: one past its reset is the next beat's to end).
func (b Book) holding(plan Plan, window string, reset time.Time) bool {
	return slices.ContainsFunc(b.Holds, func(h Hold) bool {
		return h.Plan == plan && h.Window == window && minute(h.Reset) == minute(reset)
	})
}

func (b Book) clone() Book {
	out := Book{Version: bookVersion, Holds: slices.Clone(b.Holds)}

	if len(b.Told) > 0 {
		out.Told = make(map[string]Told, len(b.Told))
		for k, v := range b.Told {
			out.Told[k] = v
		}
	}

	return out
}

// same reports whether two books say the same, an empty list and none alike.
func (b Book) same(o Book) bool {
	b.Version, o.Version = 0, 0

	x, _ := json.Marshal(b)
	y, _ := json.Marshal(o)

	return string(x) == string(y)
}

// ReadBook reads the holds file in dir. A file that is not there is an empty book; one that does not parse is an empty
// book and an error, so a caller may say so and still go on: nothing is held by a file that cannot be read.
func ReadBook(dir string) (Book, error) {
	path := filepath.Join(dir, HoldsFile)

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Book{Version: bookVersion}, nil
	}

	if err != nil {
		return Book{Version: bookVersion}, err
	}

	raw, err := io.ReadAll(io.LimitReader(f, bookMax))
	_ = f.Close()

	if err != nil {
		return Book{Version: bookVersion}, err
	}

	var b Book
	if err := json.Unmarshal(raw, &b); err != nil {
		return Book{Version: bookVersion}, fmt.Errorf("%s: %w", path, err)
	}

	b.Version = bookVersion

	return b, nil
}

// WriteBook keeps the book in dir, whole or not at all. A book with nothing in it removes the file.
func WriteBook(dir string, b Book) error {
	path := filepath.Join(dir, HoldsFile)
	b.Version = bookVersion

	if len(b.Told) == 0 && len(b.Holds) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		return nil
	}

	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}

	if err := fsx.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	return fsx.WriteFile(path, append(raw, '\n'), 0o600)
}
