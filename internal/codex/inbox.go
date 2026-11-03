package codex

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/peer"
	"github.com/iamnikolie/zmina/internal/registry"
)

// The bridge's inbox: zmina writes one file per line (<nanos>-<id>.line), the bridge starts a turn with the oldest
// once its thread is idle and moves the file to taken/<id>. Nothing is typed into the bridge's console, so the owner's
// input there is never mixed with zmina's lines.

// InboxDir is the inbox of the bridge that runs the session.
func InboxDir(stateDir, session string) string { return filepath.Join(stateDir, "inbox", session) }

// line is one queued line.
type line struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Inbox posts lines to a bridge's inbox (shift.Peer).
type Inbox struct{}

// Usable reports whether the session is a bridge that takes lines from its inbox.
func (Inbox) Usable(s *registry.Session) bool {
	if s == nil || s.Inbox == "" {
		return false
	}

	st, err := os.Stat(s.Inbox)

	return err == nil && st.IsDir()
}

// Post queues one line for the bridge and returns its id; the file appears whole (rename), or not at all.
func (i Inbox) Post(_ string, s *registry.Session, text string) (string, error) {
	if !i.Usable(s) {
		return "", fmt.Errorf("%w: the bridge has no inbox", peer.ErrUnavailable)
	}

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("%w: %v", peer.ErrUnavailable, err)
	}

	id := hex.EncodeToString(b)
	raw, _ := json.Marshal(line{ID: id, Text: text})
	tmp := filepath.Join(s.Inbox, "."+id+".tmp")

	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", fmt.Errorf("%w: %v", peer.ErrUnavailable, err)
	}

	name := strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + id + ".line"
	if err := os.Rename(tmp, filepath.Join(s.Inbox, name)); err != nil {
		_ = os.Remove(tmp)

		return "", fmt.Errorf("%w: %v", peer.ErrUnavailable, err)
	}

	return id, nil
}

// Delivered reports whether the bridge started a turn with the line: dir is the inbox.
func (Inbox) Delivered(dir, _, id string) (bool, error) {
	if dir == "" || id == "" || filepath.Base(id) != id {
		return false, nil
	}

	_, err := os.Stat(filepath.Join(dir, "taken", id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

// openInbox makes the bridge's inbox and drops taken marks older than a day.
func openInbox(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "taken"), 0o700); err != nil {
		return err
	}

	marks, _ := os.ReadDir(filepath.Join(dir, "taken"))
	for _, m := range marks {
		if info, err := m.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(filepath.Join(dir, "taken", m.Name()))
		}
	}

	return nil
}

// nextLine is the oldest queued line, nil when none waits.
func nextLine(dir string) (*line, string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.line"))
	if err != nil || len(files) == 0 {
		return nil, "", err
	}

	slices.SortFunc(files, func(a, b string) int {
		na, _ := strconv.ParseInt(strings.SplitN(filepath.Base(a), "-", 2)[0], 10, 64)
		nb, _ := strconv.ParseInt(strings.SplitN(filepath.Base(b), "-", 2)[0], 10, 64)

		return int(na - nb)
	})

	raw, err := os.ReadFile(files[0])
	if err != nil {
		return nil, "", err
	}

	var l line
	if err := json.Unmarshal(raw, &l); err != nil || l.ID == "" || filepath.Base(l.ID) != l.ID {
		// A broken file would block the queue: it is set aside.
		_ = os.Rename(files[0], files[0]+".broken")

		return nil, "", nil
	}

	return &l, files[0], nil
}

// take marks a line as started in a turn.
func take(dir, path, id string) error {
	if err := os.WriteFile(filepath.Join(dir, "taken", id), nil, 0o600); err != nil {
		return err
	}

	return os.Remove(path)
}
