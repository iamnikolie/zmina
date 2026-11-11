package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
)

// limitsTapCommand is the status line of a role (roleSettings wraps the owner's own): Claude Code runs it on every
// redraw with its JSON on stdin. It writes what that JSON says about the plan's limits to the crew's limits file
// (zmina limits reads it), then runs the command it wraps with the same bytes on stdin and prints its output as it
// is, so the status line looks as it did. Without a wrapped command it prints nothing. It never breaks the status
// line: whatever goes wrong with the tap is dropped, the wrapped command runs anyway, and its exit code is the
// tap's own.
//
// The wrapped command is what follows --: one word is a shell command line as the owner wrote it in settings, more
// words are a program and its arguments.
func (a *app) limitsTapCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "_limits-tap [-- command...]",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		// A status line's stderr is Claude Code's to show: the tap adds nothing of its own to it.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, wrapped []string) error {
			in := cmd.InOrStdin()

			var input []byte
			if piped(in) {
				input, _ = io.ReadAll(in)
			}

			a.tapLimits(input, time.Now())

			return a.runWrapped(wrapped, input)
		},
	}
}

// tapLimits records the status line's limits. Every failure is dropped, a panic included: the tap is only ever a
// side effect of drawing the status line.
func (a *app) tapLimits(input []byte, now time.Time) {
	defer func() { _ = recover() }()

	path, err := filepath.Abs(a.crewPath())
	if err != nil {
		return
	}

	// The crew's directory is where its files live; one that is not there is no crew to write for, and the tap
	// makes none.
	if st, err := os.Stat(filepath.Dir(path)); err != nil || !st.IsDir() {
		return
	}

	_, _ = limits.Tap(crew.Layout{Dir: filepath.Dir(path)}.Limits(), input, now)
}

// runWrapped runs the wrapped status line on the bytes the tap read, its output going straight to ours. Its exit
// code is passed on.
func (a *app) runWrapped(wrapped []string, input []byte) error {
	if len(wrapped) == 0 {
		return nil
	}

	var c *exec.Cmd
	if len(wrapped) == 1 {
		c = exec.Command("/bin/sh", "-c", wrapped[0])
	} else {
		c = exec.Command(wrapped[0], wrapped[1:]...)
	}

	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(input), a.out, a.err

	err := c.Run()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit):
		code := exit.ExitCode()
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}

		return &exitError{code: code}
	}

	// The wrapped command could not be started at all (a program that is not there): say so as a shell does.
	fmt.Fprintf(a.err, "zmina: _limits-tap: %v\n", err)

	return &exitError{code: 127}
}
