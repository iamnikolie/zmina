// Package crew reads the crew file: which role sessions work the shift, where they start, how they boot, and which
// of them the ticker keeps up.
package crew

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/iamnikolie/zmina/internal/cron"
)

// Keep says what the ticker does with a role that is down.
type Keep string

const (
	// KeepManual roles run only when someone starts them.
	KeepManual Keep = "manual"
	// KeepAlways roles are started again by the ticker whenever they are down.
	KeepAlways Keep = "always"
)

const (
	defaultNamespace    = "~/.claude"
	defaultDir          = "~"
	defaultAgents       = "agents"
	defaultDispatcher   = "dispatcher"
	defaultStuckMinutes = 90
	defaultIdleMinutes  = 30
	defaultBootGrace    = 2 * time.Minute
	defaultPrefix       = "zm-"
	socket              = "zmina"
	launchdLabel        = "io.iamnikolie.zmina"
	bootMax             = 16 * 1024
)

var (
	roleRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,59}$`)
)

// Role is one long-lived named session.
type Role struct {
	// Name is the role's key in the crew file; it is also its tmux session.
	Name string `yaml:"-"`
	// Session is the Claude Code session name other sessions message it by (default <prefix><role>).
	Session string `yaml:"session"`
	// Dir is the working directory (default: the crew's dir).
	Dir string `yaml:"dir"`
	// Boot is the first message (default: read the role's AGENT.md and boot, when there is one).
	Boot string `yaml:"boot"`
	// Keep is manual or always; the dispatcher is always.
	Keep Keep `yaml:"keep"`
	// Model is passed to claude as --model when set.
	Model string `yaml:"model"`
	// Args are extra claude flags, placed before the session name.
	Args []string `yaml:"args"`
	// About is one line on what the role takes; the dispatcher routes by it.
	About string `yaml:"about"`
	// RemoteControl overrides the crew's remote_control for this role.
	RemoteControl *bool `yaml:"remote_control"`
	// Resume "always" starts the role on its last conversation (claude --resume) whenever one is known.
	Resume string `yaml:"resume"`
	// Schedule is a five-field cron spec; at each time the ticker starts the role if needed and types Prompt.
	Schedule string `yaml:"schedule"`
	// Prompt is the one line a scheduled run types into the role.
	Prompt string `yaml:"prompt"`
	// Expires marks a temporary seat: past it, a role that is down is reported for removal.
	Expires string `yaml:"expires"`
	// ContextMax is how full the role's conversation may get (150k, 1m or a number of tokens) before the ticker
	// types ContextAction into it, once it sits idle; empty: the crew's context_max; off: never.
	ContextMax string `yaml:"context_max"`
	// DropTools are Claude Code tools this role starts without, on top of the crew's drop_tools; KeepTools are
	// tools of the crew's list this role keeps.
	DropTools []string `yaml:"drop_tools"`
	KeepTools []string `yaml:"keep_tools"`
	// ContextAction is compact (the default, or the crew's: /compact, the conversation goes on summarised) or clear
	// (/clear, then
	// the boot line again: a fresh conversation).
	ContextAction string `yaml:"context_action"`

	schedule   *cron.Schedule
	expiresAt  time.Time
	contextMax int
	dirErr     error
}

// The values of context_action.
const (
	ContextCompact = "compact"
	ContextClear   = "clear"
)

// contextFloor is the least context_max: a fresh session already fills tens of thousands of tokens (system prompt,
// tools, skills), and below that a role would be trimmed every pause.
const contextFloor = 50_000

// ContextLimit is the parsed context_max in tokens, 0 when the role has none.
func (r *Role) ContextLimit() int { return r.contextMax }

// ContextDo is what the ticker types when the role's conversation is past context_max: compact or clear.
func (r *Role) ContextDo() string { return or(r.ContextAction, ContextCompact) }

// ContextOff is the context_max value that keeps a role out of the crew's context_max.
const ContextOff = "off"

// DirErr says why the role's dir cannot be used (gone, not a directory), nil when it can. The crew still loads
// with such a role, so one stale seat never stops the ticker; the role itself does not start.
func (r *Role) DirErr() error { return r.dirErr }

// ResumeAlways is the Resume value that resumes the role's last conversation on every start.
const ResumeAlways = "always"

// Scheduled is the role's parsed schedule, nil when it has none.
func (r *Role) Scheduled() *cron.Schedule { return r.schedule }

// ExpiresAt is when a temporary seat expires; zero for a permanent one.
func (r *Role) ExpiresAt() time.Time { return r.expiresAt }

// MaxInstance is the highest copy number of one role (dev#2 ... dev#9).
const MaxInstance = 9

// Instance is copy n of the role: dev#2 under the session zm-dev-2, same dir, boot, model and args, never kept up,
// never scheduled, never resumed on its own.
func (r *Role) Instance(n int) *Role {
	cp := *r
	cp.Name = r.Name + "#" + strconv.Itoa(n)
	cp.Session = r.Session + "-" + strconv.Itoa(n)
	cp.Keep = KeepManual
	cp.Resume, cp.Schedule, cp.Prompt, cp.Expires = "", "", "", ""
	cp.schedule, cp.expiresAt = nil, time.Time{}
	cp.Args = slices.Clone(r.Args)
	cp.DropTools, cp.KeepTools = slices.Clone(r.DropTools), slices.Clone(r.KeepTools)

	return &cp
}

// Crew is the whole crew file.
type Crew struct {
	// Path is where the file was read from, made absolute; zmina's state lives next to it.
	Path string `yaml:"-"`
	// Warnings are what is wrong with the file without stopping it: unknown keys, a namespace that differs from
	// the caller's.
	Warnings []string `yaml:"-"`
	// Name sets a second crew apart from the first: its own tmux server and ticker (default: none).
	Name string `yaml:"name"`
	// Prefix starts every default session name (default zm-).
	Prefix string `yaml:"prefix"`
	// Namespace is the CLAUDE_CONFIG_DIR every role runs with.
	Namespace string `yaml:"namespace"`
	// Dir is the default working directory.
	Dir string `yaml:"dir"`
	// Agents holds <role>/AGENT.md boot documents (default: agents/ next to the crew file).
	Agents string `yaml:"agents"`
	// Claude is the claude binary (default: claude from PATH).
	Claude string `yaml:"claude"`
	// Dispatcher names the role that never sleeps and hears the ticker's notes.
	Dispatcher string `yaml:"dispatcher"`
	// StuckMinutes is how long a role may stay busy before the dispatcher hears of it.
	StuckMinutes int `yaml:"stuck_minutes"`
	// IdleMinutes is how long a role may sit idle without an active task before the dispatcher hears of it.
	IdleMinutes int `yaml:"idle_minutes"`
	// RemoteControl opens every role to Remote Control (claude --remote-control); default true.
	RemoteControl *bool `yaml:"remote_control"`
	// BootGrace is how long a started role is left to boot before lines are typed into it (default 2m).
	BootGrace string `yaml:"boot_grace"`
	// DropEnv lists environment variables (NAME, or PREFIX*) kept from the roles, on top of the built-in list.
	DropEnv []string `yaml:"drop_env"`
	// ContextMax and ContextAction are every role's context_max and context_action unless it sets its own.
	ContextMax    string `yaml:"context_max"`
	ContextAction string `yaml:"context_action"`
	// DropTools are Claude Code tools (Artifact, mcp__server) every role starts without (claude --disallowedTools):
	// each tool's description fills the context of every turn.
	DropTools []string `yaml:"drop_tools"`

	bootGrace time.Duration
	// Roles are keyed by role name.
	Roles map[string]*Role `yaml:"roles"`
	// Watches are periodic detectors the ticker runs, keyed by name; each queues notes for the dispatcher.
	Watches map[string]*Watch `yaml:"watch"`
	// Projects holds what `zmina ui` shows per project, keyed by the tasks' project name.
	Projects map[string]*Project `yaml:"projects"`
}

// Project is one project's look on the ui page.
type Project struct {
	// Tint pins the project's colour, p1 … p5, when the one picked from its name collides with another's.
	Tint string `yaml:"tint"`
}

// Tints are the projects' pinned tints, keyed by project name; a tint the page does not have is left out.
func (c *Crew) Tints() map[string]string {
	out := map[string]string{}

	for name, p := range c.Projects {
		if p != nil && tintRe.MatchString(p.Tint) {
			out[name] = p.Tint
		}
	}

	return out
}

var tintRe = regexp.MustCompile(`^p[1-5]$`)

// DefaultPath is $ZMINA_CREW, else ~/.zmina/crew.yaml.
func DefaultPath() string {
	if p := os.Getenv("ZMINA_CREW"); p != "" {
		return p
	}

	return ExpandHome("~/.zmina/crew.yaml")
}

// ConfigPath is the crew file of a named config: ~/.zmina/<name>/crew.yaml.
func ConfigPath(name string) string {
	return filepath.Join(ExpandHome("~/.zmina"), filepath.Base(name), "crew.yaml")
}

// Load reads, completes and checks a crew file. A role whose dir is missing loads with DirErr set.
func Load(path string) (*Crew, error) { return load(path, false) }

// LoadStrict reads a crew file and also refuses a role whose dir is not a directory: for checking an edit before
// it is written.
func LoadStrict(path string) (*Crew, error) { return load(path, true) }

func load(path string, strict bool) (*Crew, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("crew: %s does not exist; run 'zmina init' to write a starter", path)
	}

	if err != nil {
		return nil, fmt.Errorf("crew: %w", err)
	}

	if abs, err := filepath.Abs(path); err == nil {
		// A job (the ticker) runs from another directory: the crew is always known by its absolute path.
		path = abs
	}

	c, warnings, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("crew: %s: %w", path, err)
	}

	for name, p := range c.Projects {
		if p != nil && p.Tint != "" && !tintRe.MatchString(p.Tint) {
			warnings = append(warnings, fmt.Sprintf("project %s: tint %q is not one of p1 … p5, ignored", name, p.Tint))
		}
	}

	for _, w := range warnings {
		c.Warnings = append(c.Warnings, filepath.Base(path)+" "+w)
	}

	c.Path = path
	c.complete()

	if err := c.check(); err != nil {
		return nil, fmt.Errorf("crew: %s: %w", path, err)
	}

	if err := c.resolve(strict); err != nil {
		return nil, fmt.Errorf("crew: %s: %w", path, err)
	}

	if err := c.checkWatches(); err != nil {
		return nil, fmt.Errorf("crew: %s: %w", path, err)
	}

	return c, nil
}

// decode reads the crew file. Keys it does not know are warnings (with their line), not errors, so a typo is seen
// without stopping the crew.
func decode(raw []byte) (*Crew, []string, error) {
	var c Crew

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	err := dec.Decode(&c)
	if errors.Is(err, io.EOF) {
		return &c, nil, nil
	}

	var te *yaml.TypeError
	if err == nil || !errors.As(err, &te) {
		return &c, nil, err
	}

	var warnings []string

	for _, e := range te.Errors {
		if !strings.Contains(e, "not found in type") {
			return nil, nil, err
		}

		// "line 12: field scheduel not found in type crew.Role"
		line, rest, _ := strings.Cut(e, ": ")
		key := strings.TrimPrefix(strings.SplitN(rest, " not found", 2)[0], "field ")
		warnings = append(warnings, fmt.Sprintf("%s: unknown key %q, ignored", line, key))
	}

	c = Crew{}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, nil, err
	}

	return &c, warnings, nil
}

// StateDir is the directory zmina keeps its state and lock in (Layout names every file in it).
func (c *Crew) StateDir() string { return c.Layout().Dir }

// Socket is the crew's tmux server (tmux -L): zmina, or zmina-<name>.
func (c *Crew) Socket() string { return suffixed(socket, "-", c.Name) }

// LaunchdLabel is the crew's ticker: io.iamnikolie.zmina, or io.iamnikolie.zmina.<name>.
func (c *Crew) LaunchdLabel() string { return suffixed(launchdLabel, ".", c.Name) }

// BootGraceD is how long a started role is left to boot before lines are typed into it.
func (c *Crew) BootGraceD() time.Duration { return c.bootGrace }

// ToolsOff are the tools a role starts without: the crew's drop_tools and its own, less its keep_tools, each once.
// A role whose args pass --disallowedTools themselves gets none.
func (c *Crew) ToolsOff(r *Role) []string {
	if r.ownsTools() {
		return nil
	}

	return c.toolsListed(r)
}

func (c *Crew) toolsListed(r *Role) []string {
	var out []string

	for _, t := range append(slices.Clone(c.DropTools), r.DropTools...) {
		if !slices.Contains(r.KeepTools, t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}

	return out
}

// RemoteControlFor reports whether a role is opened to Remote Control: its own setting, else the crew's, else yes.
func (c *Crew) RemoteControlFor(r *Role) bool {
	switch {
	case r.RemoteControl != nil:
		return *r.RemoteControl
	case c.RemoteControl != nil:
		return *c.RemoteControl
	default:
		return true
	}
}

// SystemdUnit is the crew's ticker on Linux: zmina (.service and .timer), or zmina-<name>.
func (c *Crew) SystemdUnit() string { return suffixed("zmina", "-", c.Name) }

// Role returns a role by name.
func (c *Crew) Role(name string) (*Role, error) {
	r, ok := c.Roles[name]
	if !ok {
		return nil, fmt.Errorf("no role %q in %s (roles: %s)", name, c.Path, strings.Join(c.Names(), ", "))
	}

	return r, nil
}

// Names lists the roles, the dispatcher first, then by name.
func (c *Crew) Names() []string {
	names := make([]string, 0, len(c.Roles))
	for n := range c.Roles {
		if n != c.Dispatcher {
			names = append(names, n)
		}
	}

	sort.Strings(names)

	if _, ok := c.Roles[c.Dispatcher]; ok {
		names = append([]string{c.Dispatcher}, names...)
	}

	return names
}

func (c *Crew) complete() {
	home := filepath.Dir(c.Path)

	// The namespace has one default for every caller — a terminal, the ticker, a session — so one crew file never
	// resolves to two namespaces. 'zmina init' writes the caller's own explicitly.
	if c.Namespace == "" {
		if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" && ExpandHome(env) != ExpandHome(defaultNamespace) {
			c.Warnings = append(c.Warnings, fmt.Sprintf("namespace is not set, so roles use %s, while this session "+
				"runs in %s; set namespace: in the crew file", defaultNamespace, env))
		}
	}

	// A crew of its own directory (~/.zmina/<x>/crew.yaml) is named after it unless it says otherwise, so it never
	// shares the default crew's tmux server, ticker or session names.
	if c.Name == "" && filepath.Dir(home) == ExpandHome("~/.zmina") && roleRe.MatchString(filepath.Base(home)) {
		c.Name = filepath.Base(home)
	}

	if c.Prefix == "" && c.Name != "" {
		c.Prefix = c.Name + "-"
	}

	c.Namespace = ExpandHome(or(c.Namespace, defaultNamespace))
	c.Dir = anchor(home, ExpandHome(or(c.Dir, defaultDir)))
	c.Agents = anchor(home, ExpandHome(or(c.Agents, defaultAgents)))
	c.Claude = ExpandHome(or(c.Claude, "claude"))
	c.Dispatcher = or(c.Dispatcher, defaultDispatcher)
	c.Prefix = or(c.Prefix, defaultPrefix)

	if c.StuckMinutes == 0 {
		c.StuckMinutes = defaultStuckMinutes
	}

	if c.IdleMinutes == 0 {
		c.IdleMinutes = defaultIdleMinutes
	}

	for name, r := range c.Roles {
		if r == nil {
			r = &Role{}
			c.Roles[name] = r
		}

		r.Name = name
		r.Session = or(r.Session, c.Prefix+name)
		r.Dir = anchor(home, ExpandHome(or(r.Dir, c.Dir)))
		r.Keep = Keep(or(string(r.Keep), string(KeepManual)))
		r.ContextAction = or(r.ContextAction, c.ContextAction)

		if name == c.Dispatcher {
			r.Keep = KeepAlways
		}

		if r.Boot == "" {
			r.Boot = c.defaultBoot(r)
		}

		if r.ownsTools() && len(c.toolsListed(r)) > 0 {
			c.Warnings = append(c.Warnings, fmt.Sprintf("role %s: args has --disallowedTools, so drop_tools and "+
				"keep_tools are not applied to it; move the list to drop_tools", name))
		}
	}
}

// ownsTools reports whether the role's args already pass --disallowedTools, as crew files before drop_tools did;
// zmina then adds no second one.
func (r *Role) ownsTools() bool {
	for _, a := range r.Args {
		if strings.HasPrefix(a, "--disallowedTools") || strings.HasPrefix(a, "--disallowed-tools") {
			return true
		}
	}

	return false
}

// defaultBoot points the role at its AGENT.md, relative to its directory when the file is inside it.
func (c *Crew) defaultBoot(r *Role) string {
	doc := filepath.Join(c.Agents, r.Name, "AGENT.md")
	if _, err := os.Stat(doc); err != nil {
		return ""
	}

	if rel, err := filepath.Rel(r.Dir, doc); err == nil && !strings.HasPrefix(rel, "..") {
		doc = rel
	}

	return "read " + doc + " and boot"
}

// check validates what the file states; it changes nothing.
func (c *Crew) check() error {
	if len(c.Roles) == 0 {
		return errors.New("no roles")
	}

	if _, ok := c.Roles[c.Dispatcher]; !ok {
		return fmt.Errorf("dispatcher %q is not one of the roles", c.Dispatcher)
	}

	if c.Name != "" && !roleRe.MatchString(c.Name) {
		return fmt.Errorf("name %q: a lowercase word joined by dashes", c.Name)
	}

	if c.StuckMinutes < 0 {
		return errors.New("stuck_minutes must be positive")
	}

	if c.IdleMinutes < 0 {
		return errors.New("idle_minutes must be positive")
	}

	if err := checkTools(c.DropTools); err != nil {
		return err
	}

	if a := c.ContextAction; a != "" && a != ContextCompact && a != ContextClear {
		return fmt.Errorf("context_action is %s or %s, not %q", ContextCompact, ContextClear, a)
	}

	sessions := map[string]string{}

	for _, name := range c.Names() {
		r := c.Roles[name]

		if !roleRe.MatchString(name) {
			return fmt.Errorf("role %q: names are lowercase words joined by dashes", name)
		}

		if !sessionRe.MatchString(r.Session) {
			return fmt.Errorf("role %s: session %q: letters, digits, '.', '_' and '-' only", name, r.Session)
		}

		if other, dup := sessions[r.Session]; dup {
			return fmt.Errorf("roles %s and %s share the session %q", other, name, r.Session)
		}

		sessions[r.Session] = name

		if r.Keep != KeepManual && r.Keep != KeepAlways {
			return fmt.Errorf("role %s: keep is manual or always, not %q", name, r.Keep)
		}

		// claude reads a first message that starts with a dash as a flag.
		if strings.HasPrefix(strings.TrimSpace(r.Boot), "-") || len(r.Boot) > bootMax || !utf8.ValidString(r.Boot) {
			return fmt.Errorf("role %s: boot must start with a word and be at most %d bytes", name, bootMax)
		}

		if r.Resume != "" && r.Resume != ResumeAlways {
			return fmt.Errorf("role %s: resume is %q or empty, not %q", name, ResumeAlways, r.Resume)
		}

		if err := checkTools(append(slices.Clone(r.DropTools), r.KeepTools...)); err != nil {
			return fmt.Errorf("role %s: %w", name, err)
		}

		if a := r.ContextAction; a != "" && a != ContextCompact && a != ContextClear {
			return fmt.Errorf("role %s: context_action is %s or %s, not %q", name, ContextCompact, ContextClear, a)
		}
	}

	return nil
}

// resolve turns what the file states as text into values (boot_grace, schedules, expiry dates) and looks at the
// filesystem: a role whose dir is gone is marked (DirErr), and with strict refused.
func (c *Crew) resolve(strict bool) error {
	c.bootGrace = defaultBootGrace

	if c.BootGrace != "" {
		d, err := time.ParseDuration(c.BootGrace)
		if err != nil || d < 0 {
			return fmt.Errorf("boot_grace %q: a duration such as 90s or 3m", c.BootGrace)
		}

		c.bootGrace = d
	}

	crewMax, err := ParseTokens(c.ContextMax)
	if err != nil {
		return fmt.Errorf("context_max: %w", err)
	}

	for _, name := range c.Names() {
		r := c.Roles[name]

		r.dirErr = nil
		if st, err := os.Stat(r.Dir); err != nil || !st.IsDir() {
			r.dirErr = fmt.Errorf("role %s: dir %s is not a directory", name, r.Dir)
			if strict {
				return r.dirErr
			}
		}

		if err := r.parseSchedule(); err != nil {
			return fmt.Errorf("role %s: %w", name, err)
		}

		r.contextMax = crewMax

		if r.ContextMax == ContextOff {
			r.contextMax = 0
		} else if r.ContextMax != "" {
			if r.contextMax, err = ParseTokens(r.ContextMax); err != nil {
				return fmt.Errorf("role %s: context_max: %w", name, err)
			}
		}

		r.expiresAt = time.Time{}

		if r.Expires != "" {
			t, err := ParseTime(r.Expires)
			if err != nil {
				return fmt.Errorf("role %s: expires: %w", name, err)
			}

			r.expiresAt = t
		}
	}

	return nil
}

func (r *Role) parseSchedule() error {
	r.schedule = nil

	if r.Schedule == "" {
		if r.Prompt != "" {
			return errors.New("prompt without a schedule")
		}

		return nil
	}

	sched, err := cron.Parse(r.Schedule)
	if err != nil {
		return err
	}

	if strings.TrimSpace(r.Prompt) == "" || strings.ContainsAny(r.Prompt, "\r\n") || strings.HasPrefix(r.Prompt, "/") {
		return errors.New("a schedule needs a prompt: one line, not a slash command")
	}

	r.schedule = sched

	return nil
}

// checkTools refuses a tool name claude would read as two: one --disallowedTools value carries them all,
// comma-separated.
func checkTools(tools []string) error {
	for _, t := range tools {
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, ",\n") {
			return fmt.Errorf("drop_tools / keep_tools: %q is not a tool name", t)
		}

		if t == "ToolSearch" {
			return errors.New("drop_tools: not ToolSearch — without it every deferred tool loads in full")
		}
	}

	return nil
}

// ParseTokens reads a token count: 150000, 150k, 1.5m; empty is 0.
func ParseTokens(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}

	mult := 1.0
	if num, ok := strings.CutSuffix(s, "k"); ok {
		s, mult = num, 1e3
	} else if num, ok := strings.CutSuffix(s, "m"); ok {
		s, mult = num, 1e6
	}

	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil || !(f*mult >= contextFloor && f*mult <= 1e9) { // NaN fails both
		return 0, fmt.Errorf("a token count of at least %dk, such as 150k or 1m", contextFloor/1000)
	}

	return int(f * mult), nil
}

// ParseTime reads a date (2026-11-01, end of that day), a date and time (2026-11-01 18:00) in local time, or RFC 3339.
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}

	if t, err := time.ParseInLocation(time.DateOnly, s, time.Local); err == nil {
		return t.AddDate(0, 0, 1).Add(-time.Second), nil
	}

	return time.Time{}, fmt.Errorf("%q: a date (2026-11-01), date and time (2026-11-01 18:00) or RFC 3339", s)
}

// anchor makes a relative path relative to dir (the crew file's directory), so it means the same from any caller.
func anchor(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}

	return filepath.Join(dir, p)
}

// ExpandHome turns a leading ~ into the home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}

	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

func suffixed(base, sep, name string) string {
	if name == "" {
		return base
	}

	return base + sep + name
}

func or(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}

	return v
}
