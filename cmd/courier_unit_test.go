package cmd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// serviceFakes puts launchctl, systemctl and loginctl first on PATH, as scripts that write their command line to a
// log and answer from sh: the real service manager is never asked. answer is shell, run with the program's name in $tool
// and its arguments in "$@"; it may print, and an exit status other than 0 is the program's own. The calls are read
// back with the returned function.
func serviceFakes(t *testing.T, answer string) (calls func() []string) {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")

	script := "#!/bin/sh\ntool=$(basename \"$0\")\necho \"$tool $*\" >> '" + log + "'\n" + answer + "\n"

	for _, name := range []string{"launchctl", "systemctl", "loginctl"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		raw, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		require.NoError(t, err)

		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// unitCrew is a crew whose files, and whose launchd and systemd units, are in a directory of the test's own: HOME is
// moved there, so the units the install writes are in no one's real ~/Library or ~/.config.
func unitCrew(t *testing.T, name, courier string) *crew.Crew {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	return &crew.Crew{Path: filepath.Join(home, ".zmina", "crew.yaml"), Name: name, Courier: courier}
}

func index(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}

	return -1
}

// The ticker's plist is what it was before the courier had one; the courier's keeps its job alive.
func TestLaunchdPlists(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/a&b/bin")
	t.Setenv("HOME", "/srv/u")
	t.Setenv("LANG", "")

	ticker := string(launchdPlist("io.example.tick", "/opt/zmina", "/c/crew.yaml", "/c/tick.log"))
	assert.Equal(t, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>io.example.tick</string>
	<key>ProgramArguments</key>
	<array><string>/opt/zmina</string><string>tick</string><string>--crew</string><string>/c/crew.yaml</string></array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>/usr/bin:/opt/a&amp;b/bin</string>
		<key>HOME</key><string>/srv/u</string>
		<key>LANG</key><string>en_US.UTF-8</string>
	</dict>
	<key>StartInterval</key><integer>60</integer>
	<key>RunAtLoad</key><true/>
	<key>AbandonProcessGroup</key><true/>
	<key>StandardOutPath</key><string>/c/tick.log</string>
	<key>StandardErrorPath</key><string>/c/tick.log</string>
</dict>
</plist>
`, ticker)

	courier := string(launchdCourierPlist("io.example.courier", "/opt/zmina", "/c/crew.yaml", "/c/courier.log"))
	assert.Equal(t, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>io.example.courier</string>
	<key>ProgramArguments</key>
	<array><string>/opt/zmina</string><string>_courier</string><string>--crew</string><string>/c/crew.yaml</string></array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>/usr/bin:/opt/a&amp;b/bin</string>
		<key>HOME</key><string>/srv/u</string>
		<key>LANG</key><string>en_US.UTF-8</string>
	</dict>
	<key>KeepAlive</key><true/>
	<key>RunAtLoad</key><true/>
	<key>ThrottleInterval</key><integer>10</integer>
	<key>AbandonProcessGroup</key><true/>
	<key>StandardOutPath</key><string>/c/courier.log</string>
	<key>StandardErrorPath</key><string>/c/courier.log</string>
</dict>
</plist>
`, courier)

	assert.NotContains(t, courier, "StartInterval", "it is not run on a clock: it is kept running")

	// Both read as XML to the end: a path with & does not break the file.
	for _, doc := range []string{ticker, courier} {
		dec := xml.NewDecoder(strings.NewReader(doc))

		for {
			_, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}

			require.NoError(t, err)
		}
	}
}

// The courier's service restarts whatever way the courier ended, without a limit on the starts, and has the ticker's
// environment, crew file and kill mode; its output goes to courier.log.
func TestSystemdCourierService(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/srv/a b/bin")
	t.Setenv("HOME", "/srv/a b")
	t.Setenv("LANG", "")

	svc := string(systemdCourierService("/srv/a b/bin/zmina", "/srv/a b/.zmina/100%/crew.yaml",
		"/srv/a b/.zmina/courier.log"))

	unit, service, _ := strings.Cut(svc, "[Service]")

	assert.Contains(t, unit, "StartLimitIntervalSec=0", "no start-rate limit: a burst of quick ends cannot leave it failed for good")
	assert.NotContains(t, service, "StartLimit", "in [Unit], where systemd reads it")
	assert.Contains(t, service, `ExecStart="/srv/a b/bin/zmina" _courier --crew "/srv/a b/.zmina/100%%/crew.yaml"`,
		"words with spaces are quoted, % is not a specifier")
	assert.Contains(t, service, "Type=simple")
	assert.Contains(t, service, "Restart=always", "a courier that ends with 0 is started again too")
	assert.Contains(t, service, "RestartSec=10")
	assert.Contains(t, service, `Environment="PATH=/usr/bin:/srv/a b/bin"`)
	assert.Contains(t, service, `Environment="HOME=/srv/a b"`)
	assert.Contains(t, service, `Environment="LANG=en_US.UTF-8"`)
	assert.Contains(t, service, "KillMode=process", "the ticker's: whatever it started outlives the unit")
	assert.Contains(t, service, "StandardOutput=append:/srv/a b/.zmina/courier.log")
	assert.Contains(t, service, "StandardError=append:/srv/a b/.zmina/courier.log")
	assert.Contains(t, svc, "[Install]\nWantedBy=default.target")
	assert.NotContains(t, svc, "oneshot")
	assert.NotContains(t, svc, "[Timer]")
}

// systemd: the ticker's install also installs the courier's service, enabled and started; its units are two files of
// the crew's names beside the ticker's.
func TestSystemdInstallInstallsTheCourierBesideTheTicker(t *testing.T) {
	calls := serviceFakes(t, "")
	c := unitCrew(t, "ops", "")

	var out bytes.Buffer

	require.NoError(t, systemdInstall(&out, c, "/opt/zmina"))

	dir := systemdDir()
	for _, f := range []string{"zmina-ops.service", "zmina-ops.timer", "zmina-ops.courier.service"} {
		assert.FileExists(t, filepath.Join(dir, f))
	}

	svc, err := os.ReadFile(filepath.Join(dir, "zmina-ops.courier.service"))
	require.NoError(t, err)
	assert.Contains(t, string(svc), "ExecStart=\"/opt/zmina\" _courier --crew \""+c.Path+"\"")
	assert.Contains(t, string(svc), "StandardOutput=append:"+filepath.Join(filepath.Dir(c.Path), "courier.log"))

	got := calls()
	assert.Contains(t, got, "systemctl --user enable --now zmina-ops.timer")
	assert.Contains(t, got, "systemctl --user enable zmina-ops.courier.service")
	assert.Contains(t, got, "systemctl --user restart zmina-ops.courier.service",
		"restarted, so a service installed again takes the new binary and PATH")
	assert.Less(t, index(got, "systemctl --user enable --now zmina-ops.timer"),
		index(got, "systemctl --user enable zmina-ops.courier.service"), "the ticker first")
	assert.Less(t, index(got, "systemctl --user enable zmina-ops.courier.service"),
		index(got, "systemctl --user restart zmina-ops.courier.service"))

	assert.Contains(t, out.String(), "ticker installed: zmina-ops.timer in "+dir)
	assert.Contains(t, out.String(), "courier installed: zmina-ops.courier.service in "+dir)
	assert.Contains(t, out.String(), filepath.Join(filepath.Dir(c.Path), "courier.log"))
}

// courier: off installs no courier service, and takes away one an earlier install left; with none there it says so and
// asks systemd nothing about it.
func TestSystemdInstallWithTheCourierOff(t *testing.T) {
	calls := serviceFakes(t, "")
	c := unitCrew(t, "", "off")

	var out bytes.Buffer

	require.NoError(t, systemdInstall(&out, c, "/opt/zmina"))

	dir := systemdDir()
	assert.FileExists(t, filepath.Join(dir, "zmina.timer"))
	assert.NoFileExists(t, filepath.Join(dir, "zmina.courier.service"))
	assert.Contains(t, out.String(), "courier: off in the crew file; none installed")

	for _, call := range calls() {
		assert.NotContains(t, call, "courier", "systemd is not asked about a unit that is not there")
	}

	// The same crew with an installed courier: the next install removes it.
	svc := filepath.Join(dir, "zmina.courier.service")
	require.NoError(t, os.WriteFile(svc, []byte("[Service]\n"), 0o644))

	out.Reset()
	require.NoError(t, systemdInstall(&out, c, "/opt/zmina"))

	assert.NoFileExists(t, svc)
	assert.Contains(t, out.String(), "courier: off in the crew file; the one that was installed is removed")
	assert.Contains(t, calls(), "systemctl --user disable --now zmina.courier.service")
	assert.NotContains(t, calls(), "systemctl --user restart zmina.courier.service")
	assert.FileExists(t, filepath.Join(dir, "zmina.timer"), "the ticker stays")
}

// Removing the ticker removes the courier, even one the crew file now says is off.
func TestSystemdUninstallRemovesTheCourier(t *testing.T) {
	calls := serviceFakes(t, "")
	c := unitCrew(t, "ops", "")

	require.NoError(t, systemdInstall(io.Discard, c, "/opt/zmina"))

	c.Courier = "off"

	var out bytes.Buffer

	require.NoError(t, systemdUninstall(&out, c, ""))

	dir := systemdDir()
	for _, f := range []string{"zmina-ops.service", "zmina-ops.timer", "zmina-ops.courier.service"} {
		assert.NoFileExists(t, filepath.Join(dir, f))
	}

	assert.Contains(t, calls(), "systemctl --user disable --now zmina-ops.courier.service")
	assert.Contains(t, out.String(), "ticker uninstalled")
	assert.Contains(t, out.String(), "courier uninstalled")

	// Nothing left to remove: the second time says nothing of the courier.
	out.Reset()
	require.NoError(t, systemdUninstall(&out, c, ""))
	assert.NotContains(t, out.String(), "courier")
}

// A courier service that cannot be started is an error that says whose it is; the ticker stays installed.
func TestSystemdInstallSaysWhenTheCourierFails(t *testing.T) {
	serviceFakes(t, `case "$*" in *restart*) echo "unit is masked"; exit 1;; esac`)
	c := unitCrew(t, "", "")

	err := systemdInstall(io.Discard, c, "/opt/zmina")
	require.ErrorContains(t, err, "courier: systemctl --user restart: unit is masked")
	assert.FileExists(t, filepath.Join(systemdDir(), "zmina.timer"))
}

// systemd status: the courier's state and restarts under the ticker's, or what to run when it is not installed.
func TestSystemdStatusShowsTheCourier(t *testing.T) {
	serviceFakes(t, `case "$*" in
*courier*) printf 'ActiveState=active\nSubState=running\nNRestarts=3\nResult=success\nExecMainStatus=0\n';;
*timer*) printf 'ActiveState=active\n';;
*) printf 'Result=success\n';;
esac`)

	c := unitCrew(t, "", "")
	require.NoError(t, systemdInstall(io.Discard, c, "/opt/zmina"))

	var out bytes.Buffer

	require.NoError(t, systemdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "courier ActiveState=active")
	assert.Contains(t, out.String(), "courier SubState=running")
	assert.Contains(t, out.String(), "courier NRestarts=3")

	// A ticker installed before the courier existed: status says what adds it.
	require.NoError(t, os.Remove(filepath.Join(systemdDir(), "zmina.courier.service")))

	out.Reset()
	require.NoError(t, systemdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "courier: not installed: zmina ticker install adds it")
	assert.NotContains(t, out.String(), "courier ActiveState")

	// Off in the crew file with the unit still there: says it is to be removed.
	require.NoError(t, os.WriteFile(filepath.Join(systemdDir(), "zmina.courier.service"), []byte("[Service]\n"), 0o644))
	c.Courier = "off"

	out.Reset()
	require.NoError(t, systemdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "courier: off in the crew file, but its unit is still installed: zmina ticker install removes it")
}

// launchd: the ticker's install also installs the courier's agent, loaded; each agent has its own plist and label.
func TestLaunchdInstallInstallsTheCourierBesideTheTicker(t *testing.T) {
	calls := serviceFakes(t, "")
	c := unitCrew(t, "ops", "")

	var out bytes.Buffer

	require.NoError(t, launchdInstall(&out, c, "/opt/zmina"))

	tick, courier := c.LaunchdLabel(), c.CourierLabel()
	assert.Equal(t, "io.iamnikolie.zmina.ops", tick)
	assert.Equal(t, "io.iamnikolie.zmina-courier.ops", courier)

	assert.FileExists(t, plistPath(tick))
	assert.FileExists(t, plistPath(courier))

	plist, err := os.ReadFile(plistPath(courier))
	require.NoError(t, err)
	assert.Contains(t, string(plist), "<string>_courier</string><string>--crew</string><string>"+c.Path+"</string>")
	assert.Contains(t, string(plist), "<key>KeepAlive</key><true/>")

	got := calls()
	boot := "launchctl bootstrap " + domain() + " "
	assert.Contains(t, got, boot+plistPath(tick))
	assert.Contains(t, got, boot+plistPath(courier))
	assert.Contains(t, got, "launchctl bootout "+domain()+"/"+courier, "an agent that is loaded is replaced")
	assert.Less(t, index(got, boot+plistPath(tick)), index(got, boot+plistPath(courier)), "the ticker first")

	assert.Contains(t, out.String(), "ticker installed: "+plistPath(tick))
	assert.Contains(t, out.String(), "courier installed: "+plistPath(courier)+" · log "+
		filepath.Join(filepath.Dir(c.Path), "courier.log"))
}

// courier: off installs no courier agent and removes one that is there.
func TestLaunchdInstallWithTheCourierOff(t *testing.T) {
	calls := serviceFakes(t, "")
	c := unitCrew(t, "", "off")

	var out bytes.Buffer

	require.NoError(t, launchdInstall(&out, c, "/opt/zmina"))
	assert.FileExists(t, plistPath(c.LaunchdLabel()))
	assert.NoFileExists(t, plistPath(c.CourierLabel()))
	assert.Contains(t, out.String(), "courier: off in the crew file; none installed")
	assert.NotContains(t, calls(), "launchctl bootstrap "+domain()+" "+plistPath(c.CourierLabel()))

	require.NoError(t, os.WriteFile(plistPath(c.CourierLabel()), []byte("<plist/>"), 0o644))

	out.Reset()
	require.NoError(t, launchdInstall(&out, c, "/opt/zmina"))

	assert.NoFileExists(t, plistPath(c.CourierLabel()))
	assert.Contains(t, out.String(), "courier: off in the crew file; the one that was installed is removed")
	assert.Contains(t, calls(), "launchctl bootout "+domain()+"/"+c.CourierLabel(), "and unloaded")
	assert.NotContains(t, calls(), "launchctl bootstrap "+domain()+" "+plistPath(c.CourierLabel()))
}

func TestLaunchdUninstallRemovesTheCourier(t *testing.T) {
	calls := serviceFakes(t, "")
	c := unitCrew(t, "", "")

	require.NoError(t, launchdInstall(io.Discard, c, "/opt/zmina"))

	c.Courier = "off"

	var out bytes.Buffer

	require.NoError(t, launchdUninstall(&out, c, ""))
	assert.NoFileExists(t, plistPath(c.LaunchdLabel()))
	assert.NoFileExists(t, plistPath(c.CourierLabel()))
	assert.Contains(t, calls(), "launchctl bootout "+domain()+"/"+c.CourierLabel())
	assert.Contains(t, out.String(), "ticker uninstalled")
	assert.Contains(t, out.String(), "courier uninstalled")

	out.Reset()
	require.NoError(t, launchdUninstall(&out, c, ""))
	assert.NotContains(t, out.String(), "courier")
}

func TestLaunchdInstallSaysWhenTheCourierFails(t *testing.T) {
	serviceFakes(t, `case "$*" in *zmina-courier*) echo "Bootstrap failed: 5"; exit 5;; esac`)
	c := unitCrew(t, "", "")

	err := launchdInstall(io.Discard, c, "/opt/zmina")
	require.ErrorContains(t, err, "courier: launchctl bootstrap: Bootstrap failed: 5")
	assert.FileExists(t, plistPath(c.LaunchdLabel()), "the ticker is installed")
}

// launchd status: the courier's own state, pid and runs under the ticker's, once each, or what to run.
func TestLaunchdStatusShowsTheCourier(t *testing.T) {
	serviceFakes(t, `case "$*" in
*print*zmina-courier*) printf '\tstate = running\n\tpid = 4242\n\truns = 7\n\tlast exit code = 0\n\t\tstate = active\n';;
*print*) printf '\tstate = not running\n\truns = 100\n\tlast exit code = 0\n\trun interval = 60 seconds\n';;
esac`)

	c := unitCrew(t, "", "")

	var out bytes.Buffer

	require.NoError(t, launchdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "run interval = 60 seconds")
	assert.Contains(t, out.String(), "courier state = running\n")
	assert.Contains(t, out.String(), "courier pid = 4242")
	assert.Contains(t, out.String(), "courier runs = 7")
	assert.Contains(t, out.String(), "courier last exit code = 0")
	assert.Equal(t, 1, strings.Count(out.String(), "courier state ="), "a nested state line is not repeated")

	// Not loaded: what adds it, or that it is off.
	serviceFakes(t, `case "$*" in *zmina-courier*) exit 113;; esac`)

	out.Reset()
	require.NoError(t, launchdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "courier: not installed: zmina ticker install adds it")

	require.NoError(t, os.MkdirAll(filepath.Dir(plistPath(c.CourierLabel())), 0o755))
	require.NoError(t, os.WriteFile(plistPath(c.CourierLabel()), []byte("<plist/>"), 0o644))

	out.Reset()
	require.NoError(t, launchdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "courier: installed but not loaded: zmina ticker install loads it")

	c.Courier = "off"

	out.Reset()
	require.NoError(t, launchdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "courier: off in the crew file, but its unit is still installed")
}

// Doctor and the install find the units where the install put them, on either system: the file of each is the sign.
func TestUnitFilesAreWhatTheInstalledChecksLookFor(t *testing.T) {
	c := unitCrew(t, "ops", "")

	for name, units := range map[string]func(*crew.Crew) (bool, bool){"launchd": launchdUnits, "systemd": systemdUnits} {
		ticker, courier := units(c)
		assert.False(t, ticker || courier, name+": nothing installed yet")
	}

	require.NoError(t, os.MkdirAll(systemdDir(), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(systemdDir(), "zmina-ops.timer"), nil, 0o644))

	ticker, courier := systemdUnits(c)
	assert.True(t, ticker)
	assert.False(t, courier, "a ticker alone is not a courier")

	require.NoError(t, os.WriteFile(filepath.Join(systemdDir(), "zmina-ops.courier.service"), nil, 0o644))

	ticker, courier = systemdUnits(c)
	assert.True(t, ticker && courier)

	require.NoError(t, os.MkdirAll(filepath.Dir(plistPath(c.LaunchdLabel())), 0o755))
	require.NoError(t, os.WriteFile(plistPath(c.CourierLabel()), nil, 0o644))

	ticker, courier = launchdUnits(c)
	assert.False(t, ticker, "the courier's plist is not the ticker's")
	assert.True(t, courier)

	// This machine's pair is the one of its own service manager.
	want := map[string]func(*crew.Crew) (bool, bool){"darwin": launchdUnits, "linux": systemdUnits}[runtime.GOOS]
	if want != nil {
		wantT, wantC := want(c)
		gotT, gotC := installedUnits(c)
		assert.Equal(t, []bool{wantT, wantC}, []bool{gotT, gotC})
	}
}
