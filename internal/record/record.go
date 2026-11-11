// Package record stores role-shaped worker-authored results and checkpoints.
package record

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
)

// Ref identifies an immutable record. Paths are reconstructed from the crew layout.
type Ref struct {
	Task     string    `json:"task,omitempty"`
	Revision string    `json:"revision"`
	At       time.Time `json:"at"`
}

// Document deliberately has no development-specific fields.
type Document struct {
	Version int               `json:"version"`
	Kind    string            `json:"kind"`
	Task    string            `json:"task"`
	Role    string            `json:"role"`
	By      string            `json:"by"`
	At      time.Time         `json:"at"`
	Shape   crew.HandoffShape `json:"shape"`
	Values  map[string]string `json:"values"`
}

var taskID = regexp.MustCompile(`^T-[0-9]+$`)
var revisionID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// revisionPrefix is what --revision takes: the whole hash or its start, eight hex digits at least.
var revisionPrefix = regexp.MustCompile(`^[a-f0-9]{8,64}$`)

// ArchiveRoot is where zmina retention moves the records of tasks closed long ago, under the crew's directory:
// <ArchiveRoot>/<yyyy-mm>/<kind>s/<task>/<revision>.json. Reads fall back to it (read-only: nothing is moved back or
// written there), and a directory whose files went has a StubFile saying so.
var ArchiveRoot = filepath.Join("archive", "retention")

// StubFile is the note retention leaves in a records directory it emptied.
const StubFile = "archived.txt"

// archived is the files of kind under task id in the archive that match name, the newest month last.
func archived(dir, kind, id, name string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, ArchiveRoot, "*", kind+"s", id, name))
	return paths
}

// ShortLen is how many hex digits a revision is shown with: enough to tell it apart, short enough to read.
const ShortLen = 12

// Short is rev cut to ShortLen hex digits, or longer while another of revs starts the same: what a line shows of it
// still names that one revision.
func Short(revs []string, rev string) string {
	n := min(ShortLen, len(rev))
	for ; n < len(rev); n++ {
		clash := false
		for _, o := range revs {
			if o != rev && strings.HasPrefix(o, rev[:n]) {
				clash = true
				break
			}
		}
		if !clash {
			break
		}
	}
	return rev[:n]
}

// Resolve is the whole revision of task id's record that prefix names: the hash itself, or a start of it (at least
// eight hex digits) that only one of the task's revisions has.
func Resolve(dir, kind, id, prefix string) (string, error) {
	_, rev, err := Find(dir, kind, []string{id}, prefix)
	return rev, err
}

// Find is the one record among the tasks ids (a task and the task its records were carried from) whose revision
// starts with prefix, and that task. A prefix two of them share is refused wherever they lie: an answer from
// another task would be a different record than the one asked for.
func Find(dir, kind string, ids []string, prefix string) (string, string, error) {
	if !revisionPrefix.MatchString(prefix) {
		return "", "", fmt.Errorf("invalid revision %q: a hash or its first 8 or more hex digits", prefix)
	}
	type hit struct{ id, rev string }
	var found []hit
	for _, id := range unique(ids) {
		revs, err := revisions(dir, kind, id, prefix)
		if err != nil {
			return "", "", err
		}
		for _, rev := range revs {
			found = append(found, hit{id, rev})
		}
	}
	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("no %s revision %s for %s%s", kind, prefix, strings.Join(unique(ids), " or "), stubNotes(dir, kind, ids))
	case 1:
		return found[0].id, found[0].rev, nil
	}
	return "", "", fmt.Errorf("revision %s of %s names %d %ss; give more of it", prefix, strings.Join(unique(ids), " or "), len(found), kind)
}

// Revisions is every revision of kind kept under the tasks ids: what a short form has to stay apart from.
func Revisions(dir, kind string, ids []string) []string {
	var out []string
	for _, id := range unique(ids) {
		revs, _ := revisions(dir, kind, id, "")
		out = append(out, revs...)
	}
	return out
}

// revisions is the revisions of task id's records that start with prefix, read from the file names.
func revisions(dir, kind, id, prefix string) ([]string, error) {
	if _, err := path(dir, kind, id, strings.Repeat("0", 64)); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, kind+"s", id, prefix+"*.json"))
	if err != nil {
		return nil, err
	}
	paths = append(paths, archived(dir, kind, id, prefix+"*.json")...)
	var out []string
	for _, p := range paths {
		if rev := strings.TrimSuffix(filepath.Base(p), ".json"); revisionID.MatchString(rev) && !slices.Contains(out, rev) {
			out = append(out, rev)
		}
	}
	return out, nil
}

// stubNotes is what the notes retention left in the tasks' emptied records directories say, for an error: the
// archived files are the likeliest reason a revision is not found.
func stubNotes(dir, kind string, ids []string) string {
	var out string
	for _, id := range unique(ids) {
		if note, err := os.ReadFile(filepath.Join(dir, kind+"s", id, StubFile)); err == nil {
			out += " (archived and not in the archive: " + strings.TrimSpace(string(note)) + ")"
		}
	}
	return out
}

func unique(ids []string) []string {
	var out []string
	for _, id := range ids {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func path(dir, kind, id, revision string) (string, error) {
	if (kind != "result" && kind != "checkpoint") || !taskID.MatchString(id) || !revisionID.MatchString(revision) {
		return "", fmt.Errorf("invalid record identity")
	}
	return filepath.Join(dir, kind+"s", id, revision+".json"), nil
}

// Validate checks named values against the supplied role shape, including unknown sections.
func Validate(shape crew.HandoffShape, values map[string]string) error {
	expected := map[string]bool{}
	for _, s := range shape.Sections {
		expected[s.Name] = true
		if _, ok := values[s.Name]; !ok {
			return fmt.Errorf("missing section %q; use the role handoff shape", s.Name)
		}
	}
	for k := range values {
		if !expected[k] {
			return fmt.Errorf("unknown section %q", k)
		}
	}
	return nil
}

// Save is content-addressed, excluding observation time, so retries keep the first revision/time.
func Save(dir string, d Document) (Ref, error) {
	if err := Validate(d.Shape, d.Values); err != nil {
		return Ref{}, err
	}
	at := d.At.UTC()
	d.At = time.Time{}
	raw, err := json.Marshal(d)
	if err != nil {
		return Ref{}, err
	}
	rev := fmt.Sprintf("%x", sha256.Sum256(raw))
	p, err := path(dir, d.Kind, d.Task, rev)
	if err != nil {
		return Ref{}, err
	}
	if old, err := Load(dir, d.Kind, d.Task, rev); err == nil {
		return Ref{Task: d.Task, Revision: rev, At: old.At}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Ref{}, err
	}
	d.At = at
	if err := fsx.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return Ref{}, err
	}
	raw, err = json.MarshalIndent(d, "", "  ")
	if err != nil {
		return Ref{}, err
	}
	if err = fsx.WriteFile(p, append(raw, '\n'), 0o600); err != nil {
		return Ref{}, err
	}
	return Ref{Task: d.Task, Revision: rev, At: at}, nil
}

func Load(dir, kind, id, revision string) (Document, error) {
	p, err := path(dir, kind, id, revision)
	if err != nil {
		return Document{}, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		// Moved by retention: read the archived copy (the newest month when a task was archived twice).
		if found := archived(dir, kind, id, revision+".json"); len(found) > 0 {
			raw, err = os.ReadFile(found[len(found)-1])
		} else if note, nerr := os.ReadFile(filepath.Join(filepath.Dir(p), StubFile)); nerr == nil {
			err = fmt.Errorf("%s was archived and is not in the archive: %s: %w", p, strings.TrimSpace(string(note)), os.ErrNotExist)
		}
	}
	if err != nil {
		return Document{}, err
	}
	var d Document
	if err = json.Unmarshal(raw, &d); err != nil {
		return d, err
	}
	if d.Task != id || d.Kind != kind {
		return d, fmt.Errorf("record identity mismatch")
	}
	at := d.At
	d.At = time.Time{}
	raw, err = json.Marshal(d)
	d.At = at
	if err != nil {
		return d, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != revision {
		return d, fmt.Errorf("record content hash mismatch")
	}
	return d, nil
}

// All enumerates revision references for reconciliation, independently of task state or event retention.
func All(dir, kind string) (map[string][]Ref, error) {
	if kind != "result" && kind != "checkpoint" {
		return nil, fmt.Errorf("invalid record kind")
	}
	paths, err := filepath.Glob(filepath.Join(dir, kind+"s", "T-*", "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string][]Ref{}
	var problems error
	for _, p := range paths {
		id := filepath.Base(filepath.Dir(p))
		rev := filepath.Base(p)
		rev = rev[:len(rev)-5]
		d, err := Load(dir, kind, id, rev)
		if err != nil {
			problems = errors.Join(problems, err)
			continue
		}
		out[id] = append(out[id], Ref{Task: id, Revision: rev, At: d.At})
	}
	return out, problems
}
