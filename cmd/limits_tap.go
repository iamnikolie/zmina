package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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
// tap's own. The wrapped command has wrappedTimeout: past it the tap ends quietly with what was printed so far, and
// the command's whole process group is killed, as it is when the tap itself is told to end.
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

// wrappedTimeout is how long the wrapped status line may take: Claude Code gives up on its own status line after a
// short while, and a command that never exits must not hold the pane or outlive the tap.
const wrappedTimeout = 5 * time.Second

// wrappedWaitDelay is how long the tap waits, once the wrapped command has ended or been killed, for the output
// pipes something it started still holds.
const wrappedWaitDelay = 500 * time.Millisecond

// runWrapped runs the wrapped status line on the bytes the tap read, its output going straight to ours. Its exit
// code is passed on.
func (a *app) runWrapped(wrapped []string, input []byte) error {
	return a.runWrappedWithin(wrapped, input, wrappedTimeout)
}

// runWrappedWithin is runWrapped with the time the command has. The command runs in a process group of its own, and
// the whole group is killed when the time is up and when the tap is told to end (SIGTERM, SIGINT, SIGHUP: the
// tap exits with 128 plus the signal), so nothing it started outlives either. A command that is out of time ends the
// tap quietly with what it printed so far: the status line shows that, and no error text of ours. A group is not
// killed once the command has ended by itself: a status line may leave a background refresh running on purpose.
func (a *app) runWrappedWithin(wrapped []string, input []byte, timeout time.Duration) error {
	if len(wrapped) == 0 {
		return nil
	}

	// Before the command starts, so a signal never finds it running unwatched.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	defer signal.Stop(sigs)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var c *exec.Cmd
	if len(wrapped) == 1 {
		c = exec.CommandContext(ctx, "/bin/sh", "-c", wrapped[0])
	} else {
		c = exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	}

	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(input), a.out, a.err
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = wrappedWaitDelay

	if err := c.Start(); err != nil {
		// The wrapped command could not be started at all (a program that is not there): say so as a shell does.
		fmt.Fprintf(a.err, "zmina: _limits-tap: %v\n", err)

		return &exitError{code: 127}
	}

	waited := make(chan error, 1)

	go func() { waited <- c.Wait() }()

	var (
		err  error
		told os.Signal
	)

	select {
	case err = <-waited:
	case told = <-sigs:
		cancel() // Cancel kills the group
		err = <-waited
	}

	var exit *exec.ExitError

	switch {
	case told != nil:
		code := 1
		if sig, ok := told.(syscall.Signal); ok {
			code = 128 + int(sig)
		}

		return &exitError{code: code}
	case ctx.Err() != nil, errors.Is(err, exec.ErrWaitDelay):
		return nil
	case err == nil:
		return nil
	case errors.As(err, &exit):
		code := exit.ExitCode()
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}

		return &exitError{code: code}
	}

	return &exitError{code: 1}
}
