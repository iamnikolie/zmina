package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

// retireCommand marks this crew as no longer the live one. The mark is the owner's word and sits in the crew file, so
// a state file recovered or a stop lifted does not lose it. It is the only thing that says two crews are one: their
// names never do.
func (a *app) retireCommand() *cobra.Command {
	var (
		movedTo    string
		off, clear bool
	)

	c := &cobra.Command{
		Use:   "retire [--moved-to <node>/<crew>] | --off",
		Short: "Mark this crew as no longer the live one and stop it: boot, ls and tell say so and name where it went on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if off {
				if movedTo != "" {
					return errors.New("--off or --moved-to, not both")
				}

				if err := a.setRetired("", ""); err != nil {
					return err
				}

				fmt.Fprintln(a.out, "retired mark off; the shift stays stopped until zmina start")

				return nil
			}

			if err := a.checkMovedTo(movedTo); err != nil {
				return err
			}

			if err := a.setRetired(time.Now().Format(time.DateOnly), movedTo); err != nil {
				return err
			}

			if movedTo != "" {
				fmt.Fprintln(a.out, "retired: its live crew is", movedTo)
			} else {
				fmt.Fprintln(a.out, "retired, no live crew named")
			}

			return a.downEveryRole(clear)
		},
	}

	c.Flags().StringVar(&movedTo, "moved-to", "", "the crew it went on as, <node>/<crew>")
	c.Flags().BoolVar(&off, "off", false, "take the mark off; the shift stays stopped until zmina start")
	c.Flags().BoolVar(&clear, "clear", false, "drop drafts (C-c) and close dialogs (Escape) first instead of refusing")

	return c
}

// setRetired writes the mark, or takes it off with both empty, under the state lock; marking stops the shift too.
func (a *app) setRetired(date, movedTo string) error {
	return a.withState(func(s *shift.Shift) error {
		e, err := crew.Open(s.Crew.Path)
		if err != nil {
			return err
		}

		e.SetCrewKey("retired", date)
		e.SetCrewKey("moved_to", movedTo)

		if err := e.SaveCrewKeys(); err != nil {
			return err
		}

		if date != "" {
			s.Stop()
			s.Log("", "retire", "", strings.TrimSpace("moved to "+movedTo))
		} else {
			s.Log("", "retire", "", "off")
		}

		return nil
	})
}

// checkMovedTo refuses what does not read as another crew; a node this one has not heard of is a warning, since a
// crew may be marked before its new node is met.
func (a *app) checkMovedTo(movedTo string) error {
	if movedTo == "" {
		return nil
	}

	ad, err := mesh.ParseAddress(movedTo)
	if err != nil {
		return fmt.Errorf("--moved-to: %w", err)
	}

	if ad.Crew == "" || ad.String() != movedTo {
		return fmt.Errorf("--moved-to %q: want <node>/<crew>", movedTo)
	}

	if _, err := crew.NewMovedTo(movedTo); err != nil {
		return err
	}

	me, err := openNode()
	if err != nil {
		return nil //nolint:nilerr // a crew may be retired on a machine that is no node yet
	}

	if ad.Node == me.Name {
		if ad.Crew == a.crewKey() {
			return errors.New("--moved-to names this very crew")
		}

		return nil
	}

	book, _ := me.Peers()
	if !slices.ContainsFunc(book, func(p mesh.Peer) bool { return p.Name == ad.Node }) {
		fmt.Fprintf(a.err, "zmina: warning: no node %q known here (zmina node ls)\n", ad.Node)
	}

	return nil
}
