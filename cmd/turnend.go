package cmd

import (
	"os/exec"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/spf13/cobra"
)

const (
	// turnEndWait is how long the detached half of a Stop hook waits for the session to go idle; past it the beat
	// delivers as before.
	turnEndWait = 10 * time.Second
	// turnEndPoll is how often it looks.
	turnEndPoll = 250 * time.Millisecond
)

// turnEndCommand is the Stop hook of a role on its own channel (roleSettings). Run by Claude Code it reads the
// session id, starts its own detached copy and returns at once: no output, exit 0 whatever happens, so the end of
// a turn costs the role no tokens and no wait. The copy (--wait) waits for the session to go idle and posts what
// waits for the role the way the beat would (Shift.TurnEnd), so a line goes when the turn is over, not up to a
// minute later.
func (a *app) turnEndCommand() *cobra.Command {
	var (
		role, session string
		wait          bool
	)

	c := &cobra.Command{
		Use:    "_turn-end",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		// Nothing is ever printed: a hook's words would reach the user's screen.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wait {
				a.turnEnd(role, session)

				return nil
			}

			sid := readHook(cmd.InOrStdin()).SessionID
			if role == "" || sid == "" {
				return nil
			}

			spawn := a.spawn
			if spawn == nil {
				spawn = spawnDetached
			}

			_ = spawn([]string{"--crew", a.crewPath(), "_turn-end", "--role", role, "--session", sid, "--wait"})

			return nil
		},
	}

	c.Flags().StringVar(&role, "role", "", "the role whose turn ended")
	c.Flags().StringVar(&session, "session", "", "its session id (the detached copy)")
	c.Flags().BoolVar(&wait, "wait", false, "wait for the session to go idle, then post (the detached copy)")

	return c
}

// spawnDetached starts this zmina in a session of its own with nothing on stdin, stdout or stderr, so the hook that
// started it returns at once and Claude Code waits for no pipe it holds.
func spawnDetached(args []string) error {
	bin := self()
	if bin == "" {
		bin = "zmina"
	}

	c := exec.Command(bin, args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := c.Start(); err != nil {
		return err
	}

	return c.Process.Release()
}

// turnEnd waits up to turnEndWait for the role's session to be idle, then posts under the lock what waits for it.
// A session that stays busy (a background shell) or another session is left to the beat.
func (a *app) turnEnd(role, session string) {
	s, err := a.newShift()
	if err != nil {
		return
	}

	for deadline := time.Now().Add(turnEndWait); ; time.Sleep(turnEndPoll) {
		if st, err := shift.Read(s.Crew.StateDir()); err == nil {
			s.State = st
			if s.TurnOver(role, session) {
				break
			}
		}

		if time.Now().After(deadline) {
			return
		}
	}

	_ = a.withState(func(s *shift.Shift) error {
		s.By = "turn-end"
		s.TurnEnd(role, session)

		return nil
	})
}
