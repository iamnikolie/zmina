package crew

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, body string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "crew.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))

	return p
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents/designer"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "agents/designer/AGENT.md"), []byte("x"), 0o600))

	crewFile := filepath.Join(root, "crew.yaml")
	require.NoError(t, os.WriteFile(crewFile, []byte("dir: "+root+"\nroles:\n  dispatcher:\n    keep: manual\n  designer:\n  dev:\n    session: zm-dev-x\n"), 0o600))

	c, err := Load(crewFile)
	require.NoError(t, err)

	assert.Equal(t, []string{"dispatcher", "designer", "dev"}, c.Names())
	assert.Equal(t, KeepAlways, c.Roles["dispatcher"].Keep, "the dispatcher is always kept up")
	assert.Equal(t, KeepManual, c.Roles["designer"].Keep)
	assert.Equal(t, "zm-designer", c.Roles["designer"].Session)
	assert.Equal(t, "read agents/designer/AGENT.md and boot", c.Roles["designer"].Boot, "agents/ sits next to the crew file")
	assert.Equal(t, ExpandHome("~/.claude"), c.Namespace)
	assert.Empty(t, c.Roles["dev"].Boot, "no AGENT.md, no boot")
	assert.Equal(t, root, c.Roles["dev"].Dir)
	assert.Equal(t, 90, c.StuckMinutes)

	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/ns-of-caller")

	c, err = Load(crewFile)
	require.NoError(t, err)
	assert.Equal(t, ExpandHome("~/.claude"), c.Namespace, "one default for every caller")
	require.Len(t, c.Warnings, 1)
	assert.Contains(t, c.Warnings[0], "while this session runs in /tmp/ns-of-caller")
}

func TestUnknownKeysAreWarnings(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	dir := t.TempDir()

	c, err := Load(write(t, "dir: "+dir+"\nnamespce: /x\nroles:\n  dispatcher:\n  dev:\n    scheduel: \"0 1 * * *\"\n"))
	require.NoError(t, err)
	require.Len(t, c.Warnings, 2)
	assert.Contains(t, c.Warnings[0], `line 2: unknown key "namespce", ignored`)
	assert.Contains(t, c.Warnings[1], `line 6: unknown key "scheduel", ignored`)

	_, err = Load(write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n    keep: [x]\n"))
	assert.Error(t, err, "a wrong type is still an error")
}

func TestRelativePathsFollowTheCrewFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "work", "api"), 0o700))

	crewFile := filepath.Join(root, "crew.yaml")
	require.NoError(t, os.WriteFile(crewFile, []byte("dir: work\nroles:\n  dispatcher:\n  dev:\n    dir: work/api\n"),
		0o600))

	t.Chdir(t.TempDir())

	c, err := Load(crewFile)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "work"), c.Dir)
	assert.Equal(t, filepath.Join(root, "work", "api"), c.Roles["dev"].Dir)
	assert.Equal(t, filepath.Join(root, "agents"), c.Agents)
	assert.True(t, filepath.IsAbs(c.Path))
}

func TestCrewOfItsOwnDirectoryIsNamed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".zmina", "example")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crew.yaml"), []byte("dir: "+home+"\nroles:\n  dispatcher:\n"),
		0o600))

	c, err := Load(filepath.Join(dir, "crew.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "example", c.Name, "never the default crew's tmux server and ticker")
	assert.Equal(t, "zmina-example", c.Socket())
	assert.Equal(t, "example-dispatcher", c.Roles["dispatcher"].Session)

	require.NoError(t, os.WriteFile(filepath.Join(home, ".zmina", "crew.yaml"), []byte("dir: "+home+"\nroles:\n  dispatcher:\n"),
		0o600))

	c, err = Load(filepath.Join(home, ".zmina", "crew.yaml"))
	require.NoError(t, err)
	assert.Empty(t, c.Name, "the default crew keeps no name")
	assert.Equal(t, "zm-dispatcher", c.Roles["dispatcher"].Session)
}

func TestLoadRefuses(t *testing.T) {
	dir := t.TempDir()

	cases := map[string]string{
		"no dispatcher":  "dir: " + dir + "\nroles:\n  designer:\n",
		"flag boot":      "dir: " + dir + "\nroles:\n  dispatcher:\n    boot: --dangerously\n",
		"shared session": "dir: " + dir + "\nroles:\n  dispatcher:\n  a:\n    session: zm-dispatcher\n",
		"bad keep":       "dir: " + dir + "\nroles:\n  dispatcher:\n  a:\n    keep: sometimes\n",
		"bad name":       "dir: " + dir + "\nroles:\n  dispatcher:\n  Designer:\n",
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, body))
			assert.Error(t, err)
		})
	}
}

func TestMissingDirStopsOnlyThatRole(t *testing.T) {
	dir := t.TempDir()
	path := write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n  stale:\n    dir: "+dir+"/gone\n")

	c, err := Load(path)
	require.NoError(t, err, "one stale seat does not stop the crew")
	assert.NoError(t, c.Roles["dispatcher"].DirErr())
	assert.Error(t, c.Roles["stale"].DirErr())

	_, err = LoadStrict(path)
	assert.Error(t, err)

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Remove("stale"))
	require.NoError(t, e.Save(), "removing the stale seat is allowed")
}

func TestStarterLoads(t *testing.T) {
	agents := t.TempDir()
	for _, r := range []string{"designer", "harvester", "dispatcher"} {
		require.NoError(t, os.MkdirAll(filepath.Join(agents, r), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(agents, r, "AGENT.md"), []byte("x"), 0o600))
	}

	body := strings.Replace(Starter(agents), "dir: "+defaultDir, "dir: "+t.TempDir(), 1)

	c, err := Load(write(t, body))
	require.NoError(t, err)
	assert.Equal(t, []string{"dispatcher", "designer", "harvester"}, c.Names())
}

func TestNamedCrew(t *testing.T) {
	dir := t.TempDir()

	c, err := Load(write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	assert.Equal(t, "zmina", c.Socket())
	assert.Equal(t, "io.iamnikolie.zmina", c.LaunchdLabel())

	c, err = Load(write(t, "name: ops\nprefix: ops-\ndir: "+dir+"\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	assert.Equal(t, "zmina-ops", c.Socket(), "a second crew runs its own tmux server")
	assert.Equal(t, "io.iamnikolie.zmina.ops", c.LaunchdLabel(), "and its own ticker")
	assert.Equal(t, "ops-dispatcher", c.Roles["dispatcher"].Session)

	_, err = Load(write(t, "name: Ops X\ndir: "+dir+"\nroles:\n  dispatcher:\n"))
	assert.Error(t, err)
}

func TestConfigPath(t *testing.T) {
	assert.Equal(t, ExpandHome("~/.zmina/ops/crew.yaml"), ConfigPath("ops"))
	assert.Equal(t, ExpandHome("~/.zmina/ops/crew.yaml"), ConfigPath("../ops"), "a config is a subdirectory, never a path")
}

func TestSeatEditKeepsComments(t *testing.T) {
	dir := t.TempDir()
	path := write(t, "# the crew\ndir: "+dir+"\nroles:\n  dispatcher:   # never sleeps\n    about: routes\n  designer:\n    keep: manual   # manual | always\n")

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Add("dev", map[string]string{"about": "code", "model": "opus", "args": "--chrome,--verbose",
		"expires": "2026-11-01"}))
	require.NoError(t, e.Set("designer", map[string]string{"keep": "always", "model": "sonnet"}))
	require.NoError(t, e.Save())

	raw, _ := os.ReadFile(path)
	body := string(raw)
	assert.Contains(t, body, "# the crew")
	assert.Contains(t, body, "# never sleeps")
	assert.Contains(t, body, "keep: always # manual | always", "the comment on a changed value stays")
	assert.Contains(t, body, "args: [--chrome, --verbose]")

	_, err = os.Stat(path + ".bak")
	assert.NoError(t, err)

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "sonnet", c.Roles["designer"].Model)
	assert.Equal(t, []string{"--chrome", "--verbose"}, c.Roles["dev"].Args)
	assert.False(t, c.Roles["dev"].ExpiresAt().IsZero())

	e, err = Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Set("dev", map[string]string{"model": ""}))
	require.NoError(t, e.Remove("designer"))
	require.NoError(t, e.Save())

	c, err = Load(path)
	require.NoError(t, err)
	assert.Empty(t, c.Roles["dev"].Model)
	assert.NotContains(t, c.Roles, "designer")
}

func TestSeatEditRefusesBrokenCrew(t *testing.T) {
	dir := t.TempDir()
	path := write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n")
	before, _ := os.ReadFile(path)

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Add("bad", map[string]string{"keep": "sometimes"}))
	assert.Error(t, e.Save())

	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after), "a crew that would not load is not written")

	require.Error(t, e.Add("dispatcher", nil), "an existing role is not added twice")
	assert.Error(t, e.Set("ghost", map[string]string{"about": "x"}))
	assert.Error(t, e.Set("dispatcher", map[string]string{"colour": "red"}))
}

func TestScheduleAndExpiresChecked(t *testing.T) {
	dir := t.TempDir()

	for name, body := range map[string]string{
		"bad cron":           "  a:\n    schedule: \"61 * * * *\"\n    prompt: x\n",
		"schedule no prompt": "  a:\n    schedule: \"0 22 * * *\"\n",
		"prompt no schedule": "  a:\n    prompt: x\n",
		"slash prompt":       "  a:\n    schedule: \"0 22 * * *\"\n    prompt: /exit\n",
		"bad expires":        "  a:\n    expires: soon\n",
		"bad resume":         "  a:\n    resume: sometimes\n",
		"bad context_max":    "  a:\n    context_max: lots\n",
		"tiny context_max":   "  a:\n    context_max: 20k\n",
		"bad context_action": "  a:\n    context_max: 150k\n    context_action: restart\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n"+body))
			assert.Error(t, err)
		})
	}

	c, err := Load(write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n  a:\n    schedule: \"0 22 * * *\"\n    prompt: harvest\n    expires: 2026-11-01 18:00\n"))
	require.NoError(t, err)
	assert.NotNil(t, c.Roles["a"].Scheduled())
	assert.Equal(t, 18, c.Roles["a"].ExpiresAt().Hour())
	assert.Equal(t, "a#2", c.Roles["a"].Instance(2).Name)
	assert.Nil(t, c.Roles["a"].Instance(2).Scheduled(), "an instance is never scheduled")
}

func TestContextMax(t *testing.T) {
	for in, want := range map[string]int{"": 0, "150k": 150_000, "1.5m": 1_500_000, "200000": 200_000, "120_000": 120_000, "80K": 80_000, "50k": 50_000} {
		n, err := ParseTokens(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, n, in)
	}

	c, err := Load(write(t, "dir: "+t.TempDir()+"\nroles:\n  dispatcher:\n    context_max: 150k\n  dev:\n    context_max: 1m\n    context_action: clear\n"))
	require.NoError(t, err)
	assert.Equal(t, 150_000, c.Roles["dispatcher"].ContextLimit())
	assert.Equal(t, ContextCompact, c.Roles["dispatcher"].ContextDo())
	assert.Equal(t, ContextClear, c.Roles["dev"].ContextDo())
	assert.Equal(t, 1_000_000, c.Roles["dev"].Instance(2).ContextLimit(), "a copy keeps its role's limit")

	c, err = Load(write(t, "dir: "+t.TempDir()+"\ncontext_max: 400k\ncontext_action: clear\nroles:\n  dispatcher:\n"+
		"    context_max: 150k\n    context_action: compact\n  dev:\n  scribe:\n    context_max: off\n"))
	require.NoError(t, err)
	assert.Equal(t, 150_000, c.Roles["dispatcher"].ContextLimit(), "a role's own value wins")
	assert.Equal(t, ContextCompact, c.Roles["dispatcher"].ContextDo())
	assert.Equal(t, 400_000, c.Roles["dev"].ContextLimit(), "the crew's value")
	assert.Equal(t, ContextClear, c.Roles["dev"].ContextDo())
	assert.Zero(t, c.Roles["scribe"].ContextLimit(), "off")

	for _, body := range []string{"context_max: off\n", "context_max: 5k\n", "context_action: restart\n",
		"context_max: nan\n", "context_max: inf\n"} {
		_, err := Load(write(t, "dir: "+t.TempDir()+"\n"+body+"roles:\n  dispatcher:\n"))
		assert.Error(t, err, body)
	}
}

func TestExampleCrewLoads(t *testing.T) {
	raw, err := os.ReadFile("../../examples/crew.yaml")
	require.NoError(t, err)

	work := t.TempDir()
	body := strings.Replace(string(raw), "dir: ~/work ", "dir: "+work+" ", 1)
	require.NotEqual(t, string(raw), body, "the example's dir line changed; update this test")

	c, err := LoadStrict(write(t, body))
	require.NoError(t, err)
	assert.Equal(t, []string{"dispatcher", "designer", "dev", "reviewer", "scribe"}, c.Names())
	assert.Equal(t, "ex-dev", c.Roles["dev"].Session)
	assert.NotNil(t, c.Roles["scribe"].Scheduled())

	for _, r := range c.Names() {
		_, err := os.Stat(filepath.Join("../../examples/agents", r, "AGENT.md"))
		assert.NoError(t, err, "every example role has a boot document")
	}
}

func TestWatchValidation(t *testing.T) {
	base := "namespace: /ns\nroles:\n  dispatcher:\n    boot: b\nwatch:\n"

	for name, body := range map[string]string{
		"unknown kind":       "  x:\n    kind: rss\n    every: 5m\n",
		"no kind":            "  x:\n    every: 5m\n",
		"bad duration":       "  x:\n    kind: exec\n    every: 5 minutes\n    run: [a]\n",
		"too often":          "  x:\n    kind: exec\n    every: 30s\n    run: [a]\n",
		"exec needs run":     "  x:\n    kind: exec\n    every: 5m\n",
		"gl needs a profile": "  x:\n    kind: gitlab-mr\n    every: 5m\n",
		"bad voices":         "  x:\n    kind: gitlab-mr\n    every: 5m\n    gl_config: w\n    voices: all\n",
		"duplicate name":     "  x:\n    kind: exec\n    every: 5m\n    run: [a]\n  x:\n    kind: exec\n    every: 5m\n    run: [b]\n",
		"bad name":           "  X_1:\n    kind: exec\n    every: 5m\n    run: [a]\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, base+body))
			require.Error(t, err)
		})
	}

	c, err := Load(write(t, base+"  mrs:\n    kind: gitlab-mr\n    every: 10m\n    quiet: 10m\n    forget_after: 14d\n    gl_config: work\n  ci:\n    kind: exec\n    every: 1m\n    run: my-detector --json | tail -5\n"))
	require.NoError(t, err)

	mrs := c.Watches["mrs"]
	assert.Equal(t, "me", mrs.Author)
	assert.Equal(t, VoicesReviewers, mrs.Voices)
	assert.Equal(t, "/agent", mrs.CommandPrefix())
	assert.True(t, mrs.NotesConflicts())
	assert.True(t, mrs.Baselines())
	assert.Equal(t, 14*24*time.Hour, mrs.ForgetAfterD())
	assert.Equal(t, 2*time.Minute, mrs.TimeoutD())
	assert.Equal(t, []string{"/bin/sh", "-c", "my-detector --json | tail -5"}, c.Watches["ci"].Run.Line())
}

func TestDropTools(t *testing.T) {
	dir := t.TempDir()
	path := write(t, "dir: "+dir+"\ndrop_tools: [Artifact, Workflow]\nroles:\n  dispatcher:\n    drop_tools: [Agent, Artifact]\n"+
		"  designer:\n    keep_tools: [Artifact]\n  dev:\n")

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"Artifact", "Workflow", "Agent"}, c.ToolsOff(c.Roles["dispatcher"]), "each once")
	assert.Equal(t, []string{"Workflow"}, c.ToolsOff(c.Roles["designer"]))
	assert.Equal(t, []string{"Artifact", "Workflow"}, c.ToolsOff(c.Roles["dev"]))
	assert.Equal(t, []string{"Workflow"}, c.ToolsOff(c.Roles["designer"].Instance(2)), "a copy keeps its role's tools")

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Set("dev", map[string]string{"drop_tools": "Agent,mcp__claude-in-chrome"}))
	require.NoError(t, e.Save())

	c, err = Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"Artifact", "Workflow", "Agent", "mcp__claude-in-chrome"}, c.ToolsOff(c.Roles["dev"]))

	for name, body := range map[string]string{
		"tool search":   "drop_tools: [ToolSearch]\nroles:\n  dispatcher:\n",
		"comma in name": "drop_tools: [\"Agent,Workflow\"]\nroles:\n  dispatcher:\n",
		"empty name":    "roles:\n  dispatcher:\n    keep_tools: [\"\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, "dir: "+dir+"\n"+body))
			assert.Error(t, err)
		})
	}

	c, err = Load(write(t, "dir: "+dir+"\ndrop_tools: [Artifact]\nroles:\n  dispatcher:\n    args: [--disallowedTools=Agent]\n"))
	require.NoError(t, err, "a crew file from before drop_tools still loads")
	assert.Nil(t, c.ToolsOff(c.Roles["dispatcher"]), "its own flag stands alone")
	assert.Contains(t, strings.Join(c.Warnings, "\n"), "args has --disallowedTools")
}
