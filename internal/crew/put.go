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
	// Home is the zmina home the crew's files must lie in, links resolved ("" for no check).
	Home string
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

// Put puts raw in place of the crew file at path, as the owner's push: raw must load for the node (LoadFor), and for every
// other node it lists, and declare nodes: before it replaces anything, a file that does not is refused and the one there stays; and
// it replaces only the file whose hash is o.Base (compare and swap, under the history's lock; a *BaseError says otherwise), so an
// edit made there meanwhile is never overwritten unseen. The change is kept in the crew file's history by o.By and o.How. With no
// file at path it makes the crew (its directory too) when o.Create, else the error is ErrNoCrew. A push that fails leaves nothing it
// made. With o.Home set it writes nothing through a link that leaves that directory.
func Put(path string, raw []byte, o PutOptions) (PutResult, error) {
	var out PutResult

	if err := RefuseWhileMoving(path); err != nil {
		return out, err
	}

	dir := filepath.Dir(path)

	if o.Home != "" {
		if err := ContainedIn(o.Home, path, HistoryOf(path).Dir); err != nil {
			return out, err
		}
	}

	_, statErr := os.Stat(dir)
	_, histErr := os.Stat(filepath.Join(dir, "crew.d"))
	madeDir := false

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if !o.Create {
			return out, ErrNoCrew
		}

		// A file that is not there is not the one a base names: said before anything is made.
		if o.Base != "" {
			return out, &BaseError{}
		}

		if errors.Is(statErr, os.ErrNotExist) {
			// The directory is made by this call alone (Mkdir fails for a directory that is there), so that what a failure takes away
			// is only what it made.
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				return out, fmt.Errorf("crew: %w", err)
			}

			switch err := os.Mkdir(dir, 0o700); {
			case err == nil:
				madeDir = true
			case !errors.Is(err, os.ErrExist):
				return out, fmt.Errorf("crew: %w", err)
			}
		}
	} else if err != nil {
		return out, fmt.Errorf("crew: %w", err)
	}

	// Whatever ends here without a file at path leaves nothing this call made: the directory when it made it, else the history in it.
	done := false

	defer func() {
		switch {
		case done:
		case madeDir:
			_ = os.RemoveAll(dir)
		case errors.Is(histErr, os.ErrNotExist):
			_ = os.RemoveAll(filepath.Join(dir, "crew.d"))
		}
	}()

	c, err := loadPushed(dir, raw, o.Node)
	if err != nil {
		return out, err
	}

	out.Warnings = c.Warnings

	saved, wrote, err := HistoryOf(path).CommitIf(path, raw, o.Base, o.By, o.How)
	if err != nil {
		var be *BaseError
		// A file that is there and is no longer the base is the owner's to see: it is not this call's to take away, nor its history.
		if errors.As(err, &be) && be.Have != "" {
			done = true
		}

		return out, err
	}

	done = true
	out.Saved, out.Wrote, out.Sum = saved, wrote, sumOf(raw)

	return out, nil
}

// CheckPushed is what a node asks of a file it is sent: that it loads for node and for every node the file lists, and declares nodes:. It
// is read in a temp file of dir (the crew's own directory, so its relative paths and default name are the real ones') that is gone
// when it returns. The owner's node asks it of the file before it sends it, and the node that takes it asks again.
func CheckPushed(dir string, raw []byte, node string) (*Crew, error) {
	return loadPushed(dir, raw, node)
}

// ContainedIn refuses a path that is, or lies below, a link that leaves home: each of paths (the crew file, the directory it is in,
// where its history is kept) is resolved, links and all, and the part that is there must be home itself or below it, home being resolved
// too (a home that is a link, kept elsewhere on its machine, is a home). A link that stays inside home is a file there like any other.
func ContainedIn(home string, paths ...string) error {
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return fmt.Errorf("crew: the zmina home %s: %w", home, err)
	}

	for _, p := range paths {
		for _, q := range []string{p, filepath.Dir(p)} {
			real, err := resolveExisting(q)
			if err != nil {
				return fmt.Errorf("crew: %s: %w", q, err)
			}

			if rel, err := filepath.Rel(realHome, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%s leaves %s (it is, or is below, a link to %s): a push writes nothing through a link that leads out of "+
					"the zmina home; make it a plain directory and file there, or push another crew", q, home, real)
			}
		}
	}

	return nil
}

// resolveExisting is p with every link resolved, as far as p is there: the part that is not yet there follows the part that is.
func resolveExisting(p string) (string, error) {
	var tail []string

	for cur := filepath.Clean(p); ; {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}

			return real, nil
		}

		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}

		// A link that is there and leads nowhere is not a name that is free.
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", fmt.Errorf("a link that leads nowhere: %w", err)
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}

		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
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
		return nil, fmt.Errorf("the file does not load on %s, so it is not taken: %s", or(node, "this node"),
			strings.ReplaceAll(err.Error(), "crew: "+name+": ", ""))
	}

	// The file goes to every node in the end, and each reads it for itself: a key that is not per node under another node's entry is
	// a warning here and the error that stops that node, and a value one node's overlay makes of no use (a time zone, quiet hours, a
	// prefix) stops that node alone. A file one node takes and another does not is a crew split in two, so it is not taken.
	for _, n := range c.NodeNames() {
		if n == node {
			continue
		}

		if _, err := LoadFor(name, n); err != nil {
			return nil, fmt.Errorf("the file would not load on %s, so it is not taken: %s", n,
				strings.ReplaceAll(err.Error(), "crew: "+name+": ", ""))
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
