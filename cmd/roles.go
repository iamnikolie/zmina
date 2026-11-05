package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
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
	var asJSON, all, wide bool

	c := &cobra.Command{
		Use:   "ls [<node>/<crew>] [role...]",
		Short: "List the roles: up ones a line each, the down ones in one line (--all, --wide, or name roles for more); <node>/<crew>: another crew's",
		RunE: func(cmd *cobra.Command, args []string) error {
			done, args, err := a.remote(cmd, args, false)
			if done || err != nil {
				return err
			}

			return a.readState(func(s *shift.Shift) error {
				views, err := pickViews(s.Views(), args)
				if err != nil {
					return err
				}

				if asJSON {
					a.warnStanding(s)

					return a.printJSON(views)
				}

				if wide || len(args) > 0 {
					return a.lsWide(s, views, len(args) > 0)
				}

				return a.lsCompact(s, views, all)
			})
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVar(&all, "all", false, "a line for every role, the down ones too")
	c.Flags().BoolVar(&wide, "wide", false, "every role with its session, keep and about")

	return c
}

// pickViews keeps the named roles (and their copies: dev names dev#2 too), in the crew's order.
func pickViews(views []shift.View, names []string) ([]shift.View, error) {
	if len(names) == 0 {
		return views, nil
	}

	var out []shift.View

	for _, n := range names {
		found := false

		for _, v := range views {
			if base, _, _ := shift.SplitInstance(v.Role); v.Role == n || base == n {
				out = append(out, v)
				found = true
			}
		}

		if !found {
			return nil, fmt.Errorf("no role %q in the crew", n)
		}
	}

	return out, nil
}

// lsCompact is the default view: a line per role that is not down (state and status, for how long, context,
// tasks), then the down roles in one line, each with what is worth knowing about it in brackets.
func (a *app) lsCompact(s *shift.Shift, views []shift.View, all bool) error {
	var rows, down []shift.View

	for _, v := range views {
		if v.State == shift.PresenceDown && !all {
			down = append(down, v)
		} else {
			rows = append(rows, v)
		}
	}

	head := shiftLine(s) + fmt.Sprintf(" · %d up, %d down", len(views)-countDown(views), countDown(views))
	fmt.Fprintln(a.out, head)

	if n := s.Standing(); n != "" {
		fmt.Fprintln(a.out, n)
	}

	w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)

	for _, v := range rows {
		role := v.Role
		if v.Role == s.Crew.Dispatcher {
			role += " *"
		}

		state := stateCell(v)
		if v.Status != "" {
			state += " " + v.Status
		}

		if v.Prompt {
			state += " (dialog)"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", role, state, shortSince(v), contextCell(v), dash(strings.Join(v.Tasks, ",")))
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if len(down) > 0 {
		names := make([]string, 0, len(down))
		for _, v := range down {
			names = append(names, v.Role+downMarks(v))
		}

		fmt.Fprintln(a.out, "down: "+strings.Join(names, ", "))
	}

	if n := len(laterStore(s.Crew).Parked()); n > 0 {
		fmt.Fprintf(a.out, "later: %d parked (zmina later ls)\n", n)
	}

	return nil
}

func countDown(views []shift.View) int {
	n := 0

	for _, v := range views {
		if v.State == shift.PresenceDown {
			n++
		}
	}

	return n
}

// downMarks is what a down role's name carries in the compact view: its tasks, a broken mark, a bad dir, failed
// lines, its wake-up, its next scheduled run, its expiry. Empty for a plain down role.
func downMarks(v shift.View) string {
	var m []string

	if len(v.Tasks) > 0 {
		m = append(m, strings.Join(v.Tasks, ","))
	}

	if v.Broken {
		m = append(m, "broken")
	}

	if v.Problem != "" {
		m = append(m, "bad dir")
	}

	if v.Failed > 0 {
		m = append(m, fmt.Sprintf("%d failed lines", v.Failed))
	}

	if !v.Asleep.IsZero() {
		m = append(m, "asleep until "+v.Asleep.Local().Format("Mon 15:04"))
	}

	if !v.NextRun.IsZero() {
		m = append(m, "next "+v.NextRun.Format("01-02 15:04"))
	}

	if !v.Expires.IsZero() {
		m = append(m, "expires "+v.Expires.Format("01-02"))
	}

	if len(m) == 0 {
		return ""
	}

	return " (" + strings.Join(m, " · ") + ")"
}

// shortSince is for how long the role has been in its state, in one unit or two: 3m, 4h12m, 2d.
func shortSince(v shift.View) string {
	if v.Since.IsZero() {
		return "-"
	}

	d := time.Since(v.Since)

	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours()/24), int(d.Hours())%24)
	}
}

// lsWide is the whole table: session, keep and about too; with named roles also what each says it does.
func (a *app) lsWide(s *shift.Shift, views []shift.View, now bool) error {
	fmt.Fprintln(a.out, shiftLine(s))

	if n := s.Standing(); n != "" {
		fmt.Fprintln(a.out, n)
	}

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

	if now {
		for _, v := range views {
			if v.Now != "" {
				fmt.Fprintf(a.out, "%s now: %s\n", v.Role, v.Now)
			}
		}

		return nil
	}

	if n := len(laterStore(s.Crew).Parked()); n > 0 {
		fmt.Fprintf(a.out, "later: %d parked (zmina later ls)\n", n)
	}

	return nil
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
		all                 bool
		fresh, continueTask bool
		as                  int
		resume, taskID      string
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

				if (resume != "" || taskID != "" || fresh || continueTask) && len(names) != 1 {
					return errors.New("--resume and --task start one role")
				}

				opts := shift.UpOptions{Resume: resume, Fresh: fresh}
				if continueTask && taskID == "" {
					opts.Resume = "last"
				}

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
					mode := "configured resume policy"
					if resume != "" {
						mode = "explicit resume " + resume
					}
					if t.ID != "" && resume == "" {
						var err error
						opts, mode, err = s.TaskStart(n, t, fresh, continueTask)
						if err != nil {
							errs = append(errs, err)
							continue
						}
					}
					if err := s.UpWith(n, opts); err != nil {
						errs = append(errs, err)

						continue
					}

					r, _ := s.Role(n)

					// A resumed conversation can end at once (claude finds no such conversation): say so, never «up».
					if s.Resumed(n) != "" {
						if why := settledDead(s, n); why != "" {
							errs = append(errs, fmt.Errorf("%s ended right after its start: %s (zmina peek %s; zmina up %s --fresh)", n, why, n, n))

							continue
						}
					}

					fmt.Fprintf(a.out, "%s up as %s · zmina attach %s\n", n, r.Session, n)

					if t.ID != "" {
						fmt.Fprintf(a.out, "%s conversation: %s\n", n, mode)
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

	c.Flags().BoolVar(&fresh, "fresh", false, "start a new conversation; for tasks, bypass saved task conversation")
	c.Flags().BoolVar(&continueTask, "continue", false, "require a compatible task conversation (--task) or the last role conversation")
	c.MarkFlagsMutuallyExclusive("fresh", "continue", "resume")
	return c
}

// handTask gives a task to the role just started for it: the task names the role, and the role's first line is
// the task.
func (a *app) handTask(s *shift.Shift, name string, t task.Task) error {
	if t.Role != name {
		updated, err := a.ledger(s).Set(t.ID, ledger.Change{Role: name})
		if err != nil {
			return err
		}
		t = updated
	}

	if _, err := s.HandTask(name, t); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s handed to %s; it gets the task once booted\n", t.ID, name)

	return nil
}

func (a *app) downCommand() *cobra.Command {
	var all, force, clear bool
	var as int

	c := &cobra.Command{
		Use:   "down [role...]",
		Short: "End roles with /exit into an empty input line (--all: every role up but the dispatcher)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.downRoles(func(s *shift.Shift) ([]string, error) {
				if cmd.Flags().Changed("as") {
					if all || len(args) != 1 || as < 2 || as > 9 || strings.Contains(args[0], "#") {
						return nil, errors.New("--as ends copy N (2-9) of one role: zmina down dev --as 2 (or zmina down 'dev#2')")
					}
					args = []string{fmt.Sprintf("%s#%d", args[0], as)}
				}
				return pick(s, args, all, func(v shift.View) bool {
					return v.State == shift.PresenceUp && v.Role != s.Crew.Dispatcher
				})
			}, shift.DownOptions{Force: force, Clear: clear}, false)
		},
	}

	c.Flags().BoolVar(&all, "all", false, "every role that is up, but the dispatcher")
	c.Flags().IntVar(&as, "as", 0, "end copy N (2-9) of one role, as <role>#N")
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
	var clear, fresh, continueLast bool
	var own shift.Own

	c := &cobra.Command{
		Use:   "restart <role>",
		Short: "End and start a role using its configured resume policy (--fresh or --continue override it); from its own session, once that turn ends, on the same conversation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if own.Pane != 0 {
				return a.ownRestart(name, own, fresh, continueLast, clear)
			}

			var (
				opts   shift.UpOptions
				asked  shift.Own
				inside bool
			)

			err := a.withState(func(s *shift.Shift) error {
				var err error
				if asked, inside = s.Inside(name, os.Getpid(), callerSession()); inside {
					// Checked now, so a restart that cannot go on fails here, to the role that asked.
					if _, err = s.OwnRestartOptions(name, fresh, continueLast); err != nil {
						return err
					}

					s.Log(name, "restart-pending", "", "asked from its own session: once this turn ends")

					return nil
				}

				opts, err = s.RestartOptions(name, fresh, continueLast)
				return err
			})
			if err != nil {
				return err
			}

			if inside {
				return a.handOwnRestart(name, asked, fresh, continueLast, clear)
			}

			return a.restart(name, opts, clear)
		},
	}

	c.Flags().BoolVar(&clear, "clear", false, "drop a draft (C-c) or close a dialog (Escape) first instead of refusing")

	c.Flags().BoolVar(&fresh, "fresh", false, "start a new conversation, ignoring resume policy")
	c.Flags().BoolVar(&continueLast, "continue", false, "resume the last known conversation (fail if unknown)")
	c.MarkFlagsMutuallyExclusive("fresh", "continue")
	// The detached half of a restart a role ran from its own session: the pid in its pane and its start then.
	c.Flags().IntVar(&own.Pane, "after-turn", 0, "")
	c.Flags().Int64Var(&own.Started, "started", 0, "")
	_ = c.Flags().MarkHidden("after-turn")
	_ = c.Flags().MarkHidden("started")

	return c
}

// callerSession is the conversation this command runs in when a runtime's tool runs it: Claude Code and Codex name
// theirs in their tools' environment (a shared Codex daemon's tools carry no ZMINA_ROLE, but their thread).
func callerSession() string {
	if id := os.Getenv("CLAUDE_CODE_SESSION_ID"); id != "" {
		return id
	}

	return os.Getenv("CODEX_THREAD_ID")
}

// restart ends a role and starts it again with opts.
func (a *app) restart(name string, opts shift.UpOptions, clear bool) error {
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
		if err := s.UpWith(name, opts); err != nil {
			return err
		}

		fmt.Fprintln(a.out, name, "restarted")

		return nil
	})
}

// ownRestartWait is the longest the detached half of a restart a role ran from its own session waits for that turn
// to end; ownRestartPoll is how often it looks.
var (
	ownRestartWait = 30 * time.Minute
	ownRestartPoll = time.Second
)

// handOwnRestart hands a restart a role ran from its own session to a detached zmina: run here, putting the role down
// would end this very command (a claude's /exit takes its tools along; a shared Codex bridge interrupts the turn).
func (a *app) handOwnRestart(name string, own shift.Own, fresh, continueLast, clear bool) error {
	args := []string{"--crew", a.crewPath(), "restart", name, "--after-turn", strconv.Itoa(own.Pane),
		"--started", strconv.FormatInt(own.Started, 10)}
	for _, f := range []struct {
		flag string
		on   bool
	}{{"--fresh", fresh}, {"--continue", continueLast}, {"--clear", clear}} {
		if f.on {
			args = append(args, f.flag)
		}
	}

	spawn := a.spawn
	if spawn == nil {
		spawn = spawnDetached
	}

	if err := spawn(args); err != nil {
		return fmt.Errorf("%s: restart from its own session: %w", name, err)
	}

	how := "on this conversation"
	if fresh {
		how = "on a fresh conversation"
	}

	fmt.Fprintf(a.out, "%s restarts once this turn ends, %s (run from its own session: a process of its own does it; zmina log --role %s)\n",
		name, how, name)

	return nil
}

// ownRestart is the detached half of a restart a role ran from its own session: it waits for that turn to end, then
// restarts that very session on its conversation. Whether it may is decided under the lock that begins the down,
// so a session that replaced it, a new turn, or a competing restart is left alone. A failure is told to the
// dispatcher, as no one waits for this process.
func (a *app) ownRestart(name string, own shift.Own, fresh, continueLast, clear bool) error {
	// The role's session going down must not take this one with it.
	signal.Ignore(syscall.SIGHUP)

	err := a.runOwnRestart(name, own, fresh, continueLast, clear)
	if err == nil {
		return nil
	}

	gone := errors.Is(err, shift.ErrOwnGone)
	_ = a.withState(func(s *shift.Shift) error {
		if gone {
			s.Log(name, "restart-skipped", "", err.Error())

			return nil
		}

		s.Log(name, "restart-failed", "", err.Error())
		s.AddNotes([]string{name + ": the restart it asked for from its own session failed: " + oneLine(err.Error()) +
			"; restart it from another session (zmina restart " + name + " --continue)"})

		return nil
	})

	if gone {
		return nil
	}

	return err
}

func (a *app) runOwnRestart(name string, own shift.Own, fresh, continueLast, clear bool) error {
	var (
		opts  shift.UpOptions
		wait  bool
		claim time.Time
	)

	for deadline := time.Now().Add(ownRestartWait); ; time.Sleep(ownRestartPoll) {
		if err := a.awaitOwnTurn(name, own, deadline); err != nil {
			return err
		}

		err := a.withState(func(s *shift.Shift) error {
			var err error
			opts, wait, claim, err = s.ClaimOwnRestart(name, own, fresh, continueLast, clear)
			return err
		})
		if err == nil {
			break
		}

		if !errors.Is(err, shift.ErrOwnTurnRuns) {
			return err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("its turn did not end within %s", ownRestartWait)
		}
	}

	if wait {
		s, err := a.newShift()
		if err != nil {
			return err
		}

		shift.WaitGone(s.Tmux, []string{name}, shift.ExitWait)
	}

	return a.withState(func(s *shift.Shift) error {
		if err := s.FinishOwnRestart(name, claim, opts); err != nil {
			return err
		}

		fmt.Fprintln(a.out, name, "restarted")

		return nil
	})
}

// awaitOwnTurn waits, without the lock, until the turn that asked for the restart looks over, so the lock is taken
// only to check and claim it; the check that counts is ClaimOwnRestart's, under the lock.
func (a *app) awaitOwnTurn(name string, own shift.Own, deadline time.Time) error {
	s, err := a.newShift()
	if err != nil {
		return err
	}

	for ; ; time.Sleep(ownRestartPoll) {
		if st, err := shift.Read(s.Crew.StateDir()); err == nil {
			s.State = st

			over, gone := s.OwnTurnOver(name, own)
			if gone {
				return shift.ErrOwnGone
			}

			if over {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("its turn did not end within %s", ownRestartWait)
		}
	}
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
					id, err := s.QueueTracked(args[0], text)
					if err != nil {
						return err
					}

					if id != "" {
						fmt.Fprintf(a.out, "queued for %s as %s: delivered once it is up and between turns; said again until it acks\n", args[0], id)

						return nil
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
	var (
		lines int
		raw   bool
	)

	c := &cobra.Command{
		Use:   "peek [<node>/<crew>] <role>",
		Short: "A role's state, then the last lines of its screen without blank lines, rules and footer (--raw: as is); <node>/<crew>: another crew's",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			done, args, err := a.remote(cmd, args, false)
			if done || err != nil {
				return err
			}

			switch len(args) {
			case 0:
				return errors.New("peek takes a role after the address: zmina peek <node>/<crew> <role>")
			case 2:
				return fmt.Errorf("%q is no <node>/<crew> address: zmina peek <role>, or zmina peek <node>/<crew> <role>", args[0])
			}

			return a.readState(func(s *shift.Shift) error {
				if raw {
					if lines <= 0 {
						lines = 40
					}

					text, err := s.Peek(args[0], lines)
					if err != nil {
						return err
					}

					fmt.Fprintln(a.out, text)

					return nil
				}

				if lines <= 0 {
					lines = 20
				}

				text, err := s.Peek(args[0], max(3*lines, 60))
				if err != nil {
					return err
				}

				for _, v := range s.Views() {
					if v.Role == args[0] {
						fmt.Fprintln(a.out, peekHead(v))
					}
				}

				fmt.Fprintln(a.out, compactScreen(text, lines))

				return nil
			})
		},
	}

	c.Flags().IntVarP(&lines, "lines", "n", 0, "how many lines (default 20; 40 with --raw)")
	c.Flags().BoolVar(&raw, "raw", false, "the screen as it is: blank lines, rules and the footer too")

	return c
}

// peekHead is peek's first line: the role's state and status, for how long, its context and tasks.
func peekHead(v shift.View) string {
	parts := []string{v.Role, strings.TrimSpace(stateCell(v)+" "+v.Status) + " " + shortSince(v)}
	if v.Prompt {
		parts = append(parts, "dialog on screen")
	}

	if c := contextCell(v); c != "-" {
		parts = append(parts, c)
	}

	if len(v.Tasks) > 0 {
		parts = append(parts, strings.Join(v.Tasks, ","))
	}

	return strings.Join(parts, " · ")
}

// isRule is a line drawn rather than written: mostly box-drawing dashes, perhaps with a label in it (── name ──).
func isRule(l string) bool {
	box, other := 0, 0

	for _, r := range l {
		switch {
		case r == '─' || r == '━' || r == '═' || r == '╌' || r == '┄':
			box++
		case r != ' ':
			other++
		}
	}

	return box >= 3 && box >= 2*other
}

// compactScreen is a captured screen without what carries no words: trailing spaces, blank lines, rules, and the
// footer under the input line (path, model, mode). The input line itself stays, with a draft when there is one.
// The last n lines are kept. The footer is cut only when it is recognised as such (footerAt); anything else under
// the input box — a dialog, boxed or not, an option list, any text — keeps the whole screen: a few lines too many
// cost a few tokens, a lost dialog costs the reader the question it has to answer.
func compactScreen(text string, n int) string {
	lines := strings.Split(text, "\n")
	if end := footerAt(lines); end >= 0 {
		lines = lines[:end]
	}

	var out []string

	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" || isRule(l) {
			continue
		}

		out = append(out, l)
	}

	if len(out) > n {
		out = out[len(out)-n:]
	}

	return strings.Join(out, "\n")
}

// footerAt is the index of the rule under Claude Code's input box when that box is the last block on the screen and
// only its own footer follows; -1 otherwise, and then nothing is cut. Read from the bottom up: blank lines, then
// footer lines (isFooterLine; the first under the rule may be the status line), then the closing rule, then the
// input line with its draft, then the opening rule. Any line that does not fit, or an input line that reads as a
// dialog's option, and it is not the input box.
func footerAt(lines []string) int {
	i := len(lines) - 1
	for i >= 0 && strings.TrimSpace(lines[i]) == "" {
		i--
	}

	footer := i
	for i >= 0 && !isRule(lines[i]) {
		i--
	}

	if i < 0 {
		return -1
	}

	closing := i
	for k := closing + 1; k <= footer; k++ {
		if !isFooterLine(lines[k], k == closing+1) {
			return -1
		}
	}

	// The box: from the opening rule down to the closing one, the ❯ line first, then a draft's further lines.
	i = closing - 1
	for i >= 0 && !isRule(lines[i]) {
		i--
	}

	if i < 0 {
		return -1
	}

	body := lines[i+1 : closing]
	for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}

	if len(body) == 0 || !strings.HasPrefix(body[0], "❯") || optionRe.MatchString(body[0]) {
		return -1
	}

	for _, l := range body[1:] {
		if optionRe.MatchString(l) || strings.ContainsAny(l, "❯│┃") {
			return -1
		}
	}

	return closing
}

// optionRe is a line that reads as a dialog's option: a number and a dot or bracket, maybe behind a cursor or a box.
var optionRe = regexp.MustCompile(`^\s*[│┃]?\s*(❯\s*)?\d+[.)]\s`)

// footerHints are the words of Claude Code's own lines under the input box: the mode line and its hints.
var footerHints = []string{
	"⏵⏵", "⏸", "? for shortcuts", "shift+tab to cycle", "esc to interrupt", "until auto-compact",
	"Auto-update failed", "Press Ctrl-C again", "ctrl+t to", "ctrl+o to",
}

// isFooterLine is a line Claude Code draws under its input box: indented, no box or cursor in it, and either one of
// its hint lines or — right under the rule — a status line (a path or a [model] in it).
func isFooterLine(l string, first bool) bool {
	if strings.TrimSpace(l) == "" {
		return true
	}

	if !strings.HasPrefix(l, "  ") || strings.ContainsAny(l, "❯│┃╭╰") || optionRe.MatchString(l) {
		return false
	}

	for _, h := range footerHints {
		if strings.Contains(l, h) {
			return true
		}
	}

	t := strings.TrimSpace(l)

	return first && (strings.HasPrefix(t, "~/") || strings.HasPrefix(t, "/") || modelRe.MatchString(t))
}

// modelRe is a status line's model in brackets: [Opus 5.5], [Sonnet], [gpt-6].
var modelRe = regexp.MustCompile(`\[[A-Za-z][\w .-]*\]`)

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

func (a *app) tickCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tick",
		Short: "One beat of the ticker (zmina ticker runs it every minute)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := shift.RunTick(a.withState, runWatches, a.rang, a.printTick)
			a.meshTick()
			a.budgetTick(time.Now())

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

	if !v.Asleep.IsZero() {
		out += " (asleep until " + v.Asleep.Local().Format("Mon 15:04") + ")"
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

	if s.Crew.IsRetired() {
		return line + " · RETIRED"
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

// upSettle is how long up waits before it says a resumed role is up: long enough for claude to refuse a conversation.
var upSettle = 1500 * time.Millisecond

// settledDead waits upSettle and reports the last screen line of a role whose process has ended, empty while it runs.
func settledDead(s *shift.Shift, name string) string {
	time.Sleep(upSettle)

	for _, v := range s.Views() {
		if v.Role != name || v.State != shift.PresenceDead {
			continue
		}

		screen, _ := s.Tmux.Capture(name, 20)
		lines := strings.Split(strings.TrimSpace(screen), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "Pane is dead") {
				return l
			}
		}

		return "its process exited"
	}

	return ""
}
