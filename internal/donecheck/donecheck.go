// Package donecheck is the command that closing a task as done may run: a shell line, in the task's worktree, under a
// timeout, with its output bounded. What it says is narrow on purpose: this command ran at this commit and exited with
// this status. It does not say the work is right.
//
// Nothing here knows the ledger, the crew or the state lock: the caller reads what it needs under the lock, calls Run
// with none held, and writes the Result back under the lock.
package donecheck

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The timeout a check has when it names none, and the longest one it may name.
const (
	DefaultTimeout = 10 * time.Minute
	MaxTimeout     = 30 * time.Minute
)

// runMax is the longest command line a check may be.
const runMax = 2000

// The exit statuses a shell gives when it could not run the command at all: found but not executable, not found. They
// say the environment is broken (a tool missing from the seat), not that the work failed the check.
const (
	exitNotExecutable = 126
	exitNotFound      = 127
)

// Spec is what to run, as a task or the crew file names it.
type Spec struct {
	// Run is the shell line, run as /bin/sh -c in the task's worktree.
	Run string `yaml:"run" json:"run"`
	// Timeout is how long it may run (10m by default, 30m at most); empty is the default.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Commit asks for a commit beyond the task's base as well: a branch nothing was committed on does not pass.
	Commit bool `yaml:"commit,omitempty" json:"commit,omitempty"`
	// EnvFaultRC is an exit status (3 to 255; 0 is unset) that this check uses to say it could not run its own
	// tests: a missing tool, a service that is down. Like the shell's 126 and 127 it is an environment fault, not a
	// failed check (Result.Kind). 1 and 2 are the statuses of a check that ran and failed, and are refused.
	EnvFaultRC int `yaml:"env_fault_rc,omitempty" json:"env_fault_rc,omitempty"`
}

// Off reports a spec that says there is no check: the crew file's done_check given as off or none, or as nothing.
func (s Spec) Off() bool {
	switch strings.ToLower(strings.TrimSpace(s.Run)) {
	case "", "off", "none":
		return s.Timeout == "" && !s.Commit && s.EnvFaultRC == 0
	}

	return false
}

// UnmarshalYAML reads a string (the command) or a mapping of the four keys. A key it does not know is an error, not
// a warning: a mistyped commit or timeout would otherwise be a gate that silently asks for less.
func (s *Spec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&s.Run)
	}

	for i := 0; i+1 < len(n.Content); i += 2 {
		switch key := n.Content[i]; key.Value {
		case "run", "timeout", "commit":
		case "env_fault_rc":
			// 0 is how an unset one reads, so a 0 written out is a mistake, not a choice.
			if v, err := strconv.ParseInt(n.Content[i+1].Value, 0, 64); err == nil && v == 0 {
				return fmt.Errorf("line %d: env_fault_rc is an exit status from 3 to 255, not 0", key.Line)
			}
		default:
			return fmt.Errorf("line %d: a check has no key %q (run, timeout, commit, env_fault_rc)", key.Line, key.Value)
		}
	}

	type plain Spec

	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}

	*s = Spec(p)

	return nil
}

// Validate says what is wrong with the spec, nil when it can be run. A command is one line of plain text: a control
// character in it (a line break, an escape sequence, a bidirectional override) could not be told from what it shows.
func (s Spec) Validate() error {
	run := strings.TrimSpace(s.Run)

	switch {
	case run == "":
		return errors.New("a check needs a command to run")
	case len(s.Run) > runMax:
		return fmt.Errorf("a check's command is at most %d bytes", runMax)
	case !utf8.ValidString(run) || strings.IndexFunc(run, unprintable) >= 0:
		return errors.New("a check's command is one line of plain text, with no control characters (a script goes in a " +
			"file the command runs)")
	}

	if s.EnvFaultRC != 0 {
		if err := CheckEnvFaultRC(s.EnvFaultRC); err != nil {
			return fmt.Errorf("check %w", err)
		}
	}

	if s.Timeout == "" {
		return nil
	}

	d, err := time.ParseDuration(s.Timeout)

	switch {
	case err != nil || d <= 0:
		return fmt.Errorf("check timeout %q: a span such as 90s or 10m", s.Timeout)
	case d > MaxTimeout:
		return fmt.Errorf("check timeout %s: at most %s", s.Timeout, Span(MaxTimeout))
	}

	return nil
}

// CheckEnvFaultRC says what is wrong with a status named as env_fault_rc, nil when it can be one. 1 and 2 are refused: a
// test runner, a compiler or make exit them (a general error, a misused command, a failed run) for a check that ran and
// failed, and a spec that called them an environment fault would turn every failed check into one that judges nothing. The
// shell's own 126 and 127 are faults without it.
func CheckEnvFaultRC(rc int) error {
	switch {
	case rc == 1 || rc == 2:
		return fmt.Errorf("env_fault_rc %d is the status of a check that ran and failed (a test run, a compiler, make): name "+
			"a status from 3 to 255 that only a broken environment exits (126 and 127, the shell's, are faults already)", rc)
	case rc < 0 || rc > 255:
		return fmt.Errorf("env_fault_rc %d: an exit status from 3 to 255", rc)
	default:
		return nil
	}
}

// unprintable is a rune that is no part of plain text: a control character (C0, DEL, C1), a format character (zero
// width, bidirectional marks and overrides, the byte order mark) or a line or paragraph separator.
func unprintable(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// Limit is how long the check may run.
func (s Spec) Limit() time.Duration {
	if d, err := time.ParseDuration(s.Timeout); err == nil && d > 0 {
		return min(d, MaxTimeout)
	}

	return DefaultTimeout
}

// String is the spec in one line: the command, its limit, and the commit it asks for.
func (s Spec) String() string {
	out := Line(s.Run) + " (" + Span(s.Limit())

	if s.Commit {
		out += ", a commit beyond the base"
	}

	if s.EnvFaultRC != 0 {
		out += fmt.Sprintf(", exit %d is an environment fault", s.EnvFaultRC)
	}

	return out + ")"
}

// Span is d as a person writes it: 10m, 90s, 1h, not 10m0s.
func Span(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	default:
		return d.String()
	}
}

// The places a task's check comes from.
const (
	// FromTask is a check set on the task by the owner or the dispatcher.
	FromTask = "task"
	// FromCrew is the crew file's done_check, for a task that sets none.
	FromCrew = "crew"
)

// Result is what one run of a check came to.
type Result struct {
	// Run is the command that ran; Source says whose it was (FromTask, FromCrew).
	Run    string `json:"run"`
	Source string `json:"source,omitempty"`
	// Commit is the commit the worktree stood at when it ran.
	Commit string `json:"commit"`
	// At is when it started; Duration (in milliseconds) how long it ran.
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms"`
	// Exit is the command's exit status: 0 passes. -1 when it did not exit by itself (timed out, killed, did not start).
	Exit int `json:"exit"`
	// Kind is KindEnvironment when the run says nothing of the work: the shell could not run the command (exit 126 or
	// 127, or it did not start) or the command exited with the spec's EnvFaultRC. Empty for any other run.
	Kind string `json:"kind,omitempty"`
	// TimedOut says the timeout ended it, Interrupted that the caller stopped it (a signal to zmina); Err says why else it
	// has no status of its own (could not start, killed by a signal).
	TimedOut    bool   `json:"timed_out,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Err         string `json:"error,omitempty"`
	// Output is the tail of what it printed, bounded (see Tail); Truncated says there was more before it.
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// KindEnvironment is the Kind of a run that could not run the check: the environment is broken, and the work was not
// judged. A timeout is not one: it stays an ordinary failure.
const KindEnvironment = "environment"

// Environment reports a run that says nothing of the work (see KindEnvironment).
func (r Result) Environment() bool { return r.Kind == KindEnvironment }

// Fault is what an environment fault was, in one phrase; empty for any other run. It is what a fault is told once for.
func (r Result) Fault() string {
	switch {
	case !r.Environment():
		return ""
	case r.Err != "":
		return r.Err
	case r.Exit == exitNotExecutable:
		return "exit 126: the shell found the command and could not execute it"
	case r.Exit == exitNotFound:
		return "exit 127: the shell did not find the command"
	default:
		return fmt.Sprintf("exit %d: the status the check names as an environment fault (env_fault_rc)", r.Exit)
	}
}

// Passed reports whether the command ran and exited 0.
func (r Result) Passed() bool { return r.Exit == 0 && !r.TimedOut && !r.Interrupted && r.Err == "" }

// Duration is how long the command ran.
func (r Result) Duration() time.Duration { return time.Duration(r.DurationMS) * time.Millisecond }

// Failure is why the run did not pass, in one phrase; empty when it passed.
func (r Result) Failure(limit time.Duration) string {
	switch {
	case r.Passed():
		return ""
	case r.TimedOut:
		return "timed out after " + Span(limit)
	case r.Interrupted:
		return "interrupted"
	case r.Err != "":
		return r.Err
	default:
		return fmt.Sprintf("exit %d", r.Exit)
	}
}
