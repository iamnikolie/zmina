package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		Use:   "ls [role...]",
		Short: "List the roles: up ones a line each, the down ones in one line (--all, --wide, or name roles for more)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				views, err := pickViews(s.Views(), args)
				if err != nil {
					return err
				}

				if asJSON {
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

	c := &cobra.Command{
		Use:   "restart <role>",
		Short: "End and start a role using its configured resume policy (--fresh or --continue override it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var opts shift.UpOptions
			err := a.withState(func(s *shift.Shift) error {
				var err error
				opts, err = s.RestartOptions(name, fresh, continueLast)
				return err
			})
			if err != nil {
				return err
			}

			err = a.downRoles(func(s *shift.Shift) ([]string, error) {
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
		},
	}

	c.Flags().BoolVar(&clear, "clear", false, "drop a draft (C-c) or close a dialog (Escape) first instead of refusing")

	c.Flags().BoolVar(&fresh, "fresh", false, "start a new conversation, ignoring resume policy")
	c.Flags().BoolVar(&continueLast, "continue", false, "resume the last known conversation (fail if unknown)")
	c.MarkFlagsMutuallyExclusive("fresh", "continue")

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
	var (
		lines int
		raw   bool
	)

	c := &cobra.Command{
		Use:   "peek <role>",
		Short: "A role's state, then the last lines of its screen without blank lines, rules and footer (--raw: as is)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
// The last n lines are kept. A screen with no input box (a dialog, whose ❯ is the selection cursor) keeps every line
// that has words: its options and its confirm hint are what the reader has to act on.
func compactScreen(text string, n int) string {
	lines := strings.Split(text, "\n")
	if end := inputBoxEnd(lines); end >= 0 {
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

// inputBoxEnd is the index of the rule that closes Claude Code's input box — a rule, the ❯ line (a draft may run on
// to more lines), a rule — when the screen's last ❯ is that box's; -1 otherwise. What lies under it is the footer.
// A last ❯ outside such a box is a dialog's selected option: the dialog is the screen's present, and an input box
// above it is history, so nothing is cut.
func inputBoxEnd(lines []string) int {
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "❯") {
			last = i
			break
		}
	}

	if last < 0 {
		return -1
	}

	above := last - 1
	for above >= 0 && strings.TrimSpace(lines[above]) == "" {
		above--
	}

	if above < 0 || !isRule(lines[above]) {
		return -1
	}

	for j := last + 1; j < len(lines); j++ {
		if isRule(lines[j]) {
			return j
		}
	}

	return -1
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
