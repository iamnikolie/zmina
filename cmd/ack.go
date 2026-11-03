package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) ackCommand() *cobra.Command {
	var (
		all  bool
		role string
	)

	c := &cobra.Command{
		Use:   "ack [S-n...]",
		Short: "Lines handled: stop saying them again (ids from the lines; --all: every line of the calling role)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("name the ids to ack (S-12 S-13), or --all")
			}

			caller := by()
			if role == "" {
				role = caller
			}

			return a.withState(func(s *shift.Shift) error {
				acked, warn := s.AckLines(args, all, role, caller)
				for _, w := range warn {
					fmt.Fprintln(a.err, "zmina ack: "+w)
				}

				if len(acked) == 0 {
					fmt.Fprintln(a.out, "nothing acked")

					return nil
				}

				fmt.Fprintln(a.out, "acked "+strings.Join(acked, " "))

				return nil
			})
		},
	}

	c.Flags().BoolVar(&all, "all", false, "ack every line of the role")
	c.Flags().StringVar(&role, "role", "", "the role whose lines --all acks (default $ZMINA_ROLE)")

	return c
}

func (a *app) pendingCommand() *cobra.Command {
	var (
		role         string
		brief, every bool
		asJSON       bool
	)

	c := &cobra.Command{
		Use:   "pending",
		Short: "Lines delivered and not acked yet: the calling role's (--role, --all for every role)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" && !every {
				role = by()
				if role == "hand" {
					role = ""
				}
			}

			return a.readState(func(s *shift.Shift) error {
				if brief {
					// The hook a socket role runs at a session's start and after a trim: what it prints goes into the
					// context. The tag rule only after a trim (a start's boot line names its own tag), and only to
					// the role itself — another seat could be asked to run this and pass the tag on.
					rule := trimmed(hookSource(cmd.InOrStdin())) && role != "" && role == os.Getenv("ZMINA_ROLE")
					if text := s.PendingBrief(role, rule); text != "" {
						fmt.Fprintln(a.out, text)
					}

					return nil
				}

				list := s.Pending(role)
				if asJSON {
					return a.printJSON(list)
				}

				if len(list) == 0 {
					fmt.Fprintln(a.out, "nothing waits for an ack")

					return nil
				}

				for _, p := range list {
					when := "queued"
					if !p.First.IsZero() {
						when = "since " + p.First.Local().Format("Mon 15:04")
						if p.Renotes > 0 {
							when += fmt.Sprintf(", said %d more times", p.Renotes)
						}
					}

					fmt.Fprintf(a.out, "%s  %s  %s  %s\n", p.ID, p.Role, when, clip(oneLine(p.Text), 120))
				}

				return nil
			})
		},
	}

	c.Flags().StringVar(&role, "role", "", "this role's lines (default $ZMINA_ROLE)")
	c.Flags().BoolVar(&every, "all", false, "every role's lines")
	c.Flags().BoolVar(&brief, "brief", false, "one block for the role's context (the hook after a compaction)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// hookSource is the source a SessionStart hook is run for (startup, resume, clear, compact), from the JSON Claude
// Code gives it on stdin; "" when run by hand.
func hookSource(in io.Reader) string {
	return readHook(in).Source
}

// hookInput is the part of the JSON Claude Code gives a hook on stdin that zmina reads.
type hookInput struct {
	Source    string `json:"source"`
	SessionID string `json:"session_id"`
}

// readHook reads a hook's input: empty when run by hand (a terminal on stdin) or when it is not JSON. The whole
// object is read, however long: a Stop hook's carries the turn's last reply (last_assistant_message).
func readHook(in io.Reader) hookInput {
	if !piped(in) {
		return hookInput{}
	}

	var hook hookInput

	_ = json.NewDecoder(in).Decode(&hook)

	return hook
}

// piped reports whether a reader is not a terminal: a hook's stdin, a pipe or a file.
func piped(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return true
	}

	st, err := f.Stat()

	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

// trimmed reports a session-start source after which the boot line may be gone.
func trimmed(source string) bool { return source == "compact" || source == "clear" }
