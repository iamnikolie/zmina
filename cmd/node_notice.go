package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/mesh"
)

// nodeEvent records something the node itself should say in the event log of every crew on this machine: a node has
// no log of its own, and each crew's dispatcher reads its own (zmina log). A crew whose file does not load is left
// out.
func nodeEvent(kind, detail string) {
	for _, c := range uiCrews("") {
		cr, err := crew.Load(c.Path)
		if err != nil {
			continue
		}

		_ = eventLog(cr).Add(events.Event{Event: kind, By: "mesh", Detail: detail})
	}
}

func (a *app) nodeStrict() *cobra.Command {
	return &cobra.Command{
		Use:       "strict [on|off]",
		Short:     "Whether this node refuses owner requests that name no node (off by default; on once every node binds its requests)",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			me, err := openNode()
			if err != nil {
				return err
			}

			if len(args) == 0 {
				fmt.Fprintf(a.out, "strict requests: %s\n", map[bool]string{true: "on", false: "off"}[me.StrictRequests])

				return nil
			}

			switch args[0] {
			case "on", "off":
			default:
				return fmt.Errorf("strict is on or off, not %q", args[0])
			}

			on := args[0] == "on"
			if err := mesh.SetStrictRequests(meshDir(), on); err != nil {
				return err
			}

			if !on {
				fmt.Fprintln(a.out, "strict requests: off · this node takes owner requests in the old form again")

				return nil
			}

			fmt.Fprintln(a.out, "strict requests: on · this node refuses owner requests that name no node (the next session sees it)")

			if peers, err := me.Peers(); err == nil {
				var behind []string

				for _, p := range peers {
					if p.Cert != nil && !p.OK.IsZero() && !p.TerminalV2 {
						behind = append(behind, p.Name)
					}
				}

				if len(behind) > 0 {
					fmt.Fprintf(a.out, "warning: no terminal_v2 from %s in its last session: if one is the owner's node, its "+
						"requests are refused here until it runs a zmina that binds them\n", strings.Join(behind, ", "))
				}
			}

			return nil
		},
	}
}
