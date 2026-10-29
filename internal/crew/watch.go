package crew

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Watch kinds.
const (
	// WatchExec runs a command that prints the current events as JSON lines.
	WatchExec = "exec"
	// WatchGitLabMR finds review threads and conflicts on the author's open GitLab merge requests, or answers,
	// pushes and review requests on the merge requests a reviewer is asked to review, through gl.
	WatchGitLabMR = "gitlab-mr"
)

// Watch voices.
const (
	VoicesReviewers = "reviewers"
	VoicesAnyone    = "anyone"
)

const (
	defaultWatchTimeout = 2 * time.Minute
	defaultForgetAfter  = 14 * 24 * time.Hour
	defaultCommand      = "/agent"
	minEvery            = time.Minute
)

var watchRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// Watch is a periodic detector the ticker runs: it finds work and queues notes for the dispatcher, never does it.
type Watch struct {
	// Name is the watch's key in the crew file and the source of its notes.
	Name string `yaml:"-"`
	Kind string `yaml:"kind"`
	// Every, Quiet, Timeout and ForgetAfter are spans: 90s, 10m, 2h, 14d.
	Every       string `yaml:"every"`
	Quiet       string `yaml:"quiet"`
	Timeout     string `yaml:"timeout"`
	ForgetAfter string `yaml:"forget_after"`
	// Enabled false skips the watch and keeps its state.
	Enabled *bool `yaml:"enabled"`
	// Baseline (default true) records the first run's events without noting them.
	Baseline *bool `yaml:"baseline"`

	// Run is the exec kind's command: a list is argv, a string runs with /bin/sh -c.
	Run Command `yaml:"run"`
	// Dir is where the exec command runs (default: the crew's dir).
	Dir string `yaml:"dir"`

	// GLConfig is the gl profile (it sets the GitLab host).
	GLConfig string `yaml:"gl_config"`
	// Author is whose open merge requests: me (the token owner) or a username.
	Author string `yaml:"author"`
	// Reviewer watches the open merge requests this user reviews instead: me or a username.
	Reviewer string `yaml:"reviewer"`
	// Projects are path prefixes of a merge request's project; empty is every project.
	Projects []string `yaml:"projects"`
	// Voices are whose notes count besides the author's: reviewers or anyone. With reviewer:, reviewers counts
	// only the merge request's author as the one who answers.
	Voices string `yaml:"voices"`
	// Allow are more usernames counted as voices.
	Allow []string `yaml:"allow"`
	// Command is the author's instruction prefix (default /agent); "" turns it off.
	Command *string `yaml:"command"`
	// Conflicts (default true) notes merge conflicts of merge requests with a reviewer.
	Conflicts *bool `yaml:"conflicts"`
	// Drafts includes draft merge requests.
	Drafts bool `yaml:"drafts"`
	// Info also notes new comments outside open threads.
	Info bool `yaml:"info"`

	every, quiet, timeout, forgetAfter time.Duration
}

// Command is a command line: argv, or one string for /bin/sh -c.
type Command struct {
	Argv  []string
	Shell string
}

// UnmarshalYAML reads a list (argv) or a string (shell).
func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		return n.Decode(&c.Argv)
	}

	return n.Decode(&c.Shell)
}

// MarshalYAML writes it back the way it was read.
func (c Command) MarshalYAML() (any, error) {
	if c.Argv != nil {
		return c.Argv, nil
	}

	return c.Shell, nil
}

// Empty reports a command with nothing to run.
func (c Command) Empty() bool { return len(c.Argv) == 0 && strings.TrimSpace(c.Shell) == "" }

// Line is the command line to start: argv as given, or /bin/sh -c for a string.
func (c Command) Line() []string {
	if c.Argv != nil {
		return c.Argv
	}

	return []string{"/bin/sh", "-c", c.Shell}
}

// EveryD is how often the watch runs.
func (w *Watch) EveryD() time.Duration { return w.every }

// QuietD is how long an event that moved is held before it is noted.
func (w *Watch) QuietD() time.Duration { return w.quiet }

// TimeoutD bounds one run.
func (w *Watch) TimeoutD() time.Duration { return w.timeout }

// ForgetAfterD is how long a key that is gone is remembered.
func (w *Watch) ForgetAfterD() time.Duration { return w.forgetAfter }

// Fingerprint identifies the watch's configuration, to tell whether it changed while a run went.
func (w *Watch) Fingerprint() string {
	raw, _ := yaml.Marshal(w)
	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:8])
}

// On reports whether the ticker runs the watch.
func (w *Watch) On() bool { return w.Enabled == nil || *w.Enabled }

// Baselines reports whether the first run only records.
func (w *Watch) Baselines() bool { return w.Baseline == nil || *w.Baseline }

// CommandPrefix is the author's instruction prefix, empty when turned off.
func (w *Watch) CommandPrefix() string {
	if w.Command == nil {
		return defaultCommand
	}

	return strings.TrimSpace(*w.Command)
}

// NotesConflicts reports whether merge conflicts are noted.
func (w *Watch) NotesConflicts() bool { return w.Conflicts == nil || *w.Conflicts }

// WatchNames lists the watches by name.
func (c *Crew) WatchNames() []string {
	names := make([]string, 0, len(c.Watches))
	for n := range c.Watches {
		names = append(names, n)
	}

	sort.Strings(names)

	return names
}

// Watch returns a watch by name.
func (c *Crew) Watch(name string) (*Watch, error) {
	w, ok := c.Watches[name]
	if !ok {
		return nil, fmt.Errorf("no watch %q in %s (watches: %s)", name, c.Path, strings.Join(c.WatchNames(), ", "))
	}

	return w, nil
}

func (c *Crew) checkWatches() error {
	for _, name := range c.WatchNames() {
		w := c.Watches[name]
		if w == nil {
			return fmt.Errorf("watch %s: needs a kind", name)
		}

		w.Name = name
		if err := w.Check(c); err != nil {
			return fmt.Errorf("watch %s: %w", name, err)
		}
	}

	return nil
}

// Check validates a watch and fills its defaults; the crew gives the default dir.
func (w *Watch) Check(c *Crew) error {
	if !watchRe.MatchString(w.Name) {
		return errors.New("names are lowercase words joined by dashes")
	}

	// The dispatcher tells a watch's "<name>: ..." signal from a role's health note by the name.
	if _, clash := c.Roles[w.Name]; clash {
		return errors.New("a role has this name; give the watch its own")
	}

	var err error

	if w.every, err = span("every", w.Every, 0); err != nil {
		return err
	}

	if w.every < minEvery {
		return fmt.Errorf("every: at least %s (the ticker beats every minute)", minEvery)
	}

	if w.quiet, err = span("quiet", w.Quiet, 0); err != nil {
		return err
	}

	if w.timeout, err = span("timeout", w.Timeout, defaultWatchTimeout); err != nil {
		return err
	}

	if w.timeout <= 0 {
		return errors.New("timeout must be positive")
	}

	if w.forgetAfter, err = span("forget_after", w.ForgetAfter, defaultForgetAfter); err != nil {
		return err
	}

	switch w.Kind {
	case WatchExec:
		if w.Run.Empty() {
			return errors.New("kind exec needs run: a list (argv) or a string (/bin/sh -c)")
		}

		w.Dir = ExpandHome(or(w.Dir, c.Dir))
	case WatchGitLabMR:
		if w.GLConfig == "" {
			return errors.New("kind gitlab-mr needs gl_config: the gl profile")
		}

		if w.Author != "" && w.Reviewer != "" {
			return errors.New("set author: or reviewer:, not both (two watches for both sides)")
		}

		if w.Reviewer == "" {
			w.Author = or(w.Author, "me")
		}

		// The reviewer side has no author's instructions and no conflicts of its own.
		if w.Reviewer != "" && w.Command != nil {
			c.Warnings = append(c.Warnings, "watch "+w.Name+": command does not apply with reviewer:, ignored")
		}

		if w.Reviewer != "" && w.Conflicts != nil {
			c.Warnings = append(c.Warnings, "watch "+w.Name+": conflicts does not apply with reviewer:, ignored")
		}

		w.Voices = or(w.Voices, VoicesReviewers)

		if w.Voices != VoicesReviewers && w.Voices != VoicesAnyone {
			return fmt.Errorf("voices is %s or %s, not %q", VoicesReviewers, VoicesAnyone, w.Voices)
		}
	case "":
		return fmt.Errorf("needs a kind: %s or %s", WatchGitLabMR, WatchExec)
	default:
		return fmt.Errorf("kind %q: %s or %s", w.Kind, WatchGitLabMR, WatchExec)
	}

	return nil
}

// span reads a field's span, or the fallback when it is empty.
func span(field, s string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return fallback, nil
	}

	d, err := ParseSpan(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}

	return d, nil
}

// ParseSpan reads a Go duration (90s, 10m, 2h) or a number of days (14d).
func ParseSpan(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q: 90s, 10m, 2h or 14d", s)
		}

		return time.Duration(n) * 24 * time.Hour, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q: 90s, 10m, 2h or 14d", s)
	}

	return d, nil
}
