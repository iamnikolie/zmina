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
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
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
	if !revisionPrefix.MatchString(prefix) {
		return "", fmt.Errorf("invalid revision %q: a hash or its first 8 or more hex digits", prefix)
	}
	if _, err := path(dir, kind, id, strings.Repeat("0", 64)); err != nil {
		return "", err
	}
	paths, err := filepath.Glob(filepath.Join(dir, kind+"s", id, prefix+"*.json"))
	if err != nil {
		return "", err
	}
	var found []string
	for _, p := range paths {
		if rev := strings.TrimSuffix(filepath.Base(p), ".json"); revisionID.MatchString(rev) {
			found = append(found, rev)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no %s revision %s for %s", kind, prefix, id)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("revision %s of %s names %d %ss; give more of it", prefix, id, len(found), kind)
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
	} else if !os.IsNotExist(err) {
		return Ref{}, err
	}
	d.At = at
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return Ref{}, err
	}
	raw, err = json.MarshalIndent(d, "", "  ")
	if err != nil {
		return Ref{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".record-*")
	if err != nil {
		return Ref{}, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return Ref{}, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return Ref{}, err
	}
	if err = f.Close(); err != nil {
		return Ref{}, err
	}
	if err = os.Rename(f.Name(), p); err != nil {
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
