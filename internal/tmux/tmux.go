// Package tmux runs zmina's own tmux server: one detached tmux session per role, on a socket no other tmux shares.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Timeout bounds every tmux call: a hung server must not hang the ticker or hold zmina's lock.
var Timeout = 10 * time.Second

// minVersion is the oldest tmux zmina works with (new-session -e).
const minVersion = 3.2

// Client talks to zmina's tmux server.
type Client struct {
	// Bin is the tmux binary.
	Bin string
	// Socket is the tmux server's name (tmux -L), one per crew.
	Socket string
	// Drop lists environment variables (NAME, or PREFIX* ) kept out of the server, on top of DefaultDrop.
	Drop []string
}

// fallback is where Homebrew puts tmux, for callers that run with a minimal PATH (a launchd job: /usr/bin:/bin).
var fallback = []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux"}

// New finds tmux on PATH, else where Homebrew puts it, checks its version and talks to the server named socket.
func New(socket string) (*Client, error) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		bin = ""

		for _, f := range fallback {
			if fi, serr := os.Stat(f); serr == nil && fi.Mode()&0o111 != 0 {
				bin = f

				break
			}
		}

		if bin == "" {
			return nil, errors.New("tmux is not on PATH: brew install tmux (or your package manager's tmux)")
		}
	}

	c := &Client{Bin: bin, Socket: socket}

	if v, err := c.version(); err == nil && v < minVersion {
		return nil, fmt.Errorf("tmux %.1f is too old; zmina needs %.1f or newer", v, minVersion)
	}

	return c, nil
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

func (c *Client) version() (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, c.Bin, "-V").Output()
	if err != nil {
		return 0, err
	}

	m := versionRe.FindStringSubmatch(string(out))
	if m == nil {
		return 0, fmt.Errorf("tmux -V: %q", out)
	}

	return strconv.ParseFloat(m[1]+"."+m[2], 64)
}

// State is what tmux says of a role's session.
type State int

const (
	// Absent: no such session (or no server at all).
	Absent State = iota
	// Up: the role's process runs.
	Up
	// Dead: the session is there but the role's process ended; its screen is kept (remain-on-exit).
	Dead
)

// Pane is a role's pane: the one zmina tagged when it started the role (@zmina), else — for a session started before
// zmina tagged panes — the first window's first pane. The owner may open more windows or panes when attached; every
// command aims at this one.
type Pane struct {
	State State
	ID    string
	PID   int
}

// tag marks the role's pane.
const tag = "@zmina"

// Probe reads a role's session. An error means tmux could not be asked (a timeout, a broken server) — not that the
// session is gone: callers must not take it as absence.
func (c *Client) Probe(name string) (Pane, error) {
	// "=name:" — without the colon tmux 3.8 reads the target as a window too and takes an unambiguous prefix:
	// "=dev" would find dev3's panes while dev is down.
	out, err := c.run("list-panes", "-s", "-t", "="+name+":", "-F",
		"#{pane_id}\t#{"+tag+"}\t#{pane_dead}\t#{pane_pid}")
	if err != nil {
		if absent(err) {
			return Pane{State: Absent}, nil
		}

		return Pane{}, err
	}

	var first, tagged *Pane

	// Only newlines are trimmed: a dead pane has no pid, so its line ends in a tab that is a field separator.
	for _, line := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[0] == "" {
			continue
		}

		p := Pane{ID: f[0], State: Up}
		if f[2] == "1" {
			p.State = Dead
		} else {
			p.PID, _ = strconv.Atoi(f[3])
		}

		if first == nil {
			first = &p
		}

		if f[1] != "" && tagged == nil {
			tagged = &p
		}
	}

	switch {
	case tagged != nil:
		return *tagged, nil
	case first != nil:
		return *first, nil
	default:
		return Pane{State: Absent}, nil
	}
}

// absent tells "there is no such session or server" from a failure to ask.
func absent(err error) bool {
	msg := err.Error()

	for _, m := range []string{"can't find session", "can't find window", "can't find pane", "no server running",
		"error connecting to", "session not found"} {
		if strings.Contains(msg, m) {
			return true
		}
	}

	return false
}

// target is the pane commands go to, or an error when the role has no session.
func (c *Client) target(name string) (string, error) {
	p, err := c.Probe(name)
	if err != nil {
		return "", err
	}

	if p.State == Absent {
		return "", fmt.Errorf("tmux: no session %s", name)
	}

	return p.ID, nil
}

// Has reports whether a session is up and its process still runs.
func (c *Client) Has(name string) bool {
	p, err := c.Probe(name)

	return err == nil && p.State == Up
}

// Dead reports whether a session is there but its process has ended.
func (c *Client) Dead(name string) bool {
	p, err := c.Probe(name)

	return err == nil && p.State == Dead
}

// PanePID is the pid of the process the role runs (claude itself: it is started without a shell).
func (c *Client) PanePID(name string) (int, error) {
	p, err := c.Probe(name)
	if err != nil {
		return 0, err
	}

	if p.State != Up {
		return 0, fmt.Errorf("tmux: %s is not running", name)
	}

	return p.PID, nil
}

// Start opens a detached session in dir that runs argv with env on top of the server's environment, and tags its
// pane. The role's window stays after its process ends (remain-on-exit, set on that window only) until it is killed,
// so its last screen can be captured; other windows close as usual.
func (c *Client) Start(name, dir string, env []string, argv []string) error {
	// A dead session under the name (the ticker has not collected it yet) would refuse the new one.
	if c.Dead(name) {
		_ = c.Kill(name)
	}

	first := "=" + name + ":{start}.{top-left}"

	args := []string{"new-session", "-d", "-s", name, "-c", dir, "-x", "220", "-y", "60"}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}

	args = append(args, "--")
	for _, a := range argv {
		args = append(args, escape(a))
	}

	args = append(args, ";", "set-option", "-p", "-t", first, tag, "1",
		";", "set-option", "-w", "-t", "="+name+":{start}", "remain-on-exit", "on")

	_, err := c.run(args...)

	return err
}

// KeepScreens tags the role's pane and sets remain-on-exit on its window, for sessions started before zmina did
// both at start.
func (c *Client) KeepScreens(name string) error {
	id, err := c.target(name)
	if err != nil {
		return err
	}

	if _, err := c.run("set-option", "-p", "-t", id, tag, "1"); err != nil {
		return err
	}

	_, err = c.run("set-option", "-w", "-t", id, "remain-on-exit", "on")

	return err
}

// sgr matches one terminal colour or style sequence.
var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Prompt reads Claude Code's input line on the role's screen: what is typed into it, and whether it is there at
// all (a dialog has none). The input line is a ❯ line right under a rule (─────); a dialog's ❯ marks its
// selected answer and has none above it. A dimmed placeholder counts as empty; a draft that starts with an empty
// line is read from its first line that is not.
func (c *Client) Prompt(name string) (draft string, ok bool, err error) {
	id, err := c.target(name)
	if err != nil {
		return "", false, err
	}

	out, err := c.run("capture-pane", "-p", "-e", "-t", id)
	if err != nil {
		return "", false, err
	}

	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		plain := strings.TrimSpace(sgr.ReplaceAllString(lines[i], ""))
		if !strings.HasPrefix(plain, "❯") || !underRule(lines[:i]) {
			continue
		}

		rest := lines[i][strings.Index(lines[i], "❯")+len("❯"):]
		// Claude Code puts a no-break space after the ❯.
		if strings.HasPrefix(strings.TrimLeftFunc(rest, unicode.IsSpace), "\x1b[2m") {
			return "", true, nil
		}

		if draft := strings.TrimSpace(sgr.ReplaceAllString(rest, "")); draft != "" {
			return draft, true, nil
		}

		for _, l := range lines[i+1:] {
			plain := strings.TrimSpace(sgr.ReplaceAllString(l, ""))
			if strings.HasPrefix(plain, "───") {
				break
			}

			if plain != "" {
				return plain, true, nil
			}
		}

		return "", true, nil
	}

	return "", false, nil
}

// underRule reports whether the nearest non-empty line at the end of above is a rule.
func underRule(above []string) bool {
	for i := len(above) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(sgr.ReplaceAllString(above[i], "")); l != "" {
			return strings.HasPrefix(l, "───")
		}
	}

	return false
}

// Attached reports whether a terminal is attached to the role's session: someone may be typing into it.
func (c *Client) Attached(name string) (bool, error) {
	id, err := c.target(name)
	if err != nil {
		return false, err
	}

	out, err := c.run("display-message", "-p", "-t", id, "#{session_attached}")
	if err != nil {
		return false, err
	}

	n := strings.TrimSpace(out)

	return n != "" && n != "0", nil
}

// Capture is the last n lines of the role's screen and scrollback, as text, trailing blank lines dropped.
func (c *Client) Capture(name string, n int) (string, error) {
	id, err := c.target(name)
	if err != nil {
		return "", err
	}

	out, err := c.run("capture-pane", "-p", "-J", "-t", id, "-S", "-"+strconv.Itoa(n+200))
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimRight(out, "\n "), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}

	// A dead pane shows "Pane is dead (...)" on its bottom row, below empty rows: keep it under the last output.
	var status []string
	if last := len(lines) - 1; last >= 0 && strings.HasPrefix(lines[last], "Pane is dead") {
		status = lines[last:]
		lines = lines[:last]

		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	}

	lines = append(lines, status...)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n"), nil
}

// Keys presses named keys (Enter, Down, Escape, ...) in the role's pane.
func (c *Client) Keys(name string, keys ...string) error {
	id, err := c.target(name)
	if err != nil {
		return err
	}

	_, err = c.run(append([]string{"send-keys", "-t", id}, keys...)...)

	return err
}

// Kill ends a session and what runs in it.
func (c *Client) Kill(name string) error {
	_, err := c.run("kill-session", "-t", "="+name)

	return err
}

// Type types one line into the role's pane and presses Enter. The text goes after -- so a leading dash is text.
func (c *Client) Type(name, text string) error {
	id, err := c.target(name)
	if err != nil {
		return err
	}

	if _, err := c.run("send-keys", "-t", id, "-l", "--", escape(text)); err != nil {
		return err
	}

	_, err = c.run("send-keys", "-t", id, "Enter")

	return err
}

// escape keeps a trailing semicolon of an argument: tmux reads an argument ending in ';' as a command separator.
func escape(arg string) string {
	if strings.HasSuffix(arg, ";") {
		return strings.TrimSuffix(arg, ";") + "\\;"
	}

	return arg
}

// AttachArgv is the command that attaches the terminal to a session.
func (c *Client) AttachArgv(name string) []string {
	return []string{c.Bin, "-u", "-L", c.Socket, "attach-session", "-t", "=" + name}
}

func (c *Client) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Bin, append([]string{"-u", "-L", c.Socket}, args...)...)
	cmd.Env = CleanEnv(os.Environ(), c.Drop)

	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("tmux %s: no answer in %s", args[0], Timeout)
	}

	if err != nil {
		return "", fmt.Errorf("tmux %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}

	return string(out), nil
}

// DefaultDrop are the variables that tie a process to the session or terminal it runs in. The first zmina call
// starts the tmux server and every role inherits its environment: a role started from inside a Claude Code session
// or a terminal app must not look like a child of it. Provider settings (CLAUDE_CODE_USE_BEDROCK, ANTHROPIC_*) pass.
var DefaultDrop = []string{
	"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_MESSAGING_*", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_EXECPATH", "CLAUDE_PID", "CLAUDE_EFFORT",
	"CLAUDE_CONFIG_DIR", "ZMINA_ROLE", "TMUX", "TMUX_PANE", "CMUX_*", "TERM_SESSION_ID", "ITERM_SESSION_ID",
	"__CFBundleIdentifier",
}

// CleanEnv drops DefaultDrop and the extra names (NAME, or PREFIX*) from env.
func CleanEnv(env, extra []string) []string {
	drop := append(append([]string{}, DefaultDrop...), extra...)
	out := make([]string, 0, len(env))

	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !matches(k, drop) {
			out = append(out, kv)
		}
	}

	return out
}

func matches(name string, patterns []string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if name == p {
			return true
		}
	}

	return false
}
