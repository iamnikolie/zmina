package message

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// ArchiveDir is the directory under the store's own where finished messages are kept. Scan and All do not read it;
// Load falls back to it, so a message of an archived id is still found.
const ArchiveDir = "archive"

// reportedFile is where the store remembers what was already said about files and messages that failed (no .json
// in the name: Scan does not take it for a message).
const reportedFile = "reported"

// Finished reports whether m is done with: every revision is superseded or read by its destination, every envelope
// it needed is queued, and nothing about it happened since cutoff (the report, its receipts, its last failed sync).
// A message that is not finished, or too recent, stays where it is.
func Finished(m Message, cutoff time.Time) bool {
	if len(m.Revisions) == 0 {
		return false
	}
	newest := time.Time{}
	later := func(ts ...time.Time) {
		for _, t := range ts {
			if t.After(newest) {
				newest = t
			}
		}
	}
	for _, v := range m.Revisions {
		if v == nil || (!v.Superseded && StageRank(v.Stage) < StageRank("read")) {
			return false
		}
		for _, in := range v.Intents {
			if !in.Queued {
				return false
			}
		}
		later(v.Report.At, v.LastSyncErrorAt)
		for _, t := range v.Observed {
			later(t)
		}
		for _, t := range v.RemoteObserved {
			later(t)
		}
	}
	return !newest.After(cutoff)
}

// Finished is the ids of the messages that are done with and untouched since cutoff, in id order.
func (s Store) Finished(cutoff time.Time) ([]string, error) {
	all, _, err := s.Scan()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range all {
		if Finished(m, cutoff) {
			ids = append(ids, m.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// Archive moves one message's file into the archive directory, replacing an older archived copy of the same id.
func (s Store) Archive(id string) error {
	if !hashID.MatchString(id) {
		return fmt.Errorf("invalid message ID")
	}
	dir := filepath.Join(s.Dir, ArchiveDir)
	if err := fsx.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(s.Dir, id+".json"), filepath.Join(dir, id+".json")); err != nil {
		return err
	}
	return errors.Join(fsx.SyncDir(dir), fsx.SyncDir(s.Dir))
}

// Reported is what was already said about a failure: a file or message by name, with the error it gave then. A
// reconcile that finds the same failure again says nothing new.
func (s Store) Reported() map[string]string {
	raw, err := os.ReadFile(filepath.Join(s.Dir, reportedFile))
	if err != nil {
		return nil
	}
	var out map[string]string
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// SetReported replaces what is remembered as said. Nothing is written when it is the same as before, and an empty
// set removes the file.
func (s Store) SetReported(said map[string]string) error {
	path := filepath.Join(s.Dir, reportedFile)
	if len(said) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if maps.Equal(said, s.Reported()) {
		return nil
	}
	return writeAtomic(s.Dir, reportedFile, said)
}
