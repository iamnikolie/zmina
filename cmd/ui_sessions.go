package cmd

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
)

// A browser signed in to the cockpit holds a session: a random 256-bit id it carries in its cookie, and a record of
// it in ~/.zmina/ui-sessions.json (0600) that keeps only the SHA-256 of the id, when the session began, when it was
// last seen and when it ends. The file alone cannot sign anyone in. A session ends 30 days after it was last seen,
// and at the latest 90 days after it began. The token that signed the browser in is never kept in the browser.
// zmina ui sessions lists the file and ends sessions in it; a running cockpit sees the change on its next request.

const (
	// uiSessionIdle is how long a session lasts without being seen; uiSessionMax is the longest it lasts at all.
	uiSessionIdle = 30 * 24 * time.Hour
	uiSessionMax  = 90 * 24 * time.Hour
	// uiSessionSeen is how often a session's last-seen time is written to the file: once a minute at most.
	uiSessionSeen = time.Minute
	// uiSessionsKept bounds the file: past it, the sessions seen longest ago go.
	uiSessionsKept = 64
)

func uiSessionsPath() string { return crew.ExpandHome("~/.zmina/ui-sessions.json") }

// uiSession is one signed-in browser as the file keeps it.
type uiSession struct {
	// Hash is the hex SHA-256 of the session id; the id itself is kept nowhere.
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	Expires  time.Time `json:"expires"`
}

type uiSessionFile struct {
	Sessions []uiSession `json:"sessions"`
}

// short is the id the owner sees and types: the first eight characters of the hash.
func (s uiSession) short() string { return s.Hash[:min(8, len(s.Hash))] }

// live reports whether the session still stands at now.
func (s uiSession) live(now time.Time) bool {
	return now.Before(s.Expires) && now.Sub(s.Created) < uiSessionMax && now.Sub(s.LastSeen) < uiSessionIdle
}

// seen is the session seen at now: its idle end moves, never past the longest life.
func (s uiSession) seen(now time.Time) uiSession {
	s.LastSeen = now
	s.Expires = minTime(now.Add(uiSessionIdle), s.Created.Add(uiSessionMax))

	return s
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}

	return a
}

func hashSessionID(id string) string {
	sum := sha256.Sum256([]byte(id))

	return hex.EncodeToString(sum[:])
}

// uiSessions is the cockpit's sessions: the file, and a copy of it in memory that is read again whenever the file
// changes (zmina ui sessions edits it from another process).
type uiSessions struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	list []uiSession
	// stamp is the file as the copy in memory was read; nil when there was none.
	stamp os.FileInfo
}

func newUISessions(path string, now func() time.Time) *uiSessions {
	return &uiSessions{path: path, now: now}
}

// refresh reads the file again when it is not the one the copy came from; the caller holds the lock. A file that
// cannot be read or parsed leaves no sessions: nobody is let in on its account.
func (s *uiSessions) refresh() {
	info, err := os.Stat(s.path)
	if err != nil {
		s.list, s.stamp = nil, nil

		return
	}

	if s.stamp != nil && os.SameFile(s.stamp, info) && s.stamp.ModTime().Equal(info.ModTime()) && s.stamp.Size() == info.Size() {
		return
	}

	s.list, s.stamp = nil, nil

	if list, err := readUISessions(s.path); err == nil {
		s.list, s.stamp = list, info
	}
}

// readUISessions is the file's sessions; none for a file that is not there.
func readUISessions(path string) ([]uiSession, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var f uiSessionFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return f.Sessions, nil
}

// update changes the file under a lock shared with every other zmina process: it reads the file as it is now, drops
// the sessions that ended, lets fn change the rest, and writes the result. The caller holds s.mu.
func (s *uiSessions) update(fn func([]uiSession) []uiSession) error {
	if err := fsx.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}

	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}

	defer func() { _ = lock.Close() }()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}

	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	// A file that does not parse is written over: it let nobody in anyway.
	list, _ := readUISessions(s.path)
	now := s.now()
	list = slices.DeleteFunc(list, func(e uiSession) bool { return !e.live(now) })
	list = fn(list)

	slices.SortStableFunc(list, func(a, b uiSession) int { return b.LastSeen.Compare(a.LastSeen) })

	if len(list) > uiSessionsKept {
		list = list[:uiSessionsKept]
	}

	raw, err := json.MarshalIndent(uiSessionFile{Sessions: list}, "", "  ")
	if err != nil {
		return err
	}

	if err := fsx.WriteFile(s.path, raw, 0o600); err != nil {
		return err
	}

	s.list, s.stamp = list, nil
	if info, err := os.Stat(s.path); err == nil {
		s.stamp = info
	}

	return nil
}

// find is the index of the session with this hash, or -1. Every stored hash is compared, in constant time, whether
// or not an earlier one matched.
func (s *uiSessions) find(hash string) int {
	at := -1

	for i := range s.list {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(s.list[i].Hash)) == 1 {
			at = i
		}
	}

	return at
}

// start begins a session and returns its id, for the cookie. The id is not kept.
func (s *uiSessions) start() (string, error) {
	id := randomHex(32)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	err := s.update(func(list []uiSession) []uiSession {
		return append(list, uiSession{Hash: hashSessionID(id), Created: now, LastSeen: now, Expires: now.Add(uiSessionIdle)})
	})
	if err != nil {
		return "", err
	}

	return id, nil
}

// valid reports whether id is a session that stands, and counts the request as the session's being seen: its idle
// end moves, written to the file once a minute at most. A session ended from another process is not valid.
func (s *uiSessions) valid(id string) bool {
	if id == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.refresh()

	now := s.now()
	hash := hashSessionID(id)

	at := s.find(hash)
	if at < 0 || !s.list[at].live(now) {
		return false
	}

	if now.Sub(s.list[at].LastSeen) < uiSessionSeen {
		return true
	}

	kept := true

	err := s.update(func(list []uiSession) []uiSession {
		kept = false

		for i := range list {
			if list[i].Hash == hash {
				list[i], kept = list[i].seen(now), true
			}
		}

		return list
	})
	if err != nil {
		// The file could not be written: the session stands for this run, and the next minute tries again.
		if at = s.find(hash); at >= 0 {
			s.list[at] = s.list[at].seen(now)
		}

		return true
	}

	return kept
}

// standing is valid that does not count as a sighting: for a stream that is open to check whether its session has
// been ended, which must not keep the session alive.
func (s *uiSessions) standing(id string) bool {
	if id == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.refresh()

	at := s.find(hashSessionID(id))

	return at >= 0 && s.list[at].live(s.now())
}

// end ends the session of id (sign-out).
func (s *uiSessions) end(id string) error {
	hash := hashSessionID(id)

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.update(func(list []uiSession) []uiSession {
		return slices.DeleteFunc(list, func(e uiSession) bool { return e.Hash == hash })
	})
}

// all is the sessions that stand, newest seen first.
func (s *uiSessions) all() ([]uiSession, error) {
	list, err := readUISessions(s.path)
	if err != nil {
		return nil, err
	}

	now := s.now()
	list = slices.DeleteFunc(list, func(e uiSession) bool { return !e.live(now) })
	slices.SortStableFunc(list, func(a, b uiSession) int { return b.LastSeen.Compare(a.LastSeen) })

	return list, nil
}

// revoke ends the session whose id starts with prefix (the short id zmina ui sessions prints); it must name one.
func (s *uiSessions) revoke(prefix string) error {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if len(prefix) < 4 {
		return errors.New("name the session by the id zmina ui sessions prints (at least 4 characters)")
	}

	var hit []string

	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.update(func(list []uiSession) []uiSession {
		for _, e := range list {
			if strings.HasPrefix(e.Hash, prefix) {
				hit = append(hit, e.short())
			}
		}

		if len(hit) != 1 {
			return list
		}

		return slices.DeleteFunc(list, func(e uiSession) bool { return strings.HasPrefix(e.Hash, prefix) })
	})
	if err != nil {
		return err
	}

	switch len(hit) {
	case 0:
		return fmt.Errorf("no session starts with %s", prefix)
	case 1:
		return nil
	}

	return fmt.Errorf("%s names %d sessions (%s): give more of the id", prefix, len(hit), strings.Join(hit, ", "))
}

// revokeAll ends every session and says how many there were.
func (s *uiSessions) revokeAll() (int, error) {
	n := 0

	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.update(func(list []uiSession) []uiSession {
		n = len(list)

		return nil
	})

	return n, err
}

// uiSessionsCommand is zmina ui sessions: what is signed in to the cockpit, and the way to end it.
func (a *app) uiSessionsCommand() *cobra.Command {
	var (
		revoke string
		all    bool
	)

	c := &cobra.Command{
		Use:   "sessions",
		Short: "List the browsers signed in to the cockpit; --revoke <id> or --all ends sessions",
		Long: "Lists the browsers signed in to the cockpit on this machine: a short id, when each signed in, when it " +
			"was last seen and when it ends (30 days after it was last seen, 90 days after it signed in at the " +
			"latest). --revoke <id> ends one by the id listed; --all ends every one. The browser has to sign in again " +
			"(a code from zmina connect); a running cockpit stops serving it, and closes its open streams, within a few " +
			"seconds. Reads and edits ~/.zmina/ui-sessions.json; the cockpit need not run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s := newUISessions(uiSessionsPath(), time.Now)

			switch {
			case revoke != "" && all:
				return errors.New("--revoke and --all together: name one session, or end all")
			case revoke != "":
				if err := s.revoke(revoke); err != nil {
					return err
				}

				fmt.Fprintln(a.out, "ended", strings.ToLower(strings.TrimSpace(revoke)))

				return nil
			case all:
				n, err := s.revokeAll()
				if err != nil {
					return err
				}

				fmt.Fprintf(a.out, "ended %d sessions\n", n)

				return nil
			}

			list, err := s.all()
			if err != nil {
				return err
			}

			if len(list) == 0 {
				fmt.Fprintln(a.out, "no browser is signed in")

				return nil
			}

			fmt.Fprintf(a.out, "%-8s  %-16s  %-16s  %s\n", "ID", "SIGNED IN", "LAST SEEN", "ENDS")

			const layout = "2006-01-02 15:04"

			for _, e := range list {
				fmt.Fprintf(a.out, "%-8s  %-16s  %-16s  %s\n", e.short(), e.Created.Local().Format(layout),
					e.LastSeen.Local().Format(layout), e.Expires.Local().Format(layout))
			}

			return nil
		},
	}

	c.Flags().StringVar(&revoke, "revoke", "", "end the session with this id (as listed)")
	c.Flags().BoolVar(&all, "all", false, "end every session")

	return c
}
