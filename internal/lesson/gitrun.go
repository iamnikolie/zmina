package lesson

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// The triage's git calls all end: a remote that never answers (a credential prompt nobody sees, a dead ssh) must not
// hold the detached run for good, where nothing watches it. A call on local files gets GitTimeout, one that talks to
// the remote GitNetTimeout; both are variables so tests can shorten them.
var (
	GitTimeout    = 2 * time.Minute
	GitNetTimeout = 5 * time.Minute
)

// gitWaitDelay is how long a git call's Wait keeps waiting for its output once git itself has ended (or was
// killed): a process git started (ssh, a hook) may hold the pipes far longer.
const gitWaitDelay = 3 * time.Second

// terminalDriven reports whether a terminal drives this run (zmina lesson triage by hand, zmina doctor); a variable
// so tests need not depend on how they were started.
var terminalDriven = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// gitTimeout is how long a git command may take: the commands that talk to the remote get the longer one.
func gitTimeout(args []string) time.Duration {
	if len(args) > 0 {
		switch args[0] {
		case "fetch", "push", "pull", "ls-remote", "clone":
			return GitNetTimeout
		}
	}

	return GitTimeout
}

// Git runs git in a directory and returns its output. It ends after GitTimeout (GitNetTimeout for a command that
// talks to the remote), see GitContext.
func Git(dir string, args ...string) (string, error) {
	return GitContext(context.Background(), dir, args...)
}

// GitContext is Git that also ends when ctx does. At the end git is killed, and with it, when no terminal drives
// the run (the daily run is detached), everything it started: ssh and the repository's hooks die with it instead of
// outliving it holding its pipes. Run from a terminal, git stays in the caller's process group, since the owner's
// terminal may drive it (a passphrase prompt, Ctrl-C), and only git itself is killed; Wait returns shortly after
// either way.
func GitContext(ctx context.Context, dir string, args ...string) (string, error) {
	out, stderr, err := runGit(ctx, dir, false, args)
	if err != nil {
		if why := strings.TrimSpace(stderr); why != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, why)
		}

		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	return strings.TrimSpace(out), nil
}

// gitCombined is git whose output, standard and error together, comes back even when it failed (a push's reply is
// on its standard error).
func gitCombined(ctx context.Context, dir string, args ...string) (string, error) {
	out, _, err := runGit(ctx, dir, true, args)

	return out, err
}

// runGit runs git with the deadline of its command and returns its output; combined has stderr in out.
func runGit(parent context.Context, dir string, combined bool, args []string) (out, stderr string, err error) {
	timeout := gitTimeout(args)

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.WaitDelay = gitWaitDelay

	if !terminalDriven() {
		ownGroup(cmd)
	}

	var stdout, errb bytes.Buffer

	cmd.Stdout = &stdout

	if combined {
		cmd.Stderr = &stdout
	} else {
		cmd.Stderr = &errb
	}

	err = cmd.Run()
	if err != nil {
		switch {
		case parent.Err() != nil:
			err = fmt.Errorf("stopped: %w", parent.Err())
		case ctx.Err() != nil:
			err = fmt.Errorf("no answer in %s", timeout)
		}
	}

	return stdout.String(), errb.String(), err
}

// ownGroup runs c in a process group of its own and has the end of its context kill the whole group.
func ownGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}

		return err
	}
}
