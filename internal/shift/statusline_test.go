package shift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type statusLineSetting struct {
	StatusLine map[string]any `json:"statusLine"`
}

func roleStatusLine(t *testing.T, f fix, role string) map[string]any {
	t.Helper()

	var got statusLineSetting
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings(role)), &got))

	return got.StatusLine
}

func writeSettings(t *testing.T, dir, body string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600))
}

// A role's status line is the tap; the owner's own is wrapped by it, never replaced, and the settings files it comes
// from are never written.
func TestRoleStatusLineWrapsTheOwnersOwn(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/zmina"
	f.s.Crew.Namespace = t.TempDir()
	writeSettings(t, f.s.Crew.Namespace, `{"theme":"dark","statusLine":{"type":"command","command":"~/bin/line.sh --short 'a b'","padding":1}}`)

	before, err := os.ReadFile(filepath.Join(f.s.Crew.Namespace, "settings.json"))
	require.NoError(t, err)

	base := "'/opt/zmina' --crew '" + f.s.Crew.Path + "'"
	want := map[string]any{"type": "command", "padding": 1.0, "command": base + ` _limits-tap -- '~/bin/line.sh --short '"'"'a b'"'"''`}

	assert.Equal(t, want, roleStatusLine(t, f, "designer"))
	assert.Equal(t, want, roleStatusLine(t, f, "designer#2"), "a copy has its role's")

	after, err := os.ReadFile(filepath.Join(f.s.Crew.Namespace, "settings.json"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "the owner's config is only read")
}

func TestRoleStatusLineTapsAloneWhenTheOwnerHasNone(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/zmina"
	f.s.Crew.Namespace = t.TempDir()

	assert.Equal(t, map[string]any{"type": "command", "command": "'/opt/zmina' --crew '" + f.s.Crew.Path + "' _limits-tap"},
		roleStatusLine(t, f, "designer"))

	writeSettings(t, f.s.Crew.Namespace, `{"theme":"dark"}`)
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' _limits-tap", roleStatusLine(t, f, "designer")["command"])
}

// The status line Claude Code would pick for the role is the one wrapped: a project's own wins over the namespace's.
func TestRoleStatusLineWrapsTheOneTheRoleWouldDraw(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/zmina"
	f.s.Crew.Namespace = t.TempDir()
	writeSettings(t, f.s.Crew.Namespace, `{"statusLine":{"type":"command","command":"/ns/line.sh"}}`)

	project := filepath.Join(f.s.Crew.Roles["designer"].Dir, ".claude")
	require.NoError(t, os.MkdirAll(project, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(project, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"/project/line.sh"}}`), 0o600))

	assert.Contains(t, roleStatusLine(t, f, "designer")["command"], "-- '/project/line.sh'")
	assert.Contains(t, roleStatusLine(t, f, "harvester")["command"], "-- '/project/line.sh'", "same directory, same project")

	require.NoError(t, os.WriteFile(filepath.Join(project, "settings.local.json"), []byte(`{"statusLine":{"type":"command","command":"/local/line.sh"}}`), 0o600))
	assert.Contains(t, roleStatusLine(t, f, "designer")["command"], "-- '/local/line.sh'")
}

// Without a zmina executable there are no hooks and no tap: settings stay as TestAckHookInSettings has them.
func TestNoTapWithoutAnExecutable(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = ""

	var plain map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &plain))
	assert.NotContains(t, plain, "statusLine")
}

// Only a role that takes its lines on its own channel starts with zmina's settings, so only it carries the tap: a role
// typed to (delivery tmux) and every session the owner starts keep their own status line.
func TestOnlyTheRoleStartsWithTheTap(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/zmina"
	require.NoError(t, f.s.Up("designer"))
	assert.Contains(t, strings.Join(f.tm.argv["designer"], " "), "_limits-tap")

	f.s.Crew.Delivery = "tmux"
	r, err := f.s.Role("designer")
	require.NoError(t, err)
	assert.NotContains(t, f.s.Command(r, ""), "--settings")
}

func writeProjectLine(t *testing.T, dir, file, command string) {
	t.Helper()

	project := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(project, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(project, file), []byte(`{"statusLine":{"type":"command","command":"`+command+`"}}`), 0o600))
}

// A role started in a task's worktree runs there, so the project whose status line it would draw is that worktree: it is
// looked up first, then the role's own dir, then the namespace's.
func TestRoleStatusLineLooksInTheWorktreeTheRoleStartsIn(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/zmina"
	f.s.Crew.Namespace = t.TempDir()
	writeSettings(t, f.s.Crew.Namespace, `{"statusLine":{"type":"command","command":"/ns/line.sh"}}`)

	own := f.s.Crew.Roles["designer"].Dir
	wt := t.TempDir()
	started := func() string {
		t.Helper()

		var got statusLineSetting
		require.NoError(t, json.Unmarshal([]byte(f.s.roleSettingsIn("designer", wt)), &got))

		return got.StatusLine["command"].(string)
	}

	assert.Contains(t, started(), "-- '/ns/line.sh'", "no project line anywhere: the namespace's")

	writeProjectLine(t, own, "settings.json", "/own/line.sh")
	assert.Contains(t, started(), "-- '/own/line.sh'", "the worktree has none: the role's dir")

	writeProjectLine(t, wt, "settings.json", "/wt/line.sh")
	assert.Contains(t, started(), "-- '/wt/line.sh'", "the worktree's project wins over the role's dir")

	writeProjectLine(t, wt, "settings.local.json", "/wt/local.sh")
	assert.Contains(t, started(), "-- '/wt/local.sh'", "its local settings win over its project's")

	assert.Contains(t, f.s.roleSettings("designer"), "-- '/own/line.sh'", "without a start dir the role's own")
}

// The start dir reaches the settings through the start itself, not only through the helper.
func TestUpWithDirWrapsTheStatusLineOfThatDir(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/zmina"
	f.s.Crew.Namespace = t.TempDir()

	wt := t.TempDir()
	writeProjectLine(t, wt, "settings.json", "/wt/line.sh")

	require.NoError(t, f.s.UpWith("designer", UpOptions{Dir: wt}))
	assert.Contains(t, strings.Join(f.tm.argv["designer"], " "), "/wt/line.sh")
}
