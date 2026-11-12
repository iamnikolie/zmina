package cmd

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// hookInputMax is how much of a hook's input these hooks read: the question of an AskUserQuestion is a few kilobytes,
// and a longer input (a Write's whole file in a PermissionRequest) is not one.
const hookInputMax = 1 << 20

// readHookInput reads a hook's whole input, up to hookInputMax, and drains what is left so Claude Code never writes
// into a closed pipe. Nothing when run by hand (a terminal on stdin).
func readHookInput(in io.Reader) []byte {
	if !piped(in) {
		return nil
	}

	raw, _ := io.ReadAll(io.LimitReader(in, hookInputMax))
	_, _ = io.Copy(io.Discard, in)

	return raw
}

// hookLayout is where the crew's files are, nothing made: false when the crew file's directory is not there.
func (a *app) hookLayout() (crew.Layout, bool) {
	path, err := filepath.Abs(a.crewPath())
	if err != nil {
		return crew.Layout{}, false
	}

	if st, err := os.Stat(filepath.Dir(path)); err != nil || !st.IsDir() {
		return crew.Layout{}, false
	}

	return crew.Layout{Dir: filepath.Dir(path)}, true
}

// permissionRequestCommand is the PermissionRequest hook of a Claude role (roleSettings). Claude Code runs it when a
// dialog is about to be shown, with the tool and its input on stdin. For AskUserQuestion it writes the questions and
// their options as the role's record (shift.RecordQuestion), which is how zmina knows the question on a screen without
// reading the screen. It never decides anything: it prints no allow or deny, nothing at all, and exits 0 whatever
// goes wrong — a hook that failed or answered would break or settle the dialog it is run for. A Go panic exits 2, which
// a hook reads as a refusal, so it is recovered.
func (a *app) permissionRequestCommand() *cobra.Command {
	var role string

	c := &cobra.Command{
		Use:    "_permission-request",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		// A line written by another zmina may carry a flag this one does not know: it still ends quietly.
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		SilenceErrors:      true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer func() { _ = recover() }()

			input := readHookInput(cmd.InOrStdin())

			if l, ok := a.hookLayout(); ok && len(input) > 0 {
				_ = shift.RecordQuestion(l, role, input, time.Now())
			}

			return nil
		},
	}

	c.Flags().StringVar(&role, "role", "", "the role the hook runs for")

	return c
}

// stopFailureCommand is the StopFailure hook of a Claude role (roleSettings): Claude Code runs it when a turn ends on an
// API error, which runs no Stop hook. It records the error for the role (shift.RecordFailure), so the role's view and
// the dispatcher's note say how the turn ended, and starts the detached copy of _turn-end that posts what waits once
// the session is idle, as the Stop hook does for any other turn's end. It retries nothing and prints nothing, and
// exits 0 whatever goes wrong, a record that cannot be written included.
func (a *app) stopFailureCommand() *cobra.Command {
	var role string

	c := &cobra.Command{
		Use:                "_stop-failure",
		Hidden:             true,
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		SilenceErrors:      true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer func() { _ = recover() }()

			input := readHookInput(cmd.InOrStdin())

			l, ok := a.hookLayout()
			if !ok || len(input) == 0 || role == "" {
				return nil
			}

			sid, _ := shift.RecordFailure(l, role, input, time.Now())
			if sid == "" {
				return nil
			}

			spawn := a.spawn
			if spawn == nil {
				spawn = spawnDetached
			}

			_ = spawn([]string{"--crew", a.crewPath(), "_turn-end", "--role", role, "--session", sid, "--wait", "--failed"})

			return nil
		},
	}

	c.Flags().StringVar(&role, "role", "", "the role the hook runs for")

	return c
}
