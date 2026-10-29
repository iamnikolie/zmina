package cmd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
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

	if err := os.MkdirAll(filepath.Dir(plistPath(label)), 0o755); err != nil {
		return err
	}

	plist := launchdPlist(label, bin, c.Path, tickLog(c))
	if err := os.WriteFile(plistPath(label), plist, 0o644); err != nil {
		return err
	}

	_ = launchctl("bootout", domain()+"/"+label)
	if err := launchctl("bootstrap", domain(), plistPath(label)); err != nil {
		return err
	}

	fmt.Fprintf(w, "ticker installed: %s · log %s\n", plistPath(label), tickLog(c))

	return nil
}

func launchdUninstall(w io.Writer, c *crew.Crew, _ string) error {
	_ = launchctl("bootout", domain()+"/"+c.LaunchdLabel())
	if err := os.Remove(plistPath(c.LaunchdLabel())); err != nil && !os.IsNotExist(err) {
		return err
	}

	fmt.Fprintln(w, "ticker uninstalled")

	return nil
}

func launchdStatus(w io.Writer, c *crew.Crew, _ string) error {
	out, err := exec.Command("launchctl", "print", domain()+"/"+c.LaunchdLabel()).CombinedOutput()
	if err != nil {
		fmt.Fprintln(w, "ticker not loaded")

		return nil
	}

	for _, line := range bytes.Split(out, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if bytes.HasPrefix(t, []byte("state =")) || bytes.HasPrefix(t, []byte("last exit code")) ||
			bytes.HasPrefix(t, []byte("runs =")) || bytes.HasPrefix(t, []byte("run interval")) {
			fmt.Fprintln(w, string(t))
		}
	}

	return nil
}

// launchdPlist runs the tick every minute and at load. AbandonProcessGroup keeps the tmux server the first tick may
// start alive after the tick exits.
func launchdPlist(label, bin, crewFile, log string) []byte {
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
	<array><string>%s</string><string>tick</string><string>--crew</string><string>%s</string></array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>%s</string>
		<key>HOME</key><string>%s</string>
		<key>LANG</key><string>%s</string>
	</dict>
	<key>StartInterval</key><integer>60</integer>
	<key>RunAtLoad</key><true/>
	<key>AbandonProcessGroup</key><true/>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, esc(label), esc(bin), esc(crewFile), esc(os.Getenv("PATH")), esc(os.Getenv("HOME")), esc(lang), esc(log), esc(log))
}

func plistPath(label string) string {
	return crew.ExpandHome("~/Library/LaunchAgents/" + label + ".plist")
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %s: %w", args[0], bytes.TrimSpace(out), err)
	}

	return nil
}
