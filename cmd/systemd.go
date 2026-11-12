package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
)

// The Linux ticker: a oneshot user service that runs one tick, and a timer that starts it every minute. Beside it, the
// courier: a user service that keeps 'zmina _courier' running.

func systemdInstall(w io.Writer, c *crew.Crew, bin string) error {
	unit := c.SystemdUnit()
	dir := systemdDir()

	if err := fsx.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	if err := fsx.WriteFile(filepath.Join(dir, unit+".service"), systemdService(bin, c.Path, tickLog(c)), 0o644); err != nil {
		return err
	}

	if err := fsx.WriteFile(filepath.Join(dir, unit+".timer"), systemdTimer(c.Path), 0o644); err != nil {
		return err
	}

	if err := systemctl("daemon-reload"); err != nil {
		return err
	}

	if err := systemctl("enable", "--now", unit+".timer"); err != nil {
		return err
	}

	fmt.Fprintf(w, "ticker installed: %s.timer in %s · log %s\n", unit, dir, tickLog(c))

	if err := systemdCourier(w, c, bin); err != nil {
		return err
	}

	if !lingering() {
		fmt.Fprintln(w, "note: lingering is off, so the roles stop when you log out; 'loginctl enable-linger' keeps them")
	}

	return nil
}

// systemdCourier puts the courier's service beside the ticker's, or, with courier: off in the crew file, takes the one
// that is there away. It is part of the ticker's install, not a command of its own. A service that is installed
// again is restarted, so a new binary path or PATH takes hold.
func systemdCourier(w io.Writer, c *crew.Crew, bin string) error {
	unit := c.CourierUnit()

	if !c.CourierOn() {
		removed, err := systemdUnload(unit)
		if err != nil {
			return fmt.Errorf("courier: %w", err)
		}

		fmt.Fprintln(w, courierOffSaid(removed))

		return nil
	}

	if err := fsx.WriteFile(filepath.Join(systemdDir(), unit+".service"), systemdCourierService(bin, c.Path, courierLog(c)),
		0o644); err != nil {
		return fmt.Errorf("courier: %w", err)
	}

	for _, args := range [][]string{{"daemon-reload"}, {"enable", unit + ".service"}, {"restart", unit + ".service"}} {
		if err := systemctl(args...); err != nil {
			return fmt.Errorf("courier: %w", err)
		}
	}

	fmt.Fprintf(w, "courier installed: %s.service in %s · log %s\n", unit, systemdDir(), courierLog(c))

	return nil
}

// systemdUnload stops a service and removes its unit file; removed says whether there was one.
func systemdUnload(unit string) (removed bool, err error) {
	file := filepath.Join(systemdDir(), unit+".service")
	if !fileExists(file) {
		return false, nil
	}

	_ = systemctl("disable", "--now", unit+".service")

	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		return false, err
	}

	_ = systemctl("daemon-reload")

	return true, nil
}

func systemdUninstall(w io.Writer, c *crew.Crew, _ string) error {
	unit := c.SystemdUnit()
	_ = systemctl("disable", "--now", unit+".timer")

	for _, ext := range []string{".timer", ".service"} {
		if err := os.Remove(filepath.Join(systemdDir(), unit+ext)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	_ = systemctl("daemon-reload")
	fmt.Fprintln(w, "ticker uninstalled")

	removed, err := systemdUnload(c.CourierUnit())
	if err != nil {
		return fmt.Errorf("courier: %w", err)
	}

	if removed {
		fmt.Fprintln(w, "courier uninstalled")
	}

	return nil
}

func systemdStatus(w io.Writer, c *crew.Crew, _ string) error {
	unit := c.SystemdUnit()

	out, err := runTool(serviceTimeout, true, "systemctl", "--user", "show", unit+".timer", "-p", "ActiveState",
		"-p", "LastTriggerUSec", "-p", "NextElapseUSecRealtime")
	if err != nil || bytes.Contains(out, []byte("ActiveState=inactive")) && !unitExists(unit) {
		fmt.Fprintln(w, "ticker not installed")
		systemdCourierStatus(w, c)

		return nil
	}

	fmt.Fprint(w, string(out))

	if svc, err := runTool(serviceTimeout, true, "systemctl", "--user", "show", unit+".service", "-p", "Result",
		"-p", "ExecMainStatus"); err == nil {
		fmt.Fprint(w, string(svc))
	}

	systemdCourierStatus(w, c)
	fmt.Fprintf(w, "Linger=%v\n", lingering())

	return nil
}

// systemdCourierStatus says whether the courier's service is installed and what systemd knows of its runs: its state,
// how often it was started again, how its last run ended.
func systemdCourierStatus(w io.Writer, c *crew.Crew) {
	unit := c.CourierUnit()

	if !fileExists(filepath.Join(systemdDir(), unit+".service")) {
		fmt.Fprintln(w, courierNotLoaded(c, false))

		return
	}

	out, err := runTool(serviceTimeout, true, "systemctl", "--user", "show", unit+".service", "-p", "ActiveState",
		"-p", "SubState", "-p", "NRestarts", "-p", "Result", "-p", "ExecMainStatus")
	if err != nil {
		fmt.Fprintln(w, "courier: its service manager does not answer")

		return
	}

	if !c.CourierOn() {
		fmt.Fprintln(w, courierNotLoaded(c, true))
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			fmt.Fprintln(w, "courier "+line)
		}
	}
}

// systemdService runs one tick. KillMode=process leaves the tmux server the first tick may start alive after the
// tick exits — without it systemd would end every role with the unit.
func systemdService(bin, crewFile, log string) []byte {
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "en_US.UTF-8"
	}

	return fmt.Appendf(nil, `[Unit]
Description=zmina ticker: one beat for %s

[Service]
Type=oneshot
ExecStart=%s tick --crew %s
Environment=%s
Environment=%s
Environment=%s
KillMode=process
StandardOutput=append:%s
StandardError=append:%s
`, unitEsc(crewFile), unitArg(bin), unitArg(crewFile), unitArg("PATH="+os.Getenv("PATH")),
		unitArg("HOME="+os.Getenv("HOME")), unitArg("LANG="+lang), unitEsc(log), unitEsc(log))
}

// systemdCourierService keeps 'zmina _courier' running. Restart=always starts it again whenever it ends, whatever its
// exit status (a courier ends with 0 for a new binary, its spent life or a signal), after RestartSec. StartLimitIntervalSec=0
// turns systemd's start-rate limit off: with the limit on, a burst of quick ends (the binary replaced half way, a
// crew file not there for a moment) would leave the unit failed for good, and nothing would start it again;
// RestartSec alone spaces the starts. The environment, the crew file argument, the output and KillMode=process are
// the ticker's: a tmux client the courier started is not killed with it, and never the roles' tmux server.
func systemdCourierService(bin, crewFile, log string) []byte {
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "en_US.UTF-8"
	}

	return fmt.Appendf(nil, `[Unit]
Description=zmina courier: delivers a waiting line at once for %s
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=%s _courier --crew %s
Environment=%s
Environment=%s
Environment=%s
Restart=always
RestartSec=%d
KillMode=process
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, unitEsc(crewFile), unitArg(bin), unitArg(crewFile), unitArg("PATH="+os.Getenv("PATH")),
		unitArg("HOME="+os.Getenv("HOME")), unitArg("LANG="+lang), courierRestartSeconds, unitEsc(log), unitEsc(log))
}

// systemdTimer starts the service at the start of every minute.
func systemdTimer(crewFile string) []byte {
	return fmt.Appendf(nil, `[Unit]
Description=zmina ticker every minute for %s

[Timer]
OnCalendar=minutely
AccuracySec=1s

[Install]
WantedBy=timers.target
`, unitEsc(crewFile))
}

// unitEsc escapes systemd's specifier character; unitArg also quotes one command-line word or assignment.
func unitEsc(s string) string { return strings.ReplaceAll(s, "%", "%%") }

func unitArg(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(unitEsc(s))

	return `"` + s + `"`
}

func systemdDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user")
	}

	return crew.ExpandHome("~/.config/systemd/user")
}

func unitExists(unit string) bool {
	_, err := os.Stat(filepath.Join(systemdDir(), unit+".timer"))

	return err == nil
}

// systemdUnits says whether the crew's ticker timer and courier service are on disk.
func systemdUnits(c *crew.Crew) (ticker, courier bool) {
	return unitExists(c.SystemdUnit()), fileExists(filepath.Join(systemdDir(), c.CourierUnit()+".service"))
}

// lingering asks logind whether this user's services outlive a logout. $USER is often unset under systemd: the
// user is looked up.
func lingering() bool {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}

	out, err := runTool(serviceTimeout, true, "loginctl", "show-user", name, "-p", "Linger")

	return err == nil && strings.TrimSpace(string(out)) == "Linger=yes"
}

func systemctl(args ...string) error {
	out, err := runTool(serviceTimeout, true, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %s: %w", args[0], bytes.TrimSpace(out), err)
	}

	return nil
}
