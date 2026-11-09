package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *app) attachCommand() *cobra.Command {
	var raw bool
	c := &cobra.Command{
		Use:   "attach [<node>/<crew>] <role>",
		Short: "Open a role's terminal (shared Codex: native TUI; --raw: tmux bridge); <node>/<crew>: another crew's, over ssh -t",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			done, args, err := a.remote(cmd, args, true)
			if done || err != nil {
				return err
			}

			switch len(args) {
			case 0:
				return errors.New("attach takes a role after the address: zmina attach <node>/<crew> <role>")
			case 2:
				return fmt.Errorf("%q is no <node>/<crew> address: zmina attach <role>, or zmina attach <node>/<crew> <role>", args[0])
			}

			process, err := a.attachProcess(args[0], raw)
			if err != nil {
				return err
			}
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				argv := []string{"zmina", "--crew", a.crewPath(), "attach", args[0]}
				if raw {
					argv = append(argv, "--raw")
				}
				return fmt.Errorf("no terminal here; run it in one: %s", attachCommandLine(argv))
			}
			if process.Err != nil {
				return process.Err
			}
			if process.Dir != "" {
				if err := os.Chdir(process.Dir); err != nil {
					return err
				}
			}
			return syscall.Exec(process.Path, process.Args, process.Env)
		},
	}
	c.Flags().BoolVar(&raw, "raw", false, "attach to the original tmux console (Codex bridge debugging)")
	return c
}

// attachProcess prepares the native terminal without modifying the role, its queues or its saved thread.
func (a *app) attachProcess(name string, raw bool) (*exec.Cmd, error) {
	var process *exec.Cmd
	err := a.readState(func(s *shift.Shift) error {
		r, err := s.Role(name)
		if err != nil {
			return err
		}
		if r.IsCodex() && r.CodexTransport != "shared" && !raw {
			command := attachCommandLine([]string{"zmina", "--crew", s.Crew.Path, "attach", name, "--raw"})
			return fmt.Errorf("%s uses codex_transport: stdio; native TUI attachment requires codex_transport: shared and its daemon. For the current bridge console run: %s. Change transport only after stopping the role and its copies", name, command)
		}
		if r.IsCodex() && r.CodexTransport == "shared" && !raw {
			p, err := s.Tmux.Probe(name)
			if err != nil {
				return err
			}
			if p.State != tmux.Up {
				return fmt.Errorf("%s is not up; start it with zmina up %s before attaching", name, name)
			}
			reg := codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
			current, err := reg.ByPID(p.PID)
			if err != nil {
				return err
			}
			if current == nil || current.Name != r.Session || current.SessionID == "" {
				return fmt.Errorf("%s has no current Codex thread yet; wait for startup or inspect zmina attach %s --raw", name, name)
			}
			dir := current.Cwd
			if dir == "" {
				dir = s.State.Role(name).Dir
			}
			if dir == "" {
				dir = r.Dir
			}
			argv := []string{"resume", current.SessionID, "--remote", "unix://", "--cd", dir}
			for _, pair := range [][2]string{{"--profile", r.Profile}, {"--model", r.Model}, {"--sandbox", r.Sandbox}} {
				if pair[1] != "" {
					argv = append(argv, pair[0], pair[1])
				}
			}
			// Native CLI config rejects explicit "untrusted" too. Rejoin the live
			// daemon thread without a policy override so its existing policy survives.
			if r.ApprovalPolicy != "" && r.ApprovalPolicy != "untrusted" {
				argv = append(argv, "-c", "approval_policy="+strconv.Quote(r.ApprovalPolicy))
			}
			if r.ReasoningEffort != "" {
				argv = append(argv, "-c", "model_reasoning_effort="+strconv.Quote(r.ReasoningEffort))
			}
			// The thread's tier as the bridge set it, so the native client never switches it to the owner's global one.
			// A profile that does not read is left to the native client to report (the bridge could not have started).
			profile, _ := codex.LoadProfile(r.CodexHome, r.Profile)
			argv = append(argv, "-c", "service_tier="+strconv.Quote(codex.ServiceTier(r.ServiceTier, profile)))
			process = exec.Command(s.Crew.Codex, argv...)
			process.Dir = dir
			process.Env = attachEnvironment(os.Environ(), s.Env(r))
			return process.Err
		}
		client, err := s.Tmux.(*lazyTmux).client()
		if err != nil {
			return err
		}
		argv := client.AttachArgv(name)
		process = exec.Command(argv[0], argv[1:]...)
		process.Env = os.Environ()
		return process.Err
	})
	return process, err
}

func attachEnvironment(base, overrides []string) []string {
	replaced := map[string]bool{}
	for _, value := range overrides {
		key, _, _ := strings.Cut(value, "=")
		replaced[key] = true
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, value := range base {
		key, _, _ := strings.Cut(value, "=")
		if !replaced[key] {
			result = append(result, value)
		}
	}
	return append(result, overrides...)
}

func attachCommandLine(argv []string) string {
	args := make([]string, len(argv))
	for i, arg := range argv {
		args[i] = quoteCommandArg(arg)
	}
	return strings.Join(args, " ")
}
