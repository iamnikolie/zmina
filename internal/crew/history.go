package crew

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// HistoryKeep is how many versions of the crew file its history holds; past it the oldest go.
const HistoryKeep = 30

// What a version says made it, when no command did: the first time zmina looked at the file, and an edit it was not
// told of (a person or a role editing crew.yaml directly), seen on the next command that loads the file.
const (
	HowFirst = "first seen"
	HowHand  = "edited by hand"
)

// History is the versions the crew file has had, kept beside it in crew.d/history. Every version is one file named
// <UTC time>-<sha8>.yaml (the bytes, as restored) with a <same name>.json beside it saying who made it and how; HEAD
// holds the hash of the newest, so a command that loads the crew file every minute checks it with one small read and
// writes nothing while the file is as last recorded.
//
// The newest version is the file as it is now: what an edit replaces was recorded before it, either when zmina first
// looked at the file or by the edit itself.
type History struct {
	// Dir is the history directory.
	Dir string
	// Keep is how many versions are kept (default HistoryKeep).
	Keep int
	// Now is the clock; tests set it.
	Now func() time.Time
}

// HistoryOf is the history of the crew file at path.
func HistoryOf(path string) *History {
	return &History{Dir: Layout{Dir: filepath.Dir(path)}.History()}
}

// History is where this crew's file keeps its versions.
func (c *Crew) History() *History { return HistoryOf(c.Path) }

// Version is one kept version of the crew file.
type Version struct {
	// Stem is the file name without its extension; the version's identity.
	Stem string `json:"stem"`
	// At is when it was recorded, UTC.
	At time.Time `json:"at"`
	// By is who made the change when zmina did (a role, or hand for the owner's terminal); empty for an edit zmina
	// only noticed.
	By string `json:"by,omitempty"`
	// How is the command that made it ("zmina seat set dev"), or HowHand, or HowFirst.
	How string `json:"how"`
	// What is what changed against the version before it, in words ("dev: model, args; added scribe").
	What string `json:"what,omitempty"`
	// Sum is the hash of its bytes, hex.
	Sum string `json:"sum"`
}

// Recorded is what keeping a version came to.
type Recorded struct {
	Version
	// Wrote is false when the bytes already were the newest version: nothing was kept, nothing changed.
	Wrote bool `json:"-"`
	// First is true when no version came before it: the history began here, nothing changed.
	First bool `json:"-"`
	// Role is the one role the change is about, when it touched that role and nothing else.
	Role string `json:"-"`
}

// Detail is the change as an event line says it: what, then how.
func (r Recorded) Detail() string {
	what := r.What
	if what == "" {
		what = "the file changed"
	}

	return what + " (" + r.How + ")"
}

func sumOf(raw []byte) string {
	h := sha256.Sum256(raw)

	return hex.EncodeToString(h[:])
}

func (h *History) keep() int {
	if h.Keep > 0 {
		return h.Keep
	}

	return HistoryKeep
}

func (h *History) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}

	return time.Now()
}

func (h *History) head() string { return filepath.Join(h.Dir, "HEAD") }

// Seen reports whether raw is what the history last recorded: one small read, nothing written.
func (h *History) Seen(raw []byte) bool {
	got, err := os.ReadFile(h.head())

	return err == nil && strings.TrimSpace(string(got)) == sumOf(raw)
}

// Notice keeps raw, the crew file as it is on disk, when it is not what was last recorded: an edit zmina was not
// told of, or the first look. When nothing changed it reads one small file and writes nothing.
func (h *History) Notice(raw []byte) (Recorded, error) { return h.record(raw, "", "") }

// Record keeps raw, which a command of zmina's has just written, as the newest version.
func (h *History) Record(raw []byte, by, how string) (Recorded, error) { return h.record(raw, by, how) }

func (h *History) record(raw []byte, by, how string) (Recorded, error) {
	if h.Seen(raw) {
		return Recorded{}, nil
	}

	unlock, err := h.lock()
	if err != nil {
		return Recorded{}, fmt.Errorf("crew history: %w", err)
	}
	defer unlock()

	return h.recordLocked(raw, by, how)
}

// recordLocked is record with the lock held.
func (h *History) recordLocked(raw []byte, by, how string) (Recorded, error) {
	sum := sumOf(raw)

	// Another command may have kept it while this one waited for the lock.
	if h.Seen(raw) {
		return Recorded{}, nil
	}

	stems, err := h.stems()
	if err != nil {
		return Recorded{}, fmt.Errorf("crew history: %w", err)
	}

	var prev []byte

	if len(stems) > 0 {
		prev, _ = os.ReadFile(h.file(stems[len(stems)-1], ".yaml"))
	}

	if prev != nil && bytes.Equal(prev, raw) {
		// A recording that stopped before HEAD was written: mend HEAD, keep nothing twice.
		return Recorded{}, h.setHead(sum)
	}

	if how == "" {
		how = HowHand
		if len(stems) == 0 {
			how = HowFirst
		}
	}

	at := h.now().UTC().Truncate(time.Millisecond)
	if len(stems) > 0 {
		// Names sort by time; a clock that went back, or two versions in one millisecond, still sort in the order made.
		if last := stemTime(stems[len(stems)-1]); !at.After(last) {
			at = last.Add(time.Millisecond)
		}
	}

	v := Version{Stem: at.Format(stemLayout) + "-" + sum[:8], At: at, By: by, How: how, Sum: sum}

	out := Recorded{Version: v, Wrote: true, First: prev == nil}
	if prev != nil {
		out.What, out.Role = summarize(prev, raw)
		v.What = out.What
	}

	meta, err := json.Marshal(v)
	if err != nil {
		return Recorded{}, err
	}

	// The bytes first, HEAD last: a recording cut short is mended by the next one.
	if err := fsx.WriteFile(h.file(v.Stem, ".yaml"), raw, 0o600); err != nil {
		return Recorded{}, fmt.Errorf("crew history: %w", err)
	}

	if err := fsx.WriteFile(h.file(v.Stem, ".json"), append(meta, '\n'), 0o600); err != nil {
		return Recorded{}, fmt.Errorf("crew history: %w", err)
	}

	if err := h.setHead(sum); err != nil {
		return Recorded{}, err
	}

	h.prune(append(stems, v.Stem))

	return out, nil
}

// Saved is what a change of the crew file left in its history: the versions kept, oldest first (an edit nobody had
// told zmina of, then the change itself), and the trouble keeping the change itself, which does not undo it.
type Saved struct {
	Versions   []Recorded
	HistoryErr error
}

func (s *Saved) add(r Recorded) {
	if r.Wrote {
		s.Versions = append(s.Versions, r)
	}
}

// Commit writes raw as the crew file at path, holding the history's lock so that no command takes the change for an
// edit by hand. The file as it is now is kept first (and a failure to keep it stops the change: nothing would be left
// to go back to), then raw is written and kept as the newest version.
func (h *History) Commit(path string, raw []byte, by, how string) (Saved, error) {
	var saved Saved

	unlock, err := h.lock()
	if err != nil {
		return saved, fmt.Errorf("not saved, its previous version could not be kept: %w", err)
	}
	defer unlock()

	if old, err := os.ReadFile(path); err == nil {
		r, err := h.recordLocked(old, "", "")
		if err != nil {
			return saved, fmt.Errorf("not saved, its previous version could not be kept: %w", err)
		}

		saved.add(r)
	}

	if err := fsx.WriteFile(path, raw, 0o600); err != nil {
		return saved, fmt.Errorf("crew: %w", err)
	}

	r, err := h.recordLocked(raw, by, how)
	if err != nil {
		saved.HistoryErr = err

		return saved, nil
	}

	saved.add(r)

	return saved, nil
}

func (h *History) setHead(sum string) error {
	if err := fsx.WriteFile(h.head(), []byte(sum+"\n"), 0o600); err != nil {
		return fmt.Errorf("crew history: %w", err)
	}

	return nil
}

// prune drops the oldest versions past the keep count; stems is every version, oldest first.
func (h *History) prune(stems []string) {
	for len(stems) > h.keep() {
		_ = os.Remove(h.file(stems[0], ".yaml"))
		_ = os.Remove(h.file(stems[0], ".json"))
		stems = stems[1:]
	}
}

// lock serialises the writers of the history: a command noticing an edit and one making an edit.
func (h *History) lock() (func(), error) {
	if err := fsx.MkdirAll(h.Dir, 0o700); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(filepath.Join(h.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(5 * time.Second)

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()

			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}

		time.Sleep(10 * time.Millisecond)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

const stemLayout = "20060102T150405.000Z"

func stemTime(stem string) time.Time {
	t, err := time.Parse(stemLayout, stem[:min(len(stem), len(stemLayout))])
	if err != nil {
		return time.Time{}
	}

	return t
}

func (h *History) file(stem, ext string) string { return filepath.Join(h.Dir, stem+ext) }

// stems are the names of the kept versions, oldest first. A history that is not there yet has none.
func (h *History) stems() ([]string, error) {
	entries, err := os.ReadDir(h.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var out []string

	for _, e := range entries {
		if stem, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && !e.IsDir() {
			out = append(out, stem)
		}
	}

	sort.Strings(out)

	return out, nil
}

// List is the kept versions, newest first: the first is the file as it is now.
func (h *History) List() ([]Version, error) {
	stems, err := h.stems()
	if err != nil {
		return nil, fmt.Errorf("crew history: %w", err)
	}

	out := make([]Version, 0, len(stems))

	for _, stem := range slices.Backward(stems) {
		v := Version{Stem: stem, At: stemTime(stem), How: "unknown"}

		if raw, err := os.ReadFile(h.file(stem, ".json")); err == nil {
			var m Version
			if json.Unmarshal(raw, &m) == nil {
				v = m
				v.Stem = stem
			}
		}

		if v.Sum == "" {
			if _, sum, ok := strings.Cut(stem, "-"); ok {
				v.Sum = sum
			}
		}

		out = append(out, v)
	}

	return out, nil
}

// Bytes are the crew file as the version had it.
func (h *History) Bytes(v Version) ([]byte, error) {
	raw, err := os.ReadFile(h.file(v.Stem, ".yaml"))
	if err != nil {
		return nil, fmt.Errorf("crew history: %w", err)
	}

	return raw, nil
}

// summarize says what changed between two versions of the crew file, and the one role it is about when a single
// role changed and no crew-level key did. Two files that differ in comments or layout only say so.
func summarize(prev, cur []byte) (string, string) {
	var a, b map[string]any

	if yaml.Unmarshal(prev, &a) != nil || yaml.Unmarshal(cur, &b) != nil {
		return "", ""
	}

	var parts []string

	var keys []string

	for _, k := range unionKeys(a, b) {
		if k != "roles" && !reflect.DeepEqual(a[k], b[k]) {
			keys = append(keys, k)
		}
	}

	ra, _ := a["roles"].(map[string]any)
	rb, _ := b["roles"].(map[string]any)

	var added, removed, changed []string

	var touched []string

	for _, name := range unionKeys(ra, rb) {
		va, inA := ra[name]
		vb, inB := rb[name]

		switch {
		case !inA:
			added = append(added, name)
			touched = append(touched, name)
		case !inB:
			removed = append(removed, name)
			touched = append(touched, name)
		case !reflect.DeepEqual(va, vb):
			if ck := changedKeys(va, vb); len(ck) > 0 {
				changed = append(changed, name+": "+strings.Join(ck, ", "))
				touched = append(touched, name)
			}
		}
	}

	if len(added) > 0 {
		parts = append(parts, "added "+listed(added))
	}

	if len(removed) > 0 {
		parts = append(parts, "removed "+listed(removed))
	}

	parts = append(parts, changed...)

	if len(keys) > 0 {
		parts = append(parts, "crew: "+strings.Join(keys, ", "))
	}

	if len(parts) == 0 {
		return "comments or layout only", ""
	}

	role := ""
	if len(touched) == 1 && len(keys) == 0 {
		role = touched[0]
	}

	return strings.Join(parts, "; "), role
}

// changedKeys are the keys that differ between two roles' settings.
func changedKeys(a, b any) []string {
	from, okFrom := a.(map[string]any)
	to, okTo := b.(map[string]any)

	if !okFrom && a != nil || !okTo && b != nil {
		return []string{"the role"}
	}

	var out []string

	for _, k := range unionKeys(from, to) {
		if !reflect.DeepEqual(from[k], to[k]) {
			out = append(out, k)
		}
	}

	return out
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}

	var out []string

	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}

	sort.Strings(out)

	return out
}

// listed is names joined, the first few and how many more.
func listed(names []string) string {
	if len(names) <= 4 {
		return strings.Join(names, ", ")
	}

	return fmt.Sprintf("%s and %d more", strings.Join(names[:4], ", "), len(names)-4)
}
