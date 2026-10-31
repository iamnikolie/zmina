package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

func (a *app) roleCommands() []*cobra.Command {
	return []*cobra.Command{a.initCommand(), a.lsCommand(), a.upCommand(), a.downCommand(), a.restartCommand(),
		a.sendCommand(), a.peekCommand(), a.keysCommand(), a.attachCommand(), a.tickCommand()}
}

func (a *app) initCommand() *cobra.Command {
	var (
		force  bool
		agents string
	)

	c := &cobra.Command{
		Use:   "init",
		Short: "Write a starter crew file with the dispatcher and every role that has an AGENT.md",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := a.crewPath()
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s exists; --force overwrites it", path)
			}

			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}

			if agents == "" {
				agents = tildeHome(filepath.Join(filepath.Dir(path), "agents"))
			}

			if err := os.WriteFile(path, []byte(crew.Starter(agents)), 0o600); err != nil {
				return err
			}

			if _, err := crew.Load(path); err != nil {
				return err
			}

			fmt.Fprintln(a.out, "wrote", path)

			if _, err := os.Stat(filepath.Join(crew.ExpandHome(agents), "dispatcher", "AGENT.md")); err != nil {
				fmt.Fprintln(a.out, "no", filepath.Join(agents, "dispatcher", "AGENT.md"), "yet: the dispatcher boots "+
					"without a routing procedure — the repository's examples/ has one to start from")
			}

			return nil
		},
	}

	c.Flags().BoolVar(&force, "force", false, "overwrite an existing crew file")
	c.Flags().StringVar(&agents, "agents", "", "directory of <role>/AGENT.md boot documents (default: agents/ next "+
		"to the crew file)")

	return c
}

func (a *app) lsCommand() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "List the roles: up, down, dead, outside or unknown; idle or busy; the tasks they hold",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				views := s.Views()
				if asJSON {
					return a.printJSON(views)
				}

				fmt.Fprintln(a.out, shiftLine(s))

				w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "ROLE\tSESSION\tSTATE\tSTATUS\tFOR\tCONTEXT\tTASKS\tKEEP\tABOUT")

				for _, v := range views {
					role := v.Role
					if v.Role == s.Crew.Dispatcher {
						role += " *"
					}

					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", role, v.Session, stateCell(v),
						dash(v.Status), since(v), contextCell(v), dash(strings.Join(v.Tasks, ",")), keep(v), v.About)
				}

				if err := w.Flush(); err != nil {
					return err
				}

				if n := len(laterStore(s.Crew).Parked()); n > 0 {
					fmt.Fprintf(a.out, "later: %d parked (zmina later ls)\n", n)
				}

				return nil
			})
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// contextCell is how full the conversation is, with the role's context_max when it has one: 362k/150k.
func contextCell(v shift.View) string {
	cell := "-"
	if v.Context > 0 {
		cell = shift.Tokens(v.Context)
	}

	if v.ContextMax > 0 {
		cell += "/" + shift.Tokens(v.ContextMax)
	}

	return cell
}

func (a *app) upCommand() *cobra.Command {
	var (
		all            bool
		as             int
		resume, taskID string
	)

	c := &cobra.Command{
		Use: "up [role...]",
		Short: "Start roles (--all: every role that is down; --as N: copy N of one role; --resume: its last " +
			"conversation; --task T-n: in the task's worktree, handed the task)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				names, err := pick(s, args, all, func(v shift.View) bool { return v.State == shift.PresenceDown })
				if err != nil {
					return err
				}

				if as != 0 {
					if len(names) != 1 || all {
						return errors.New("--as starts a copy of one role: zmina up dev --as 2")
					}

					names[0] = fmt.Sprintf("%s#%d", names[0], as)
				}

				if (resume != "" || taskID != "") && len(names) != 1 {
					return errors.New("--resume and --task start one role")
				}

				opts := shift.UpOptions{Resume: resume}

				var t task.Task

				if taskID != "" {
					if t, err = taskStore(s.Crew).Get(task.Normalize(taskID)); err != nil {
						return err
					}

					if !t.State.Active() {
						return fmt.Errorf("%s is %s; only an active task gets a seat", t.ID, t.State)
					}

					if t.Worktree != "" {
						if st, err := os.Stat(t.Worktree); err != nil || !st.IsDir() {
							return fmt.Errorf("%s: its worktree %s is gone", t.ID, t.Worktree)
						}

						opts.Dir = t.Worktree
					}
				}

				var errs []error

				for _, n := range names {
					if err := s.UpWith(n, opts); err != nil {
						errs = append(errs, err)

						continue
					}

					r, _ := s.Role(n)
					fmt.Fprintf(a.out, "%s up as %s · zmina attach %s\n", n, r.Session, n)

					if t.ID != "" {
						if err := a.handTask(s, n, t); err != nil {
							errs = append(errs, err)
						}
					}
				}

				return errors.Join(errs...)
			})
		},
	}

	c.Flags().BoolVar(&all, "all", false, "every role that is down")
	c.Flags().IntVar(&as, "as", 0, "start copy N (2-9) of the role, as <role>#N under <session>-N")
	c.Flags().StringVar(&resume, "resume", "", "resume the role's last conversation, or --resume=<session id>")
	c.Flags().Lookup("resume").NoOptDefVal = "last"
	c.Flags().StringVar(&taskID, "task", "", "start in the task's worktree (when it has one) and queue the task for it")

	return c
}

// handTask gives a task to the role just started for it: the task names the role, and the role's first line is
// the task.
func (a *app) handTask(s *shift.Shift, name string, t task.Task) error {
	if t.Role != name {
		if _, err := a.ledger(s).Set(t.ID, ledger.Change{Role: name}); err != nil {
			return err
		}
	}

	line := "[zmina " + t.ID + "] " + t.Asked
	if t.Worktree != "" {
		line += " · you are in its worktree " + t.Worktree + " on branch " + t.Branch
	}

	if err := s.Queue(name, line); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s handed to %s; it gets the task once booted\n", t.ID, name)

	return nil
}

func (a *app) downCommand() *cobra.Command {
	var all, force, clear bool

	c := &cobra.Command{
		Use:   "down [role...]",
		Short: "End roles with /exit into an empty input line (--all: every role up but the dispatcher)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.downRoles(func(s *shift.Shift) ([]string, error) {
				return pick(s, args, all, func(v shift.View) bool {
					return v.State == shift.PresenceUp && v.Role != s.Crew.Dispatcher
				})
			}, shift.DownOptions{Force: force, Clear: clear}, false)
		},
	}

	c.Flags().BoolVar(&all, "all", false, "every role that is up, but the dispatcher")
	c.Flags().BoolVar(&force, "force", false, "also end the dispatcher")
	c.Flags().BoolVar(&clear, "clear", false, "drop a draft (C-c) or close a dialog (Escape) first instead of refusing")

	return c
}

// downRoles ends roles in three steps so the wait for /exit happens outside the lock: begin every down under it,
// wait up to ExitWait without it, then finish under it again. quietNotUp skips roles that were not up.
func (a *app) downRoles(names func(s *shift.Shift) ([]string, error), o shift.DownOptions, quietNotUp bool) error {
	var (
		begun, waitFor []string
		errs           []error
	)

	err := a.withState(func(s *shift.Shift) error {
		list, err := names(s)
		if err != nil {
			return err
		}

		for _, n := range list {
			wait, err := s.BeginDown(n, o)
			if err != nil {
				if !quietNotUp || !errors.Is(err, shift.ErrNotUp) {
					errs = append(errs, err)
				}

				continue
			}

			begun = append(begun, n)
			if wait {
				waitFor = append(waitFor, n)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if len(waitFor) > 0 {
		s, err := a.newShift()
		if err != nil {
			return err
		}

		shift.WaitGone(s.Tmux, waitFor, shift.ExitWait)
	}

	if len(begun) > 0 {
		err = a.withState(func(s *shift.Shift) error {
			for _, n := range begun {
				if err := s.FinishDown(n); err != nil {
					errs = append(errs, err)

					continue
				}

				fmt.Fprintln(a.out, n, "down")
			}

			return nil
		})
	}

	return errors.Join(append(errs, err)...)
}

func (a *app) restartCommand() *cobra.Command {
	var clear bool

	c := &cobra.Command{
		Use:   "restart <role>",
		Short: "End a role and start it fresh (a new session under the same name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			err := a.downRoles(func(s *shift.Shift) ([]string, error) {
				if _, err := s.Role(name); err != nil {
					return nil, err
				}

				return []string{name}, nil
			}, shift.DownOptions{Force: true, Clear: clear}, true)
			if err != nil {
				return err
			}

			return a.withState(func(s *shift.Shift) error {
				if err := s.Up(name); err != nil {
					return err
				}

				fmt.Fprintln(a.out, name, "restarted")

				return nil
			})
		},
	}

	c.Flags().BoolVar(&clear, "clear", false, "drop a draft (C-c) or close a dialog (Escape) first instead of refusing")

	return c
}

func (a *app) sendCommand() *cobra.Command {
	var whenIdle bool

	c := &cobra.Command{
		Use:   "send <role> <text>",
		Short: "Type one line into a role's prompt and press Enter (--when-idle: the ticker types it once the role is idle)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args[1:], " ")
			if strings.ContainsAny(text, "\r\n") {
				return errors.New("send takes one line; for more, use SendMessage from a session")
			}

			return a.withState(func(s *shift.Shift) error {
				if whenIdle {
					if err := s.Queue(args[0], text); err != nil {
						return err
					}

					fmt.Fprintf(a.out, "queued for %s: typed the first time it is up and idle\n", args[0])

					return nil
				}

				return s.Send(args[0], text)
			})
		},
	}

	c.Flags().BoolVar(&whenIdle, "when-idle", false, "queue the line; the ticker types it once the role is up and idle")

	return c
}

func (a *app) peekCommand() *cobra.Command {
	var lines int

	c := &cobra.Command{
		Use:   "peek <role>",
		Short: "Print the last lines of a role's screen (works from an agent, no terminal needed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				text, err := s.Peek(args[0], lines)
				if err != nil {
					return err
				}

				fmt.Fprintln(a.out, text)

				return nil
			})
		},
	}

	c.Flags().IntVarP(&lines, "lines", "n", 40, "how many lines")

	return c
}

func (a *app) keysCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "keys <role> <key...>",
		Short: "Press named keys in a role: Enter, Escape, Up, Down, Tab, C-c, y, n, 1-9 ...",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error { return s.PressKeys(args[0], args[1:]) })
		},
	}
}

func (a *app) attachCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <role>",
		Short: "Attach this terminal to a role (detach with your tmux prefix, then d)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.newShift()
			if err != nil {
				return err
			}

			if _, err := s.Role(args[0]); err != nil {
				return err
			}

			t, err := s.Tmux.(*lazyTmux).client()
			if err != nil {
				return err
			}

			argv := t.AttachArgv(args[0])
			if fi, _ := os.Stdin.Stat(); fi == nil || fi.Mode()&os.ModeCharDevice == 0 {
				return fmt.Errorf("no terminal here; run it in one: %s", strings.Join(argv, " "))
			}

			return syscall.Exec(argv[0], argv, os.Environ())
		},
	}
}

func (a *app) tickCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tick",
		Short: "One beat of the ticker (zmina ticker runs it every minute)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := shift.RunTick(a.withState, runWatches, a.printTick)
			a.meshTick()

			return err
		},
	}
}

func (a *app) printTick(lines []string) {
	for _, line := range lines {
		fmt.Fprintln(a.out, time.Now().Format(time.DateTime), line)
	}
}

// pick resolves the roles a command acts on: the named ones, or with all those matching filter.
func pick(s *shift.Shift, args []string, all bool, filter func(shift.View) bool) ([]string, error) {
	if all {
		var names []string

		for _, v := range s.Views() {
			if filter(v) {
				names = append(names, v.Role)
			}
		}

		return names, nil
	}

	if len(args) == 0 {
		return nil, fmt.Errorf("name roles or pass --all (roles: %s)", strings.Join(s.Crew.Names(), ", "))
	}

	for _, n := range args {
		if _, err := s.Role(n); err != nil {
			return nil, err
		}
	}

	return args, nil
}

func stateCell(v shift.View) string {
	out := string(v.State)

	switch {
	case v.Broken:
		out += " (broken)"
	case v.State != shift.PresenceUnknown && v.Problem != "":
		out += " (bad dir)"
	}

	if v.Failed > 0 {
		out += fmt.Sprintf(" (%d failed lines)", v.Failed)
	}

	return out
}

func since(v shift.View) string {
	if v.Since.IsZero() {
		return "-"
	}

	return time.Since(v.Since).Round(time.Minute).String()
}

// shiftLine is the line above the table: when the ticker last beat, and whether the shift is stopped.
func shiftLine(s *shift.Shift) string {
	line := "ticker: "

	switch last := s.State.LastTick; {
	case last.IsZero():
		line += "no beat recorded yet (zmina ticker status)"
	case time.Since(last) > 3*time.Minute:
		line += "no beat for " + age(time.Since(last)) + " — zmina ticker status, tick.log"
	default:
		line += "beat " + time.Since(last).Round(time.Second).String() + " ago"
	}

	if !s.State.Stopped.IsZero() {
		line += " · shift STOPPED since " + s.State.Stopped.Format("01-02 15:04") + " (zmina start)"
	}

	return line
}

// keep is the KEEP column: keep, and the next scheduled run or the expiry when there is one.
func keep(v shift.View) string {
	out := string(v.Keep)
	if !v.NextRun.IsZero() {
		out += " · next " + v.NextRun.Format("01-02 15:04")
	}

	if !v.Expires.IsZero() {
		out += " · expires " + v.Expires.Format("01-02")
	}

	return out
}

func dash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// tildeHome writes a path under the home directory with ~.
func tildeHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+"/") {
		return "~" + strings.TrimPrefix(p, home)
	}

	return p
}
