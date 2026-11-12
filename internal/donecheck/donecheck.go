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
	"strings"
	"time"
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

// Spec is what to run, as a task or the crew file names it.
type Spec struct {
	// Run is the shell line, run as /bin/sh -c in the task's worktree.
	Run string `yaml:"run" json:"run"`
	// Timeout is how long it may run (10m by default, 30m at most); empty is the default.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Commit asks for a commit beyond the task's base as well: a branch nothing was committed on does not pass.
	Commit bool `yaml:"commit,omitempty" json:"commit,omitempty"`
}

// UnmarshalYAML reads a string (the command) or a mapping of the three keys. A key it does not know is an error, not
// a warning: a mistyped commit or timeout would otherwise be a gate that silently asks for less.
func (s *Spec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&s.Run)
	}

	for i := 0; i+1 < len(n.Content); i += 2 {
		switch key := n.Content[i]; key.Value {
		case "run", "timeout", "commit":
		default:
			return fmt.Errorf("line %d: a check has no key %q (run, timeout, commit)", key.Line, key.Value)
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

// Validate says what is wrong with the spec, nil when it can be run.
func (s Spec) Validate() error {
	switch {
	case strings.TrimSpace(s.Run) == "":
		return errors.New("a check needs a command to run")
	case len(s.Run) > runMax:
		return fmt.Errorf("a check's command is at most %d bytes", runMax)
	case !utf8.ValidString(s.Run) || strings.ContainsRune(s.Run, 0):
		return errors.New("a check's command is text")
	}

	if s.Timeout == "" {
		return nil
	}

	d, err := time.ParseDuration(s.Timeout)

	switch {
	case err != nil || d <= 0:
		return fmt.Errorf("check timeout %q: a span such as 90s or 10m", s.Timeout)
	case d > MaxTimeout:
		return fmt.Errorf("check timeout %s: at most %s", s.Timeout, MaxTimeout)
	}

	return nil
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
	out := s.Run + " (" + Span(s.Limit())

	if s.Commit {
		out += ", a commit beyond the base"
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
	// TimedOut says the timeout ended it; Err says why else it has no status of its own (could not start, a signal,
	// stopped by the caller).
	TimedOut bool   `json:"timed_out,omitempty"`
	Err      string `json:"error,omitempty"`
	// Output is the tail of what it printed, bounded (see Tail); Truncated says there was more before it.
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Passed reports whether the command ran and exited 0.
func (r Result) Passed() bool { return r.Exit == 0 && !r.TimedOut && r.Err == "" }

// Duration is how long the command ran.
func (r Result) Duration() time.Duration { return time.Duration(r.DurationMS) * time.Millisecond }

// Failure is why the run did not pass, in one phrase; empty when it passed.
func (r Result) Failure(limit time.Duration) string {
	switch {
	case r.Passed():
		return ""
	case r.TimedOut:
		return "timed out after " + Span(limit)
	case r.Err != "":
		return r.Err
	default:
		return fmt.Sprintf("exit %d", r.Exit)
	}
}
