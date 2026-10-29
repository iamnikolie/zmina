// Package cmd implements the zmina command line.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// version is stamped at link time (-X github.com/iamnikolie/zmina/cmd.version=…).
var version = "dev"

// app is one run of the command line: where it writes and which crew file it reads. Every command is built on one,
// so tests run commands side by side without shared flags.
type app struct {
	out, err io.Writer
	config   string
	crew     string
}

// NewRoot builds the whole command tree writing to out and errw.
func NewRoot(out, errw io.Writer) *cobra.Command {
	a := &app{out: out, err: errw}

	root := &cobra.Command{
		Use:   "zmina",
		Short: "The shift: long-lived Claude Code role sessions, kept up, with one dispatcher that never sleeps",
		Long: `zmina — the shift.

Each role in the crew file (designer, reviewer, a developer seat, ...) is a long-lived
Claude Code session, started in its own tmux session under a fixed name so other
sessions message it (SendMessage to zm-<role>) and it is open to Remote Control. One
role is the dispatcher: the ticker (every minute) keeps it up, starts the other
keep: always roles again, and types its health notes into the dispatcher.

  zmina init                 write ~/.zmina/crew.yaml
  zmina up designer          start a role        zmina down designer
  zmina ls                   who is up, idle, busy
  zmina attach designer      look at it in this terminal
  zmina ticker install       the ticker (launchd on macOS, systemd on Linux)
  zmina doctor               check the setup

Run 'zmina skill' for the full agent reference.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.SetOut(out)
	root.SetErr(errw)
	root.PersistentFlags().StringVar(&a.config, "config", "", "crew config to use (subdirectory of ~/.zmina/)")
	root.PersistentFlags().StringVar(&a.crew, "crew", "", "crew file (default $ZMINA_CREW or ~/.zmina/crew.yaml)")
	root.MarkFlagsMutuallyExclusive("config", "crew")

	root.AddCommand(a.roleCommands()...)
	root.AddCommand(a.taskCommand(), a.todoCommand(), a.laterCommand(), a.seatCommand(), a.logCommand(), a.noteCommand(), a.watchCommand(),
		a.stopCommand(), a.startCommand(), a.queueCommand(), a.stateCommand(), a.tickerCommand(),
		a.launchdCommand(), a.doctorCommand(), a.usageCommand(), a.skillCommand(), a.versionCommand(), a.uiCommand())

	return root
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the zmina version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(a.out, "zmina version %s\n", version)

			return nil
		},
	}
}

// exitError ends the command with its own exit code (cobra prints the message).
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Execute runs the command line.
func Execute() {
	if err := NewRoot(os.Stdout, os.Stderr).Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}

		os.Exit(1)
	}
}

func (a *app) crewPath() string {
	if a.crew != "" {
		return crew.ExpandHome(a.crew)
	}

	if a.config != "" {
		return crew.ConfigPath(a.config)
	}

	return crew.DefaultPath()
}

// loadCrew reads the crew file and prints its warnings.
func (a *app) loadCrew() (*crew.Crew, error) {
	c, err := crew.Load(a.crewPath())
	if err != nil {
		return nil, err
	}

	for _, w := range c.Warnings {
		fmt.Fprintln(a.err, "zmina: warning:", w)
	}

	return c, nil
}

// newShift builds the shift over the crew file, with a fresh state that is not saved. tmux is found on first use,
// so commands that never touch it (tasks, log, seat) work without it.
func (a *app) newShift() (*shift.Shift, error) {
	c, err := a.loadCrew()
	if err != nil {
		return nil, err
	}

	return &shift.Shift{
		Crew:   c,
		Tmux:   &lazyTmux{socket: c.Socket(), drop: c.DropEnv},
		Reg:    &registry.Registry{Namespace: c.Namespace},
		State:  shift.NewState(),
		Tasks:  taskStore(c),
		Events: eventLog(c),
		Now:    time.Now,
		Path:   os.Getenv("PATH"),
		By:     by(),
		Warn:   func(err error) { fmt.Fprintln(a.err, "zmina: warning:", err) },
	}, nil
}

// readState runs fn on the shift with the saved state, read without the lock (so a look never waits for a down)
// and not written back.
func (a *app) readState(fn func(s *shift.Shift) error) error {
	s, err := a.newShift()
	if err != nil {
		return err
	}

	st, err := shift.Read(s.Crew.StateDir())
	if err != nil {
		// A look still works on an empty state; the writing commands refuse until it is recovered.
		fmt.Fprintln(a.err, "zmina: warning:", err)
	}

	s.State = st

	return fn(s)
}

// withState runs fn on the shift under the state lock and saves the state after. A save that fails fails the
// command, whatever fn returned: what fn did is not recorded.
func (a *app) withState(fn func(s *shift.Shift) error) error {
	s, err := a.newShift()
	if err != nil {
		return err
	}

	l, err := shift.Lock(s.Crew.StateDir())
	if err != nil {
		return err
	}

	s.State = l.State
	s.State.Prune(s.Known)
	s.PruneWatches()

	ferr := fn(s)
	if err := l.Save(); err != nil {
		return errors.Join(ferr, fmt.Errorf("not saved: %w", err))
	}

	return ferr
}

// ledger is the crew's task ledger over git.
func (a *app) ledger(s *shift.Shift) *ledger.Ledger {
	return &ledger.Ledger{
		Store:     taskStore(s.Crew),
		Trees:     ledger.Git{},
		Dir:       s.Crew.Dir,
		Worktrees: s.Crew.Layout().Worktrees(),
		Crew:      s.Crew.Name,
		Now:       s.Now,
		Log:       s.Log,
	}
}

func taskStore(c *crew.Crew) *task.Store {
	return &task.Store{Dir: c.Layout().Tasks()}
}

func eventLog(c *crew.Crew) *events.Log {
	return &events.Log{Path: c.Layout().Events()}
}

// by is who runs this command: the role a session runs as ($ZMINA_ROLE), else the owner's hand.
func by() string {
	if r := os.Getenv("ZMINA_ROLE"); r != "" {
		return r
	}

	return "hand"
}

// lazyTmux finds tmux the first time a command needs it.
type lazyTmux struct {
	socket string
	drop   []string

	once sync.Once
	c    *tmux.Client
	err  error
}

func (l *lazyTmux) client() (*tmux.Client, error) {
	l.once.Do(func() {
		l.c, l.err = tmux.New(l.socket)
		if l.c != nil {
			l.c.Drop = l.drop
		}
	})

	return l.c, l.err
}

func (l *lazyTmux) Probe(name string) (tmux.Pane, error) {
	c, err := l.client()
	if err != nil {
		return tmux.Pane{}, err
	}

	return c.Probe(name)
}

func (l *lazyTmux) Start(name, dir string, env, argv []string) error {
	c, err := l.client()
	if err != nil {
		return err
	}

	return c.Start(name, dir, env, argv)
}

func (l *lazyTmux) Kill(name string) error {
	c, err := l.client()
	if err != nil {
		return err
	}

	return c.Kill(name)
}

func (l *lazyTmux) Type(name, text string) error {
	c, err := l.client()
	if err != nil {
		return err
	}

	return c.Type(name, text)
}

func (l *lazyTmux) Keys(name string, keys ...string) error {
	c, err := l.client()
	if err != nil {
		return err
	}

	return c.Keys(name, keys...)
}

func (l *lazyTmux) Capture(name string, lines int) (string, error) {
	c, err := l.client()
	if err != nil {
		return "", err
	}

	return c.Capture(name, lines)
}

func (l *lazyTmux) Attached(name string) (bool, error) {
	c, err := l.client()
	if err != nil {
		return false, err
	}

	return c.Attached(name)
}

func (l *lazyTmux) Prompt(name string) (string, bool, error) {
	c, err := l.client()
	if err != nil {
		return "", false, err
	}

	return c.Prompt(name)
}

func (l *lazyTmux) KeepScreens(name string) error {
	c, err := l.client()
	if err != nil {
		return err
	}

	return c.KeepScreens(name)
}
