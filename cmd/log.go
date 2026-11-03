package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

func (a *app) logCommand() *cobra.Command {
	var (
		since, role, taskID string
		last                int
		asJSON, full        bool
	)

	c := &cobra.Command{
		Use:   "log",
		Short: "What happened on the shift, an event a line (--since 12h, --role, --task, --full)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				f := events.Filter{Role: role, Last: last}
				if taskID != "" {
					f.Task = task.Normalize(taskID)
				}

				if since != "" {
					d, err := parseAge(since)
					if err != nil {
						return err
					}

					f.Since = s.Now().Add(-d)
				}

				evs, err := eventLog(s.Crew).Read(f)
				if err != nil {
					return err
				}

				if asJSON {
					if evs == nil {
						evs = []events.Event{}
					}

					return a.printJSON(evs)
				}

				for _, e := range evs {
					line := e.TS.Local().Format(time.DateTime) + "  " + dash(e.Role) + "  " + e.Event
					if e.Task != "" {
						line += " " + e.Task
					}

					if e.By != "" {
						line += "  by " + e.By
					}

					if e.Detail != "" && full {
						line += "  " + e.Detail
					} else if e.Detail != "" {
						line = clip(line+"  "+oneLine(e.Detail), 160)
					}

					fmt.Fprintln(a.out, line)
				}

				return nil
			})
		},
	}

	c.Flags().StringVar(&since, "since", "", "only events this recent: 90m, 12h, 3d")
	c.Flags().StringVar(&role, "role", "", "only this role")
	c.Flags().StringVar(&taskID, "task", "", "only this task")
	c.Flags().IntVarP(&last, "last", "n", 50, "the newest n events (0: all)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVar(&full, "full", false, "each event whole (default: one line of at most 160 characters)")

	return c
}

// parseAge reads a Go duration (90m, 12h) or a number of days (3d).
func parseAge(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q: 90m, 12h or 3d", s)
		}

		return time.Duration(n) * 24 * time.Hour, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q: 90m, 12h or 3d", s)
	}

	return d, nil
}
