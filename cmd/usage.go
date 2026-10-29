package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
)

// roleUsage is what one role spent over all its sessions and copies.
type roleUsage struct {
	Role       string    `json:"role"`
	Sessions   int       `json:"sessions"`
	Turns      int       `json:"turns"`
	Input      int       `json:"input"`
	CacheWrite int       `json:"cache_write"`
	CacheRead  int       `json:"cache_read"`
	Output     int       `json:"output"`
	Last       time.Time `json:"last"`
}

func (u roleUsage) total() int { return u.Input + u.CacheWrite + u.CacheRead + u.Output }

func (a *app) usageCommand() *cobra.Command {
	var (
		since  string
		asJSON bool
	)

	c := &cobra.Command{
		Use:   "usage",
		Short: "Tokens each role spent (--since 24h): turns, input, cache writes and reads, output, from the transcripts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := parseAge(since)
			if err != nil {
				return err
			}

			return a.readState(func(s *shift.Shift) error {
				out, err := usageByRole(s.Crew, time.Now().Add(-d))
				if err != nil {
					return err
				}

				if asJSON {
					if out == nil {
						out = []roleUsage{}
					}

					return a.printJSON(out)
				}

				var sum roleUsage

				w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', tabwriter.AlignRight)
				fmt.Fprintln(w, "ROLE\tSESSIONS\tTURNS\tINPUT\tCACHE WRITE\tCACHE READ\tOUTPUT\tTOTAL\tPER TURN\t")

				for _, u := range out {
					fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t\n", u.Role, u.Sessions, u.Turns,
						shift.Tokens(u.Input), shift.Tokens(u.CacheWrite), shift.Tokens(u.CacheRead),
						shift.Tokens(u.Output), shift.Tokens(u.total()), shift.Tokens(u.total()/max(u.Turns, 1)))

					sum.Sessions += u.Sessions
					sum.Turns += u.Turns
					sum.Input += u.Input
					sum.CacheWrite += u.CacheWrite
					sum.CacheRead += u.CacheRead
					sum.Output += u.Output
				}

				fmt.Fprintf(w, "all\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t\n", sum.Sessions, sum.Turns,
					shift.Tokens(sum.Input), shift.Tokens(sum.CacheWrite), shift.Tokens(sum.CacheRead),
					shift.Tokens(sum.Output), shift.Tokens(sum.total()), shift.Tokens(sum.total()/max(sum.Turns, 1)))

				return w.Flush()
			})
		},
	}

	c.Flags().StringVar(&since, "since", "24h", "how far back: 90m, 12h, 7d")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// usageByRole sums the crew's sessions per role, a role's copies (dev#2 under zm-dev-2) with it, biggest first.
func usageByRole(c *crew.Crew, since time.Time) ([]roleUsage, error) {
	role := map[string]string{}

	for _, name := range c.Names() {
		r := c.Roles[name]
		role[r.Session] = name

		for n := 2; n <= crew.MaxInstance; n++ {
			role[r.Session+"-"+strconv.Itoa(n)] = name
		}
	}

	names := make([]string, 0, len(role))
	for s := range role {
		names = append(names, s)
	}

	sessions, err := (&registry.Registry{Namespace: c.Namespace}).UsageSince(names, since)
	if err != nil {
		return nil, err
	}

	by := map[string]*roleUsage{}

	for _, u := range sessions {
		r := role[u.Session]
		if by[r] == nil {
			by[r] = &roleUsage{Role: r}
		}

		x := by[r]
		x.Sessions++
		x.Turns += u.Turns
		x.Input += u.Input
		x.CacheWrite += u.CacheWrite
		x.CacheRead += u.CacheRead
		x.Output += u.Output

		if u.Last.After(x.Last) {
			x.Last = u.Last
		}
	}

	out := make([]roleUsage, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].total() > out[j].total() })

	return out, nil
}
