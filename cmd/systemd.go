package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
)

// The Linux ticker: a oneshot user service that runs one tick, and a timer that starts it every minute.

func systemdInstall(w io.Writer, c *crew.Crew, bin string) error {
	unit := c.SystemdUnit()
	dir := systemdDir()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, unit+".service"), systemdService(bin, c.Path, tickLog(c)), 0o644); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, unit+".timer"), systemdTimer(c.Path), 0o644); err != nil {
		return err
	}

	if err := systemctl("daemon-reload"); err != nil {
		return err
	}

	if err := systemctl("enable", "--now", unit+".timer"); err != nil {
		return err
	}

	fmt.Fprintf(w, "ticker installed: %s.timer in %s · log %s\n", unit, dir, tickLog(c))

	if !lingering() {
		fmt.Fprintln(w, "note: lingering is off, so the roles stop when you log out; 'loginctl enable-linger' keeps them")
	}

	return nil
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

	return nil
}

func systemdStatus(w io.Writer, c *crew.Crew, _ string) error {
	unit := c.SystemdUnit()

	out, err := exec.Command("systemctl", "--user", "show", unit+".timer", "-p", "ActiveState",
		"-p", "LastTriggerUSec", "-p", "NextElapseUSecRealtime").CombinedOutput()
	if err != nil || bytes.Contains(out, []byte("ActiveState=inactive")) && !unitExists(unit) {
		fmt.Fprintln(w, "ticker not installed")

		return nil
	}

	fmt.Fprint(w, string(out))

	if svc, err := exec.Command("systemctl", "--user", "show", unit+".service", "-p", "Result",
		"-p", "ExecMainStatus").CombinedOutput(); err == nil {
		fmt.Fprint(w, string(svc))
	}

	fmt.Fprintf(w, "Linger=%v\n", lingering())

	return nil
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

// lingering asks logind whether this user's services outlive a logout. $USER is often unset under systemd: the
// user is looked up.
func lingering() bool {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}

	out, err := exec.Command("loginctl", "show-user", name, "-p", "Linger").CombinedOutput()

	return err == nil && strings.TrimSpace(string(out)) == "Linger=yes"
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %s: %w", args[0], bytes.TrimSpace(out), err)
	}

	return nil
}
