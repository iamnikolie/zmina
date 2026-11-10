package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamnikolie/zmina/internal/codex"
)

// TestMain runs the package's tests in a sandbox: HOME is a temp dir, so a test that forgets its own never reads or
// writes the operator's ~/.zmina; the environment a role or a tmux pane leaves (ZMINA_CREW, ZMINA_ROLE, Claude Code's
// session variables, TMUX, CODEX_HOME, XDG_*) is dropped, so a test run from inside a role does not act on its live crew; and tmux keeps
// its sockets in a short temp dir, under macOS's 104-byte socket path limit.
func TestMain(m *testing.M) {
	os.Exit(sandboxed(m))
}

// sandboxMark is set in the sandbox: a test that runs this binary again as a child (a fake peer, a hint run by sh)
// hands it the environment it set up on purpose, which the child keeps.
const sandboxMark = "ZMINA_TEST_SANDBOX"

func sandboxed(m *testing.M) int {
	if os.Getenv(sandboxMark) != "" {
		return m.Run()
	}

	home, err := os.MkdirTemp("", "zmina-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmd tests:", err)

		return 1
	}
	defer func() { _ = os.RemoveAll(home) }()

	sockets, err := os.MkdirTemp("/tmp", "zt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmd tests:", err)

		return 1
	}
	defer func() { _ = os.RemoveAll(sockets) }()

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if inheritedFromARole(name) {
			_ = os.Unsetenv(name)
		}
	}

	_ = os.Setenv("HOME", home)
	_ = os.Setenv("TMUX_TMPDIR", sockets)
	_ = os.Setenv(sandboxMark, home)

	return m.Run()
}

// inheritedFromARole is an environment variable a role session, the cockpit or a tmux pane sets, which would point a
// test at the live crew or the operator's session.
func inheritedFromARole(name string) bool {
	switch name {
	case "TMUX", "TMUX_PANE", "CLAUDECODE", "CLAUDE_CONFIG_DIR", "CODEX_THREAD_ID", "CODEX_HOME":
		return true
	}

	// XDG_* moves config, data and state dirs (systemd units among them) off HOME.
	return strings.HasPrefix(name, "ZMINA_") || strings.HasPrefix(name, "CLAUDE_CODE_") || strings.HasPrefix(name, "XDG_")
}

func TestTheSandboxHoldsNoLiveCrew(t *testing.T) {
	home, _ := os.UserHomeDir()
	if !strings.Contains(home, "zmina-home-") {
		t.Fatalf("HOME is %s, not the sandbox", home)
	}

	for _, name := range []string{"ZMINA_CREW", "ZMINA_ROLE", "TMUX", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_SESSION_ID",
		"CODEX_HOME", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(name); ok {
			t.Errorf("%s=%s reached the tests", name, v)
		}
	}

	// What reads them falls back inside the sandbox.
	if dir := systemdDir(); !strings.HasPrefix(dir, home+string(os.PathSeparator)) {
		t.Errorf("systemd units go to %s, outside the sandbox", dir)
	}

	_, err := codex.LoadProfile("", "review")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, ".codex")) {
		t.Errorf("the Codex profile is read outside the sandbox: %v", err)
	}
}
