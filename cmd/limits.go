package cmd

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/limits"
	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) limitsCommand() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "limits",
		Short: "How full the plans are: Claude's 5h and 7d, Codex's week, how old each reading is, the crew's policy, and what it does as a window fills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.loadCrew()
			if err != nil {
				return err
			}

			now := time.Now()

			r, err := limits.Collect(c.Layout().Limits(), c.CodexHomes(), c.LimitsPolicy(), now)
			if err != nil {
				fmt.Fprintln(a.err, "zmina: warning:", err)
			}

			// What the crew does as a window fills, and the holds that are on: from the crew file, the holds file and the state's queues.
			marks := c.LimitsThresholds()
			r.Thresholds = &marks

			st, _ := shift.Read(c.StateDir())
			r.Holds = planHolds(c, st, now)

			r = r.In(c.Location())

			if asJSON {
				return a.printJSON(r)
			}

			return writeLimits(a.out, r, now.In(c.Location()))
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable: both plans, each window with its stale flag, the policy, the thresholds and the holds that are on")

	c.AddCommand(a.limitsReleaseCommand())

	return c
}

// limitsReleaseCommand is zmina limits release: the owner's (or the dispatcher's) way to lift a plan hold by hand.
func (a *app) limitsReleaseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "release <plan> [<window>]",
		Short: "Lift a plan's hold by hand (the owner and the dispatcher): its roles are let go in turn; the period is not held again, a later one may be",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, window, err := parseHoldTarget(args)
			if err != nil {
				return err
			}

			return a.withState(func(s *shift.Shift) error {
				if s.By != "hand" && s.By != s.Crew.Dispatcher {
					return errors.New("only the owner or the dispatcher can lift a hold")
				}

				lifted, err := s.LiftHolds(plan, window, s.Now())
				if err != nil {
					return err
				}

				if len(lifted.Holds) == 0 && lifted.Waits == 0 {
					fmt.Fprintf(a.out, "nothing is held for %s\n", plan)

					return nil
				}

				loc := s.Crew.Location()

				for _, h := range lifted.Holds {
					fmt.Fprintf(a.out, "lifted: %s, held at %.0f%% since %s, to end %s\n", h.Label(), h.Used, h.Since.In(loc).Format("15:04"),
						h.Reset.In(loc).Format("Mon 15:04"))
				}

				if lifted.Waits > 0 {
					fmt.Fprintf(a.out, "%d %s waiting their turn after an earlier release let go\n", lifted.Waits, plural(lifted.Waits, "role", "roles"))
				}

				if lifted.Turns > 0 {
					fmt.Fprintf(a.out, "the dispatcher goes now, the other %d %s in turn (%d at a time, a minute apart)\n", lifted.Turns,
						plural(lifted.Turns, "role", "roles"), limits.StaggerGroup)
				}

				return nil
			})
		},
	}
}

// parseHoldTarget reads the plan and the optional window zmina limits release names: claude or codex, and a window of that plan by the names
// the limits data uses (five_hour, seven_day, week), 5h and 7d for the first two.
func parseHoldTarget(args []string) (limits.Plan, string, error) {
	plan := limits.Plan(strings.ToLower(args[0]))
	if plan != limits.PlanClaude && plan != limits.PlanCodex {
		return "", "", fmt.Errorf("a plan is claude or codex, not %q", args[0])
	}

	if len(args) == 1 {
		return plan, "", nil
	}

	window := strings.ToLower(args[1])

	switch window {
	case "5h":
		window = limits.WindowFiveHour
	case "7d":
		window = limits.WindowSevenDay
	}

	if !slices.Contains(limits.PlanWindows(plan), window) {
		return "", "", fmt.Errorf("no window %q for %s: %s", args[1], plan, strings.Join(limits.PlanWindows(plan), ", "))
	}

	return plan, window, nil
}

// writeLimits prints the report: one line a plan, then the policy. A window that is stale is printed as unknown
// with the figure it last had; a plan never seen is unknown too. Nothing is ever filled in.
func writeLimits(out io.Writer, r limits.Report, now time.Time) error {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)

	if c := r.Claude; c == nil {
		fmt.Fprintln(w, "claude\tunknown (no status line has drawn the limits yet)")
	} else {
		var used, resets []string

		for _, win := range []struct {
			label string
			w     *limits.Window
		}{{"5h", c.FiveHour}, {"7d", c.SevenDay}} {
			if win.w == nil {
				continue
			}

			used = append(used, win.label+" "+windowUsed(win.w, c.At, now))

			if !win.w.Stale && win.w.Resets != nil {
				resets = append(resets, win.label+" resets "+clock(*win.w.Resets, now))
			}
		}

		fmt.Fprintf(w, "claude\t%s\t%s\t%s\n", strings.Join(used, "  "), strings.Join(resets, " · "), seen(c.At, now, ""))
	}

	if c := r.Codex; c == nil {
		fmt.Fprintln(w, "codex\tunknown (no Codex rollout with the limits found)")
	} else {
		used, resets := "", ""
		if c.Week != nil {
			used = "week " + windowUsed(c.Week, c.At, now)
			if !c.Week.Stale && c.Week.Resets != nil {
				resets = "resets " + clock(*c.Week.Resets, now)
			}
		}

		fmt.Fprintf(w, "codex\t%s\t%s\t%s\n", used, resets, seen(c.At, now, strings.TrimPrefix(c.Source, "rollout ")))
	}

	fmt.Fprintf(w, "policy\t%s\n", r.Policy)

	// The thresholds say what the crew does as any window of a plan fills, the holds what it is doing now.
	if t := r.Thresholds; t != nil {
		if t.Any() {
			fmt.Fprintf(w, "marks\t%s\n", t)
		} else {
			fmt.Fprintln(w, "marks\toff")
		}
	}

	for _, h := range r.Holds {
		fmt.Fprintf(w, "hold\t%s; the owner's own line passes\n", holdWords(h, now))
	}

	return w.Flush()
}

// windowUsed is a window's share as "41%", or, when it is stale, "unknown (last 41% at 14:02)".
func windowUsed(win *limits.Window, at, now time.Time) string {
	share := fmt.Sprintf("%.0f%%", math.Round(win.Used))
	if !win.Stale {
		return share
	}

	return fmt.Sprintf("unknown (last %s at %s)", share, clock(at, now))
}

// seen is how long ago a reading was taken, with where from when that is worth saying.
func seen(at, now time.Time, from string) string {
	d := now.Sub(at)
	if d < 0 {
		d = 0
	}

	out := "seen just now"
	if d >= time.Minute {
		out = "seen " + age(d) + " ago"
	}

	if from != "" {
		out += " (" + from + ")"
	}

	return out
}

// clock is a moment on the reader's clock: the time of day for today, the weekday with it within the week, else
// the date.
func clock(t, now time.Time) string {
	t = t.In(now.Location())

	switch d := t.Sub(now); {
	case t.Format("2006-01-02") == now.Format("2006-01-02"):
		return t.Format("15:04")
	case d > -6*24*time.Hour && d < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}

	return t.Format("2 Jan 15:04")
}
