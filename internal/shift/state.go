package shift

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/watch"
)

// stateVersion is the format this zmina writes. Files without a version are the first format and are converted on
// read; a file from a newer zmina is never rewritten.
const stateVersion = 2

// LockWait is how long a command waits for another zmina holding the lock before it gives up.
var LockWait = 30 * time.Second

var (
	// ErrStateBroken is returned for a state file that cannot be read; nothing writes over it.
	ErrStateBroken = errors.New("state file unreadable")
	// ErrStateFuture is returned for a state file written by a newer zmina.
	ErrStateFuture = errors.New("state file from a newer zmina")
)

// State is what zmina remembers between calls, in state.json next to the crew file. Some of it is what zmina saw
// (up, started, stretches already reported) and could be seen again; most is what it was told or decided (holds,
// broken marks, queues, last sessions, schedule progress) and exists nowhere else.
type State struct {
	Version int `json:"version"`
	// Roles is per role and instance.
	Roles map[string]*RoleState `json:"roles,omitempty"`
	// Signals wait for the dispatcher: work found by zmina note and the watches. They are refused at intake when
	// too many wait, never dropped once accepted.
	Signals []string `json:"signals,omitempty"`
	// Notes wait for the dispatcher too: the ticker's own health notes, deduplicated and capped.
	Notes []string `json:"notes,omitempty"`
	// Watch holds, per watch, its stored keys, held events, failures and cache.
	Watch map[string]*watch.State `json:"watch,omitempty"`
	// LastTick is when the ticker last beat, so a stopped ticker shows.
	LastTick time.Time `json:"last_tick,omitzero"`
	// Stopped is set while the shift is stopped: the ticker starts nothing.
	Stopped time.Time `json:"stopped,omitzero"`

	// Legacy is set when the file read was in the first format (converted in memory, written as this one on
	// the next save).
	Legacy bool `json:"-"`
}

// RoleState is what zmina keeps of one role.
type RoleState struct {
	// Up is whether the role was up when zmina last looked; a role that was up and is not has ended.
	Up bool `json:"up,omitempty"`
	// Started is when zmina last started (or tried to start) it.
	Started time.Time `json:"started,omitzero"`
	// StuckAt and IdleAt are the busy and idle stretches (their statusUpdatedAt) already reported.
	StuckAt int64 `json:"stuck_at,omitempty"`
	IdleAt  int64 `json:"idle_at,omitempty"`
	// QuickEnds counts the ends in a row that came soon after a start.
	QuickEnds int `json:"quick_ends,omitempty"`
	// Downing is set while a down waits for the role to exit, so the ticker leaves it alone meanwhile.
	Downing time.Time `json:"downing,omitzero"`

	// Instance marks an extra copy started with up --as (dev#2); it is forgotten once down.
	Instance bool `json:"instance,omitempty"`
	// Held roles were put down by hand: the ticker leaves them down until the next up.
	Held bool `json:"held,omitempty"`
	// Broken roles die on boot: the ticker stops starting them until the next hand up.
	Broken bool `json:"broken,omitempty"`
	// LastSession is the role's last Claude Code session id, for --resume.
	LastSession string `json:"last_session,omitempty"`
	// Inbox holds lines typed into the role the next time it is idle; Attempts counts failed tries of the first.
	Inbox    []string `json:"inbox,omitempty"`
	Attempts int      `json:"attempts,omitempty"`
	// Failed holds lines that could not be typed after several tries, for zmina queue.
	Failed []Failed `json:"failed,omitempty"`
	// NextRun is the next scheduled run, counted from the schedule in NextSpec.
	NextRun  time.Time `json:"next_run,omitzero"`
	NextSpec string    `json:"next_spec,omitempty"`
	// Expired is the expires value already reported; SkipNoted the tasks a skipped run was reported for.
	Expired   string `json:"expired,omitempty"`
	SkipNoted string `json:"skip_noted,omitempty"`
	// Trimmed is when the ticker last typed /compact or /clear into the role (context_max). TrimSession and
	// TrimBase are the conversation after it and the size it first showed: a conversation that starts over the
	// limit is not trimmed again.
	Trimmed     time.Time `json:"trimmed,omitzero"`
	TrimPending bool      `json:"trim_pending,omitempty"`
	TrimSession string    `json:"trim_session,omitempty"`
	TrimBase    int       `json:"trim_base,omitempty"`
	// NoPrompt is since when the role's screen has shown no input line (a dialog, a login, a crash); BlockedNoted
	// that the dispatcher heard of this stretch. ErrorNoted is the API error last reported, LimitNoted the usage
	// limit line last reported from the screen.
	NoPrompt     time.Time `json:"no_prompt,omitzero"`
	BlockedNoted bool      `json:"blocked_noted,omitempty"`
	ErrorNoted   string    `json:"error_noted,omitempty"`
	LimitNoted   string    `json:"limit_noted,omitempty"`
	// Now is the role's last status line on its screen (see NowLine) as the ticker last read it, NowAt since when it
	// has read that line.
	Now   string    `json:"now,omitempty"`
	NowAt time.Time `json:"now_at,omitzero"`
	// Sent is the last line typed into the role, until the role shows it took it.
	Sent *Sent `json:"sent,omitempty"`
	// Dir is where the role was started when not its crew dir (zmina up --task: the task's worktree); Trusted
	// that zmina answered Claude Code's "trust this folder" there.
	Dir     string `json:"dir,omitempty"`
	Trusted bool   `json:"trusted,omitempty"`
}

// Sent is a typed line not yet seen to land: the role's status has not changed since.
type Sent struct {
	Line  string    `json:"line"`
	At    time.Time `json:"at"`
	Tries int       `json:"tries,omitempty"`
}

// Failed is a queued line that could not be typed.
type Failed struct {
	Line  string    `json:"line"`
	Error string    `json:"error"`
	At    time.Time `json:"at"`
}

// Role is the state of one role, made on first use.
func (s *State) Role(name string) *RoleState {
	r := s.Roles[name]
	if r == nil {
		r = &RoleState{}
		s.Roles[name] = r
	}

	return r
}

// Locked is the state held under an exclusive lock, so the ticker and a hand-run command never interleave.
type Locked struct {
	*State

	path string
	lock *os.File
	// legacy is the raw first-format file this state was converted from, kept as a backup on the first save.
	legacy []byte
}

// Lock takes the lock in dir and reads the state. It waits up to LockWait for another zmina, then fails naming it.
// A state file that cannot be read, or one from a newer zmina, is an error: nothing may write over it.
func Lock(dir string) (*Locked, error) {
	f, err := takeLock(dir)
	if err != nil {
		return nil, err
	}

	l := &Locked{path: crew.Layout{Dir: dir}.State(), lock: f}

	raw, err := os.ReadFile(l.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		l.Release()

		return nil, fmt.Errorf("state: %w", err)
	}

	st, legacy, err := decode(raw)
	if err != nil {
		l.Release()

		return nil, stateError(l.path, err)
	}

	l.State = st
	if legacy {
		l.legacy = raw
	}

	return l, nil
}

// Read reads the state in dir without the lock, for commands that only look: the file is replaced whole (rename),
// so a read never sees half of it. An unreadable file gives an empty state and the error, so a look still works.
func Read(dir string) (*State, error) {
	path := crew.Layout{Dir: dir}.State()

	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return NewState(), fmt.Errorf("state: %w", err)
	}

	st, _, err := decode(raw)
	if err != nil {
		return NewState(), stateError(path, err)
	}

	return st, nil
}

// Recover moves an unreadable state file aside as state.json.broken-<time> so zmina starts fresh, and returns the
// new name. A readable file is left alone.
func Recover(dir string, now time.Time) (string, error) {
	f, err := takeLock(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	path := crew.Layout{Dir: dir}.State()

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("state: %w", err)
	}

	if _, _, err := decode(raw); err == nil {
		return "", errors.New("state.json reads fine; nothing to recover")
	}

	aside := path + ".broken-" + now.Format("20060102-150405")

	return aside, os.Rename(path, aside)
}

func stateError(path string, err error) error {
	if errors.Is(err, ErrStateFuture) {
		return fmt.Errorf("%s: %w; upgrade zmina (nothing was changed)", path, err)
	}

	return fmt.Errorf("%s: %w (%v); nothing was changed — 'zmina state recover' moves it aside and starts fresh",
		path, ErrStateBroken, err)
}

// takeLock opens and locks the lock file in dir, waiting up to LockWait, and writes this pid into it.
func takeLock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}

	path := crew.Layout{Dir: dir}.Lock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}

	deadline := time.Now().Add(LockWait)

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()

			return nil, fmt.Errorf("state: lock: %w", err)
		}

		if time.Now().After(deadline) {
			_ = f.Close()

			holder := "another zmina"
			if raw, err := os.ReadFile(path); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
					holder = "zmina pid " + strconv.Itoa(pid)
				}
			}

			return nil, fmt.Errorf("state: %s has held %s for %s; try again, or see what it is doing (ps -p)",
				holder, path, LockWait)
		}

		time.Sleep(100 * time.Millisecond)
	}

	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)

	return f, nil
}

// Save writes the state and releases the lock. The first save of a converted first-format file keeps the original
// as state.json.v1.bak.
func (l *Locked) Save() error {
	defer l.Release()

	if l.legacy != nil {
		bak := l.path + ".v1.bak"
		if _, err := os.Stat(bak); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(bak, l.legacy, 0o600); err != nil {
				return fmt.Errorf("state: %w", err)
			}
		}
	}

	l.Version = stateVersion

	raw, err := json.MarshalIndent(l.State, "", "  ")
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	tmp := l.path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}

	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	return os.Rename(tmp, l.path)
}

// Release drops the lock without writing.
func (l *Locked) Release() {
	if l.lock != nil {
		_ = l.lock.Close()
		l.lock = nil
	}
}

// NewState is an empty state.
func NewState() *State {
	s := &State{Version: stateVersion}
	s.init()

	return s
}

func (s *State) init() {
	if s.Roles == nil {
		s.Roles = map[string]*RoleState{}
	}

	if s.Watch == nil {
		s.Watch = map[string]*watch.State{}
	}

	for name, r := range s.Roles {
		if r == nil {
			delete(s.Roles, name)
		}
	}
}

// Prune drops what is remembered of roles that are no longer in the crew.
func (s *State) Prune(keep func(name string) bool) {
	for name := range s.Roles {
		if !keep(name) {
			delete(s.Roles, name)
		}
	}
}

// decode reads a state file of either format; an empty file is an empty state. legacy reports a first-format file.
func decode(raw []byte) (st *State, legacy bool, err error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return NewState(), false, nil
	}

	var probe struct {
		Version *int `json:"version"`
	}

	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false, err
	}

	if probe.Version == nil {
		st, err := decodeV1(raw)

		return st, true, err
	}

	if *probe.Version > stateVersion {
		return nil, false, fmt.Errorf("%w (version %d, this zmina writes %d)", ErrStateFuture, *probe.Version,
			stateVersion)
	}

	st = &State{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, false, err
	}

	st.init()

	return st, false, nil
}

// stateV1 is the first format: one map per fact, keyed by role.
type stateV1 struct {
	Started     map[string]time.Time    `json:"started"`
	Up          map[string]bool         `json:"up"`
	Held        map[string]bool         `json:"held"`
	Stuck       map[string]int64        `json:"stuck"`
	Notes       []string                `json:"notes"`
	Instances   map[string]bool         `json:"instances"`
	LastSession map[string]string       `json:"last_session"`
	Idle        map[string]int64        `json:"idle"`
	QuickEnds   map[string]int          `json:"quick_ends"`
	Broken      map[string]bool         `json:"broken"`
	Inbox       map[string][]string     `json:"inbox"`
	NextRun     map[string]time.Time    `json:"next_run"`
	Expired     map[string]string       `json:"expired"`
	NextSpec    map[string]string       `json:"next_spec"`
	SkipNoted   map[string]string       `json:"skip_noted"`
	Watch       map[string]*watch.State `json:"watch"`
}

func decodeV1(raw []byte) (*State, error) {
	var v stateV1
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}

	st := NewState()
	st.Legacy = true
	// The first format mixed task signals and health notes in one queue; all of it is kept as signals, which are
	// never dropped.
	st.Signals = v.Notes

	if v.Watch != nil {
		st.Watch = v.Watch
	}

	for k, x := range v.Started {
		st.Role(k).Started = x
	}

	for k, x := range v.Up {
		st.Role(k).Up = x
	}

	for k, x := range v.Held {
		st.Role(k).Held = x
	}

	for k, x := range v.Stuck {
		st.Role(k).StuckAt = x
	}

	for k, x := range v.Instances {
		st.Role(k).Instance = x
	}

	for k, x := range v.LastSession {
		st.Role(k).LastSession = x
	}

	for k, x := range v.Idle {
		st.Role(k).IdleAt = x
	}

	for k, x := range v.QuickEnds {
		st.Role(k).QuickEnds = x
	}

	for k, x := range v.Broken {
		st.Role(k).Broken = x
	}

	for k, x := range v.Inbox {
		st.Role(k).Inbox = x
	}

	for k, x := range v.NextRun {
		st.Role(k).NextRun = x
	}

	for k, x := range v.Expired {
		st.Role(k).Expired = x
	}

	for k, x := range v.NextSpec {
		st.Role(k).NextSpec = x
	}

	for k, x := range v.SkipNoted {
		st.Role(k).SkipNoted = x
	}

	st.init()

	return st, nil
}
