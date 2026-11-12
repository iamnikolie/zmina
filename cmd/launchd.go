package cmd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
)

// launchdCommand is the macOS name of 'zmina ticker', kept for scripts that use it.
func (a *app) launchdCommand() *cobra.Command {
	c := &cobra.Command{
		Use:    "launchd",
		Short:  "The ticker as a launchd agent (macOS): same as 'zmina ticker'",
		Hidden: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("launchd is macOS; use 'zmina ticker'")
			}

			return nil
		},
	}

	b := backend{"launchd", launchdInstall, launchdUninstall, launchdStatus}
	c.AddCommand(a.tickerSubs(func() (backend, error) { return b, nil })...)

	return c
}

func launchdInstall(w io.Writer, c *crew.Crew, bin string) error {
	label := c.LaunchdLabel()

	if err := launchdLoad(label, launchdPlist(label, bin, c.Path, tickLog(c))); err != nil {
		return err
	}

	fmt.Fprintf(w, "ticker installed: %s · log %s\n", plistPath(label), tickLog(c))

	return launchdCourier(w, c, bin)
}

// launchdLoad writes an agent's plist and loads it, in place of one that is loaded already.
func launchdLoad(label string, plist []byte) error {
	if err := fsx.MkdirAll(filepath.Dir(plistPath(label)), 0o755); err != nil {
		return err
	}

	if err := fsx.WriteFile(plistPath(label), plist, 0o644); err != nil {
		return err
	}

	_ = launchctl("bootout", domain()+"/"+label)

	return launchctl("bootstrap", domain(), plistPath(label))
}

// launchdUnload stops an agent and removes its plist; removed says whether there was one.
func launchdUnload(label string) (removed bool, err error) {
	_ = launchctl("bootout", domain()+"/"+label)

	if err := os.Remove(plistPath(label)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// launchdCourier puts the courier's agent beside the ticker's, or, with courier: off in the crew file, takes the one
// that is there away. It is part of the ticker's install, not a command of its own.
func launchdCourier(w io.Writer, c *crew.Crew, bin string) error {
	label := c.CourierLabel()

	if !c.CourierOn() {
		removed, err := launchdUnload(label)
		if err != nil {
			return fmt.Errorf("courier: %w", err)
		}

		fmt.Fprintln(w, courierOffSaid(removed))

		return nil
	}

	if err := launchdLoad(label, launchdCourierPlist(label, bin, c.Path, courierLog(c))); err != nil {
		return fmt.Errorf("courier: %w", err)
	}

	fmt.Fprintf(w, "courier installed: %s · log %s\n", plistPath(label), courierLog(c))

	return nil
}

func launchdUninstall(w io.Writer, c *crew.Crew, _ string) error {
	if _, err := launchdUnload(c.LaunchdLabel()); err != nil {
		return err
	}

	fmt.Fprintln(w, "ticker uninstalled")

	removed, err := launchdUnload(c.CourierLabel())
	if err != nil {
		return fmt.Errorf("courier: %w", err)
	}

	if removed {
		fmt.Fprintln(w, "courier uninstalled")
	}

	return nil
}

func launchdStatus(w io.Writer, c *crew.Crew, _ string) error {
	out, err := runTool(serviceTimeout, true, "launchctl", "print", domain()+"/"+c.LaunchdLabel())
	if err != nil {
		fmt.Fprintln(w, "ticker not loaded")
	} else {
		for _, line := range bytes.Split(out, []byte("\n")) {
			t := bytes.TrimSpace(line)
			if bytes.HasPrefix(t, []byte("state =")) || bytes.HasPrefix(t, []byte("last exit code")) ||
				bytes.HasPrefix(t, []byte("runs =")) || bytes.HasPrefix(t, []byte("run interval")) {
				fmt.Fprintln(w, string(t))
			}
		}
	}

	return launchdCourierStatus(w, c)
}

// launchdCourierStatus says whether the courier's agent is loaded and what launchd knows of its runs.
func launchdCourierStatus(w io.Writer, c *crew.Crew) error {
	out, err := runTool(serviceTimeout, true, "launchctl", "print", domain()+"/"+c.CourierLabel())
	if err != nil {
		fmt.Fprintln(w, courierNotLoaded(c, fileExists(plistPath(c.CourierLabel()))))

		return nil
	}

	seen := map[string]bool{}

	for _, line := range bytes.Split(out, []byte("\n")) {
		t := string(bytes.TrimSpace(line))

		for _, key := range []string{"state =", "pid =", "runs =", "last exit code"} {
			if strings.HasPrefix(t, key) && !seen[key] {
				seen[key] = true

				fmt.Fprintln(w, "courier "+t)
			}
		}
	}

	return nil
}

// launchdPlist runs the tick every minute and at load. AbandonProcessGroup keeps the tmux server the first tick may
// start alive after the tick exits.
func launchdPlist(label, bin, crewFile, log string) []byte {
	return launchdAgent(label, bin, "tick", crewFile, log, `	<key>StartInterval</key><integer>60</integer>
	<key>RunAtLoad</key><true/>
`)
}

// launchdCourierPlist keeps 'zmina _courier' running: KeepAlive true starts it at load and starts it again whenever
// it ends, whatever its exit status (a courier ends with 0 for a new binary, its spent life or a signal, and a
// supervisor that let it lie would leave the crew with the beat alone). ThrottleInterval is the least time between two
// starts, so a courier that ends at once (no crew file yet, a state a newer zmina wrote, another courier running) is
// tried again every courierRestartSeconds, not in a spin. The environment, the crew file argument and the output are
// the ticker's, and so is AbandonProcessGroup.
func launchdCourierPlist(label, bin, crewFile, log string) []byte {
	return launchdAgent(label, bin, "_courier", crewFile, log, fmt.Sprintf(`	<key>KeepAlive</key><true/>
	<key>RunAtLoad</key><true/>
	<key>ThrottleInterval</key><integer>%d</integer>
`, courierRestartSeconds))
}

// launchdAgent is the plist of an agent that runs 'zmina <verb> --crew <crewFile>' with this shell's PATH, HOME and
// LANG and sends its output to log; when says when it runs.
func launchdAgent(label, bin, verb, crewFile, log, when string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))

		return b.String()
	}

	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "en_US.UTF-8"
	}

	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string><string>%s</string><string>--crew</string><string>%s</string></array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>%s</string>
		<key>HOME</key><string>%s</string>
		<key>LANG</key><string>%s</string>
	</dict>
%s	<key>AbandonProcessGroup</key><true/>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, esc(label), esc(bin), verb, esc(crewFile), esc(os.Getenv("PATH")), esc(os.Getenv("HOME")), esc(lang), when, esc(log), esc(log))
}

// launchdUnits says whether the plists of the crew's ticker and courier are on disk.
func launchdUnits(c *crew.Crew) (ticker, courier bool) {
	return fileExists(plistPath(c.LaunchdLabel())), fileExists(plistPath(c.CourierLabel()))
}

func plistPath(label string) string {
	return crew.ExpandHome("~/Library/LaunchAgents/" + label + ".plist")
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) error {
	out, err := runTool(serviceTimeout, true, "launchctl", args...)
	if err != nil {
		return fmt.Errorf("launchctl %s: %s: %w", args[0], bytes.TrimSpace(out), err)
	}

	return nil
}
