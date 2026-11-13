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
	// bookVersion is the holds file's own version. A file of a higher one is not read and not touched (ErrBookFuture); a file of the
	// first version (periods kept as a map) reads, with what it told forgotten: a warn or a checkpoint may be said once more.
	bookVersion = 2
	// bookMax bounds what is read of the file.
	bookMax = 1 << 20
	// toldKeep is how long a period's record is kept past its reset: a day, so a reading that lags the clock never meets
	// a period it has forgotten.
	toldKeep = 24 * time.Hour
	// toldKeepNoReset is how long the record of a window that gave no reset time is kept.
	toldKeepNoReset = 7 * 24 * time.Hour
)

// ErrBookFuture is the holds file being of a newer version than this zmina reads: nothing is held by it here, and nothing is
// written over it.
var ErrBookFuture = errors.New("the holds file is of a newer zmina")

// Hold is one window of one plan whose roles get nothing until it resets: the window was at Used percent, from Since, and
// its reset time is Reset. Ask is the owner's todo item the hold opened when it held the dispatcher too (empty otherwise).
// Home is the Codex home whose reading made the hold: a Codex seat of another home is not held by it (empty for Claude).
type Hold struct {
	Plan   Plan      `json:"plan"`
	Window string    `json:"window"`
	Home   string    `json:"home,omitempty"`
	Reset  time.Time `json:"reset"`
	Since  time.Time `json:"since"`
	Used   float64   `json:"used"`
	Ask    string    `json:"ask,omitempty"`
}

// credible reports whether the hold's reset time can be its window's: no further from the moment the hold began than the window
// is long, with slack (MaxReset). One that is not (a reset a garbled reading gave) holds nothing, and the beat ends it.
func (h Hold) credible(now time.Time) bool {
	from := h.Since
	if from.IsZero() || from.After(now) {
		from = now
	}

	return h.Reset.Sub(from) <= MaxReset(h.Window)
}

// Active reports whether the hold is on at now: until its window's reset time, and not after, and only for a reset that can be
// its window's. The delivery asks this and nothing else, so a hold ends by the clock even when no beat has run to say so.
func (h Hold) Active(now time.Time) bool { return now.Before(h.Reset) && h.credible(now) }

// Label is the hold's window in words: "claude 5h window".
func (h Hold) Label() string { return string(h.Plan) + " " + WindowLabel(h.Window) }

// Period is what a window's period (its plan, window and reset time) was told, and whether its hold was lifted by hand: the
// moments the warn and the checkpoint went, and Lifted, when the owner lifted the hold: that period is not held again (a new
// period, a later reset, may be). Two reset times less than two minutes apart are one period, and the later is kept.
type Period struct {
	Plan       Plan      `json:"plan"`
	Window     string    `json:"window"`
	Home       string    `json:"home,omitempty"`
	Reset      time.Time `json:"reset,omitzero"`
	Warn       time.Time `json:"warn,omitzero"`
	Checkpoint time.Time `json:"checkpoint,omitzero"`
	Lifted     time.Time `json:"lifted,omitzero"`
}

// Wait is a role of a plan whose hold ended and that gets its lines later than the others: the release is paced, the dispatcher
// first, then a few roles a minute (Stagger). Until is when it goes. Pending is a wait that waits for more than the clock: a hold
// that ended by the clock alone, with no fresh reading to say the window rolled over, lets one role go first and keeps the rest until a
// fresh reading shows the window under its hold share, or Until, ten minutes on.
type Wait struct {
	Role    string    `json:"role"`
	Plan    Plan      `json:"plan"`
	Window  string    `json:"window,omitempty"`
	Home    string    `json:"home,omitempty"`
	Until   time.Time `json:"until"`
	Pending bool      `json:"pending,omitempty"`
}

// Postponed is a scheduled run of a role that a hold put off and that was said so once: the role and the time of the run (its NextRun).
type Postponed struct {
	Role string    `json:"role"`
	Run  time.Time `json:"run"`
}

// postponedKeep bounds how many postponed runs the book remembers, and for how long.
const (
	postponedMax  = 50
	postponedKeep = 7 * 24 * time.Hour
)

// Book is the holds file: the periods told, the holds that are on, the roles still waiting their turn after one ended and the scheduled runs
// that were put off.
type Book struct {
	Version   int         `json:"v"`
	Periods   []Period    `json:"periods,omitempty"`
	Holds     []Hold      `json:"holds,omitempty"`
	Waits     []Wait      `json:"waits,omitempty"`
	Postponed []Postponed `json:"postponed,omitempty"`
}

// WasPostponed reports whether a run of a role was already said to be put off.
func (b Book) WasPostponed(role string, run time.Time) bool {
	return slices.ContainsFunc(b.Postponed, func(p Postponed) bool { return p.Role == role && p.Run.Equal(run) })
}

// Postpone notes that a run was put off, keeping the newest postponedMax.
func (b *Book) Postpone(role string, run time.Time) {
	b.Postponed = append(b.Postponed, Postponed{Role: role, Run: run})
	if n := len(b.Postponed); n > postponedMax {
		b.Postponed = b.Postponed[n-postponedMax:]
	}
}

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

// HeldUntil is the hold that keeps a plan's roles longest at now: of several windows at once, the one that resets last. home is the
// Codex home of the role asking (empty: any hold of the plan counts): a hold made by another home's reading does not hold it.
func (b Book) HeldUntil(plan Plan, home string, now time.Time) (Hold, bool) {
	var best Hold

	found := false

	for _, h := range b.Holds {
		if h.Plan == plan && holds(h.Home, home) && h.Active(now) && (!found || h.Reset.After(best.Reset)) {
			best, found = h, true
		}
	}

	return best, found
}

// holds reports whether a hold of a Codex home (empty: of the plan) keeps a seat of the home asking.
func holds(holdHome, seatHome string) bool {
	return holdHome == "" || seatHome == "" || holdHome == seatHome
}

// WaitFor is the wait of a role that has not had its turn yet at now.
func (b Book) WaitFor(role string, now time.Time) (Wait, bool) {
	for _, w := range b.Waits {
		if w.Role == role && now.Before(w.Until) {
			return w, true
		}
	}

	return Wait{}, false
}

// holding reports whether a hold of the period is on the book (active or not: one past its reset is the next beat's to end).
func (b Book) holding(plan Plan, window, home string, reset time.Time) bool {
	return slices.ContainsFunc(b.Holds, func(h Hold) bool {
		return h.Plan == plan && h.Window == window && h.Home == home && sameReset(h.Reset, reset)
	})
}

// find is the place of a period in the book, -1 when it has none: the plan's window with a reset less than two minutes from
// reset, or, for a window that gives none, the one period with none.
func (b Book) find(plan Plan, window, home string, reset time.Time) int {
	return slices.IndexFunc(b.Periods, func(p Period) bool {
		return p.Plan == plan && p.Window == window && p.Home == home &&
			((reset.IsZero() && p.Reset.IsZero()) || (!reset.IsZero() && !p.Reset.IsZero() && sameReset(p.Reset, reset)))
	})
}

func (b Book) clone() Book {
	return Book{Version: bookVersion, Periods: slices.Clone(b.Periods), Holds: slices.Clone(b.Holds), Waits: slices.Clone(b.Waits),
		Postponed: slices.Clone(b.Postponed)}
}

// same reports whether two books say the same, an empty list and none alike.
func (b Book) same(o Book) bool {
	b.Version, o.Version = 0, 0

	x, _ := json.Marshal(b)
	y, _ := json.Marshal(o)

	return string(x) == string(y)
}

// ReadBook reads the holds file in dir. A file that is not there is an empty book. One that does not parse is an empty book and an
// error; one of a newer version is an empty book and ErrBookFuture. A caller may say so and still go on: nothing is held by a file
// that cannot be read.
func ReadBook(dir string) (Book, error) {
	path := filepath.Join(dir, HoldsFile)
	empty := Book{Version: bookVersion}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}

	if err != nil {
		return empty, err
	}

	raw, err := io.ReadAll(io.LimitReader(f, bookMax))
	_ = f.Close()

	if err != nil {
		return empty, err
	}

	var b Book
	if err := json.Unmarshal(raw, &b); err != nil {
		return empty, fmt.Errorf("%s: %w", path, err)
	}

	if b.Version > bookVersion {
		return empty, fmt.Errorf("%s is version %d, this zmina reads up to %d: %w", path, b.Version, bookVersion, ErrBookFuture)
	}

	b.Version = bookVersion

	return b, nil
}

// badKeep is how many holds files that did not read are kept aside (MoveAside).
const badKeep = 3

// MoveAside takes a holds file that does not read out of the way, as holds.json.bad-<time>, and keeps the newest three of them: the
// beat writes a fresh book where it was. It returns where the file went; empty when there was none.
func MoveAside(dir string, now time.Time) (string, error) {
	path := filepath.Join(dir, HoldsFile)
	to := path + ".bad-" + now.UTC().Format("20060102-150405")

	if err := os.Rename(path, to); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", err
	}

	old, _ := filepath.Glob(path + ".bad-*")
	slices.Sort(old)

	for len(old) > badKeep {
		_ = os.Remove(old[0])
		old = old[1:]
	}

	return to, nil
}

// WriteBook keeps the book in dir, whole or not at all. A book with nothing in it removes the file.
func WriteBook(dir string, b Book) error {
	path := filepath.Join(dir, HoldsFile)
	b.Version = bookVersion

	if len(b.Periods) == 0 && len(b.Holds) == 0 && len(b.Waits) == 0 && len(b.Postponed) == 0 {
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
