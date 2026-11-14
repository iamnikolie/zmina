package plugin

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// chosenKeep is how long what the owner chose of a dialog is remembered: it is read only to say how a dialog that went
// from the screen ended, within a beat or two.
const chosenKeep = 2 * 24 * time.Hour

// Chosen is the choice made of a dialog: who made it, which (0 for none: the owner's own words to the question a role's turn ended
// on), its words, when.
type Chosen struct {
	By   string    `json:"by"`
	N    int       `json:"n"`
	Text string    `json:"text,omitempty"`
	At   time.Time `json:"at"`
}

// Owner is the owner's mark (plugins/owner.json): when he last answered in a terminal, which is a sign that he is at the
// machine (quiet_when_active), and what was chosen of each dialog, so that a dialog that is gone from a screen is said to
// be chosen, and by whom. It is written by the answer commands under the state lock; the plugins' run only reads it.
type Owner struct {
	Active time.Time         `json:"active,omitzero"`
	Chosen map[string]Chosen `json:"chosen,omitempty"`
}

// ReadOwner reads the mark; an empty one when it is not there or does not read.
func ReadOwner(path string) Owner {
	var o Owner

	f, err := os.Open(path)
	if err != nil {
		return o
	}

	defer func() { _ = f.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(f, readMax))
	if json.Unmarshal(raw, &o) != nil {
		return Owner{}
	}

	return o
}

// UpdateOwner changes the mark. The caller holds the state lock, which is what keeps two writers apart. A failure to
// write is returned and is no reason to fail an answer.
func UpdateOwner(path string, now time.Time, fn func(o *Owner)) error {
	o := ReadOwner(path)
	fn(&o)

	for key, c := range o.Chosen {
		if now.Sub(c.At) > chosenKeep {
			delete(o.Chosen, key)
		}
	}

	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}

	if err := fsx.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return fsx.WriteFile(path, append(raw, '\n'), 0o600)
}
