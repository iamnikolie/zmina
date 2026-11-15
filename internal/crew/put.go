package crew

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoCrew is the refusal of Put for a crew file that is not there when it may not be made.
var ErrNoCrew = errors.New("no crew file there")

// PutOptions say how a crew file sent by the owner's node is taken (Put).
type PutOptions struct {
	// Node is the node the file is for, whoever the machine says it is: the file has to load for it.
	Node string
	// Base is the hash of the file the push replaces, "" for none; Create lets the crew be made when there is none.
	Base   string
	Create bool
	// By and How are what the history says of the change ("mesh alpha", "crew push").
	By, How string
}

// PutResult is what Put did.
type PutResult struct {
	Saved Saved
	// Wrote is false when the file there was already this one.
	Wrote bool
	// Sum is the hash of the file there now; Warnings what loading it said of it.
	Sum      string
	Warnings []string
}

// Put puts raw in place of the crew file at path, as the owner's push: raw must load for the node (LoadFor) and declare nodes:
// before it replaces anything, a file that does not is refused and the one there stays; and it replaces only the file whose
// hash is o.Base (compare and swap, under the history's lock; a *BaseError says otherwise), so an edit made there
// meanwhile is never overwritten unseen. The change is kept in the crew file's history by o.By and o.How. With no file
// at path it makes the crew (its directory too) when o.Create, else the error is ErrNoCrew.
func Put(path string, raw []byte, o PutOptions) (PutResult, error) {
	var out PutResult

	dir := filepath.Dir(path)

	_, statErr := os.Stat(dir)
	madeDir := false

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if !o.Create {
			return out, ErrNoCrew
		}

		// A file that is not there is not the one a base names: said before anything is made.
		if o.Base != "" {
			return out, &BaseError{}
		}

		madeDir = errors.Is(statErr, os.ErrNotExist)

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return out, fmt.Errorf("crew: %w", err)
		}
	} else if err != nil {
		return out, fmt.Errorf("crew: %w", err)
	}

	// Whatever ends here without a file at path leaves no directory it made.
	done := false

	defer func() {
		if madeDir && !done {
			_ = os.Remove(dir)
		}
	}()

	c, err := loadPushed(dir, raw, o.Node)
	if err != nil {
		return out, err
	}

	out.Warnings = c.Warnings

	saved, wrote, err := HistoryOf(path).CommitIf(path, raw, o.Base, o.By, o.How)
	if err != nil {
		return out, err
	}

	done = true
	out.Saved, out.Wrote, out.Sum = saved, wrote, sumOf(raw)

	return out, nil
}

// loadPushed checks a pushed file in a temp file of the crew's own directory (so its relative paths and default name
// are the real ones') and returns the crew read for the node.
func loadPushed(dir string, raw []byte, node string) (*Crew, error) {
	tmp, err := os.CreateTemp(dir, ".crew-push-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("crew: %w", err)
	}

	name := tmp.Name()

	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()

		return nil, fmt.Errorf("crew: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("crew: %w", err)
	}

	c, err := LoadFor(name, node)
	if err != nil {
		return nil, fmt.Errorf("the file does not load on this node, so it is not taken: %s",
			strings.ReplaceAll(err.Error(), "crew: "+name+": ", ""))
	}

	// The file goes to every node in the end: a key that is not per node under another node's entry is a warning here, and the
	// error that stops that node, so it is not taken.
	for _, n := range c.NodeNames() {
		if err := checkOverlay(n, c.Nodes[n]); err != nil {
			return nil, fmt.Errorf("the file would not load on %s, so it is not taken: %w", n, err)
		}
	}

	if !c.Spread() {
		return nil, errors.New("the file declares no nodes:, so it is not a crew over nodes: a push is for one file on every node " +
			"(a crew that runs on one node is not pushed)")
	}

	for i, w := range c.Warnings {
		c.Warnings[i] = "crew.yaml" + strings.TrimPrefix(w, filepath.Base(name))
	}

	return c, nil
}
