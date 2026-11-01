package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) inboxCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "inbox",
		Short: "The whole text of lines too long to type into the dispatcher: ls, show <id>",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return a.inboxLs() },
	}

	c.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "Every stored text, the newest first",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return a.inboxLs() },
	}, &cobra.Command{
		Use:   "show <id>",
		Short: "One stored text, whole; a line from another crew is shown as data, each line marked",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				e, err := s.Inbox.Get(args[0])
				if err != nil {
					return err
				}

				fmt.Fprint(a.out, showEntry(e))

				return nil
			})
		},
	})

	return c
}

func (a *app) inboxLs() error {
	return a.readState(func(s *shift.Shift) error {
		es, err := s.Inbox.List()
		if err != nil {
			return err
		}

		if len(es) == 0 {
			fmt.Fprintln(a.out, "nothing stored")

			return nil
		}

		for _, e := range es {
			fmt.Fprintf(a.out, "%s  %s  %s  %d chars  %s\n", e.ID, e.At.Local().Format(time.DateTime), e.From,
				len([]rune(e.Body)), clip(shown(e, strings.Join(strings.Fields(e.Body), " ")), 60))
		}

		return nil
	})
}

// showEntry prints an entry whole: a header line, then every line of the text after "| ", so nothing in it can pass
// for a line of zmina's own. A line from another crew is said to be data first, and its quote marks are taken out
// as in the typed note (quoteSafe).
func showEntry(e inbox.Entry) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s · %s · %s · %d chars\n", e.ID, e.From, e.At.Local().Format(time.DateTime), len([]rune(e.Body)))

	if e.Remote {
		b.WriteString(strings.TrimSpace(remoteWrap) + ":\n")
	}

	for _, line := range strings.Split(strings.TrimRight(shown(e, e.Body), "\n"), "\n") {
		b.WriteString("| " + line + "\n")
	}

	return b.String()
}

// shown is an entry's text as the page or the terminal shows it: a remote one through quoteSafe, control
// characters out of every one.
func shown(e inbox.Entry, text string) string {
	text = strings.Map(func(r rune) rune {
		if r != '\n' && (r < ' ' || r == 0x7f) {
			return -1
		}

		return r
	}, text)

	if e.Remote {
		return quoteSafe(text)
	}

	return text
}
