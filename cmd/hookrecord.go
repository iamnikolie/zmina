package cmd

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
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

// ownHook reports whether a hook that names a role runs for it: Claude Code runs a role's hooks in the role's own environment
// (ZMINA_ROLE), so a hook whose --role is another's, or whose environment is another role's, is a command typed by hand or by
// another role and records, and removes, nothing for the role it names. No ZMINA_ROLE is a hand.
func ownHook(role string) bool {
	env := os.Getenv("ZMINA_ROLE")

	return env == "" || env == role
}

// permissionRequestCommand is the PermissionRequest hook of a Claude role (roleSettings). Claude Code runs it when a
// dialog is about to be shown, with the tool and its input on stdin. For AskUserQuestion it writes the questions and
// their options as the role's record (shift.RecordQuestion), which is how zmina knows the question on a screen without
// reading the screen; for any other tool it writes the call, its tool and what it was asked to do, as the role's permission
// record (shift.RecordPermission), which is the only way zmina knows what a permission prompt is about: a plugin is told of one
// only with it. It never decides anything: it prints no allow or deny, nothing at all, and exits 0 whatever
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
				now := time.Now()
				_ = shift.RecordQuestion(l, role, input, now)
				_ = shift.RecordPermission(l, role, input, now)
			}

			return nil
		},
	}

	c.Flags().StringVar(&role, "role", "", "the role the hook runs for")

	return c
}

// stopFailureCommand is the StopFailure hook of a Claude role (roleSettings): Claude Code runs it when a turn ends on an
// API error, which runs no Stop hook. It only records: the error for the role (shift.RecordFailure), so the role's view
// and the dispatcher's note say how the turn ended and delivery holds the role's queue for a while, by the kind of the error
// (shift.FailHold, 5 minutes; shift.AccountHold, 30 minutes; for a limit until it lifts, at least FailHold and at most
// shift.LimitHoldMax), and one event in the log, the hold's start, with its times in the crew's location. It posts nothing,
// retries nothing, prints nothing, and exits 0 whatever goes wrong, a record that cannot be written included.
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
			if !ok || len(input) == 0 {
				return nil
			}

			// A turn took place, so the question an earlier turn ended on is not the role's any more: its record goes.
			if ownHook(role) {
				_ = shift.DropTurn(l, role)
			}

			f, err := shift.RecordFailure(l, role, input, time.Now())
			if err != nil || f == nil {
				return nil
			}

			// The times in the event are in the crew's location, the crew file's timezone else this machine's.
			loc := time.Local
			if c, err := crew.Load(a.crewPath()); err == nil {
				loc = c.Location()
			}

			_ = (&events.Log{Path: l.Events()}).Add(events.Event{Role: role, Event: "failure-hold", By: "hook",
				Detail: "its turn ended on an API error (" + f.Text() + "): " + shift.HoldWords(f, loc)})

			return nil
		},
	}

	c.Flags().StringVar(&role, "role", "", "the role the hook runs for")

	return c
}
