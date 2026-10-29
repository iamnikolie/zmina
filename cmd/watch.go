package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/watch"
)

func (a *app) watchCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "watch",
		Short: "Watches: periodic detectors the ticker runs; each queues notes for the dispatcher (ls, run, baseline, forget)",
	}

	c.AddCommand(a.watchLsCommand(), a.watchRunCommand(), a.watchBaselineCommand(), a.watchForgetCommand())

	return c
}

func (a *app) watchLsCommand() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "Each watch: kind, every, last run, next run, keys, the last notes and drops",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				views := s.WatchViews()
				if asJSON {
					return a.printJSON(views)
				}

				if len(views) == 0 {
					fmt.Fprintln(a.out, "no watches; add them under watch: in", s.Crew.Path)

					return nil
				}

				w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "WATCH\tKIND\tEVERY\tLAST RUN\tNEXT\tKEYS\tHELD")

				for _, v := range views {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", v.Name, v.Kind, v.Every, lastRun(v), nextRun(v),
						v.Keys, v.Pending)
				}

				if err := w.Flush(); err != nil {
					return err
				}

				for _, v := range views {
					if v.LastError != "" {
						fmt.Fprintf(a.out, "\n%s failed (%d in a row): %s\n", v.Name, v.Failures, v.LastError)
					}

					if len(v.Recent) > 0 {
						fmt.Fprintf(a.out, "\n%s, last notes and drops:\n  %s\n", v.Name, strings.Join(v.Recent, "\n  "))
					}
				}

				return nil
			})
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

func (a *app) watchRunCommand() *cobra.Command {
	var dryRun bool

	c := &cobra.Command{
		Use:   "run <watch>",
		Short: "One run now; --dry-run prints every event with its fate and writes nothing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRun {
				return a.dryRun(args[0])
			}

			return a.runOne(args[0], false)
		},
	}

	c.Flags().BoolVar(&dryRun, "dry-run", false, "print every event with its fate; write nothing")

	return c
}

func (a *app) watchBaselineCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "baseline <watch>",
		Short: "Record what the source reports now without noting it",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runOne(args[0], true) },
	}
}

func (a *app) watchForgetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <watch> [key]",
		Short: "Drop stored fingerprints (one key or all), so the next run notes them again",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := ""
			if len(args) == 2 {
				key = args[1]
			}

			return a.withState(func(s *shift.Shift) error {
				n, err := s.ForgetWatch(args[0], key)
				if err != nil {
					return err
				}

				fmt.Fprintf(a.out, "%s: forgot %d keys\n", args[0], n)

				return nil
			})
		},
	}
}

// runOne runs one watch by hand: begun and finished under the lock, its source outside it.
func (a *app) runOne(name string, baseline bool) error {
	var run shift.WatchRun

	err := a.withState(func(s *shift.Shift) error {
		var err error
		run, err = s.BeginWatch(name)

		return err
	})
	if err != nil {
		return err
	}

	res := runWatches([]shift.WatchRun{run})[0]

	return a.withState(func(s *shift.Shift) error {
		for _, line := range s.FinishWatch(run, res, baseline) {
			fmt.Fprintln(a.out, line)
		}

		return res.Err
	})
}

// dryRun runs the source and diffs against a copy of the state: every event with its fate, nothing written.
func (a *app) dryRun(name string) error {
	return a.readState(func(s *shift.Shift) error {
		run, err := s.PeekWatch(name)
		if err != nil {
			return err
		}

		res := runWatches([]shift.WatchRun{run})[0]
		for _, l := range res.Stderr {
			fmt.Fprintln(a.err, name+":", l)
		}

		if res.Err != nil {
			return res.Err
		}

		w := run.Watch
		st := s.State.Watch[name].Clone()
		cfg := watch.Config{Quiet: w.QuietD(), ForgetAfter: w.ForgetAfterD(), Baseline: w.Baselines(), Info: w.Info}
		outs := watch.Apply(st, res.Events, cfg, s.Now(), func(watch.Event) error { return nil })

		if len(outs) == 0 {
			fmt.Fprintln(a.out, "no events")
		}

		for _, o := range outs {
			fmt.Fprintf(a.out, "%-12s %s  %s: %s\n", o.String(), o.Event.Key, name, o.Event.Line())
		}

		return nil
	})
}

// runWatches runs the sources side by side, each bounded by its own timeout.
func runWatches(runs []shift.WatchRun) []watch.Result {
	results := make([]watch.Result, len(runs))

	var wg sync.WaitGroup

	for i, run := range runs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			results[i] = watch.Run(context.Background(), run.Watch, run.Env)
		}()
	}

	wg.Wait()

	return results
}

func lastRun(v shift.WatchView) string {
	switch {
	case v.Running:
		return "running"
	case v.LastRun.IsZero():
		return "never"
	case v.LastOK:
		return "ok " + time.Since(v.LastRun).Round(time.Minute).String() + " ago"
	default:
		return "failed " + time.Since(v.LastRun).Round(time.Minute).String() + " ago"
	}
}

func nextRun(v shift.WatchView) string {
	if !v.Enabled {
		return "off"
	}

	if !v.NextRun.After(time.Now()) {
		return "next beat"
	}

	return v.NextRun.Format("15:04")
}
