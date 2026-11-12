package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// A ticker backend installs, removes and reports the job that runs 'zmina tick' every minute.
type tickerOp func(w io.Writer, c *crew.Crew, bin string) error

type backend struct {
	name                       string
	install, uninstall, status tickerOp
}

func tickerBackend() (backend, error) {
	switch runtime.GOOS {
	case "darwin":
		return backend{"launchd", launchdInstall, launchdUninstall, launchdStatus}, nil
	case "linux":
		return backend{"systemd", systemdInstall, systemdUninstall, systemdStatus}, nil
	default:
		return backend{}, fmt.Errorf("no ticker for %s: run 'zmina tick' every minute from your own scheduler",
			runtime.GOOS)
	}
}

func (a *app) tickerCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "ticker",
		Short: "The ticker that runs 'zmina tick' every minute, and the courier beside it: launchd agents on macOS, systemd user units on Linux",
	}

	c.AddCommand(a.tickerSubs(tickerBackend)...)

	return c
}

func (a *app) tickerSubs(find func() (backend, error)) []*cobra.Command {
	sub := func(use, short string, op func(b backend) tickerOp, after func(c *crew.Crew)) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				b, err := find()
				if err != nil {
					return err
				}

				c, bin, err := a.tickerTarget()
				if err != nil {
					return err
				}

				if err := op(b)(a.out, c, bin); err != nil {
					return err
				}

				if after != nil {
					after(c)
				}

				return nil
			},
		}
	}

	return []*cobra.Command{
		sub("install", "Run 'zmina tick' every minute and keep the courier running beside it (courier: off in the crew file skips it), with this shell's PATH",
			func(b backend) tickerOp { return b.install }, nil),
		sub("uninstall", "Stop the ticker and the courier; the roles that are up stay up",
			func(b backend) tickerOp { return b.uninstall }, nil),
		sub("status", "Whether the ticker and the courier run, when it last beat, the end of tick.log",
			func(b backend) tickerOp { return b.status }, a.beatReport),
	}
}

// tickerTarget is the crew (with an absolute path, for the job) and the zmina binary the ticker should run.
func (a *app) tickerTarget() (*crew.Crew, string, error) {
	c, err := a.loadCrew()
	if err != nil {
		return nil, "", err
	}

	bin, err := exec.LookPath("zmina")
	if err != nil {
		if bin, err = os.Executable(); err != nil {
			return nil, "", err
		}
	}

	bin, _ = filepath.Abs(bin)

	return c, bin, nil
}

// beatReport prints when the ticker last beat and the last lines of tick.log.
func (a *app) beatReport(c *crew.Crew) {
	if st, err := shift.Read(c.StateDir()); err == nil {
		if st.LastTick.IsZero() {
			fmt.Fprintln(a.out, "last beat: none recorded")
		} else {
			fmt.Fprintf(a.out, "last beat: %s ago\n", time.Since(st.LastTick).Round(time.Second))
		}
	}

	f, err := os.Open(tickLog(c))
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var tail []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		tail = append(tail, sc.Text())
		if len(tail) > 5 {
			tail = tail[1:]
		}
	}

	if len(tail) > 0 {
		fmt.Fprintln(a.out, "tick.log:")

		for _, l := range tail {
			fmt.Fprintln(a.out, "  "+l)
		}
	}
}

// tickLog is where the ticker's output goes: tick.log next to the crew file.
func tickLog(c *crew.Crew) string { return c.Layout().TickLog() }

// courierLog is where the courier's output goes when its service runs it: courier.log next to the crew file. The
// retention pass cuts it as it cuts tick.log.
func courierLog(c *crew.Crew) string { return c.Layout().CourierLog() }

// courierRestartSeconds is the least time between two starts of the courier by its supervisor (launchd's
// ThrottleInterval, systemd's RestartSec): launchd's own default, said out loud. A courier that ends at once
// (a second one finds the first's lock; a crew file not there yet) is started again this often, each time one line in
// courier.log; and one that ended for a new binary is back within it, the beat delivering meanwhile.
const courierRestartSeconds = 10

// courierOffSaid is what the ticker's install says of the courier when the crew file has courier: off.
func courierOffSaid(removed bool) string {
	if removed {
		return "courier: off in the crew file; the one that was installed is removed"
	}

	return "courier: off in the crew file; none installed"
}

// courierNotLoaded is what 'ticker status' says of a courier its service manager does not know: there says whether
// its unit file is on disk.
func courierNotLoaded(c *crew.Crew, there bool) string {
	switch {
	case !c.CourierOn() && there:
		return "courier: off in the crew file, but its unit is still installed: zmina ticker install removes it"
	case !c.CourierOn():
		return "courier: off in the crew file"
	case there:
		return "courier: installed but not loaded: zmina ticker install loads it"
	}

	return "courier: not installed: zmina ticker install adds it"
}

// fileExists reports whether something is at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
