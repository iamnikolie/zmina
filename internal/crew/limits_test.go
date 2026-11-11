package crew

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/limits"
)

const limitsBase = "dir: /tmp\nroles:\n  dispatcher:\n"

func TestNoLimitsKeyIsTodaysRules(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	c, err := Load(write(t, limitsBase))
	require.NoError(t, err)

	assert.Nil(t, c.Limits)
	assert.Equal(t, limits.DefaultPolicy(), c.LimitsPolicy())
	assert.Equal(t, limits.Policy{Claude: limits.ClaudePolicy{Pinned: 80, Stop: 99}, Codex: limits.CodexPolicy{Pinned: 80}}, c.LimitsPolicy())
	assert.Empty(t, c.Warnings)
}

func TestLimitsKeyOverridesOneShareAtATime(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	cases := []struct {
		name, yaml string
		want       limits.Policy
	}{
		{"all four", "limits:\n  claude: {pinned: 60, stop: 90}\n  codex: {pinned: 50}\n",
			limits.Policy{Claude: limits.ClaudePolicy{Pinned: 60, Stop: 90}, Codex: limits.CodexPolicy{Pinned: 50}}},
		{"claude pinned only", "limits:\n  claude: {pinned: 70}\n",
			limits.Policy{Claude: limits.ClaudePolicy{Pinned: 70, Stop: 99}, Codex: limits.CodexPolicy{Pinned: 80}}},
		{"codex only", "limits:\n  codex:\n    pinned: 95\n",
			limits.Policy{Claude: limits.ClaudePolicy{Pinned: 80, Stop: 99}, Codex: limits.CodexPolicy{Pinned: 95}}},
		{"an empty block", "limits: {}\n", limits.DefaultPolicy()},
		{"empty plans", "limits:\n  claude:\n  codex:\n", limits.DefaultPolicy()},
		{"pinned equal to stop", "limits:\n  claude: {pinned: 99}\n",
			limits.Policy{Claude: limits.ClaudePolicy{Pinned: 99, Stop: 99}, Codex: limits.CodexPolicy{Pinned: 80}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Load(write(t, c.yaml+limitsBase))
			require.NoError(t, err)
			assert.Equal(t, c.want, got.LimitsPolicy())
			assert.Empty(t, got.Warnings)
		})
	}
}

func TestLimitsKeyIsChecked(t *testing.T) {
	cases := map[string]string{
		"limits:\n  claude: {pinned: 0}\n":            "limits.claude.pinned is a share of the window, 1 to 100, not 0",
		"limits:\n  claude: {stop: 101}\n":            "limits.claude.stop is a share of the window, 1 to 100, not 101",
		"limits:\n  claude: {pinned: -5}\n":           "limits.claude.pinned",
		"limits:\n  codex: {pinned: 150}\n":           "limits.codex.pinned is a share of the window, 1 to 100, not 150",
		"limits:\n  claude: {pinned: 95, stop: 90}\n": "limits.claude: pinned 95 is above stop 90",
		"limits:\n  claude: {pinned: 100}\n":          "limits.claude: pinned 100 is above stop 99",
		"limits:\n  claude: {stop: 50}\n":             "limits.claude: pinned 80 is above stop 50",
		"limits:\n  claude: {pinned: eighty}\n":       "cannot unmarshal !!str `eighty` into int",
		"limits:\n  claude: [pinned, 80]\n":           "ClaudeLimits",
	}

	for in, want := range cases {
		_, err := Load(write(t, in+limitsBase))
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), want, in)
	}
}

func TestLimitsKeyWarnsOnWhatItDoesNotKnow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	c, err := Load(write(t, "limits:\n  codex: {pinned: 70, stop: 99}\n  gemini: {pinned: 10}\n"+limitsBase))
	require.NoError(t, err, "a typo is a warning, as everywhere in the crew file")

	assert.Equal(t, 70, c.LimitsPolicy().Codex.Pinned)
	require.Len(t, c.Warnings, 2)
	assert.Contains(t, c.Warnings[0]+c.Warnings[1], `unknown key "stop"`, "Codex has no stop")
	assert.Contains(t, c.Warnings[0]+c.Warnings[1], `unknown key "gemini"`)
}

func TestLimitsLayoutIsNextToTheCrewFile(t *testing.T) {
	c, err := Load(write(t, limitsBase))
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(filepath.Dir(c.Path), "limits"), c.Layout().Limits())
}

func TestCodexHomesAreTheDefaultAndEachSeats(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	body := "dir: /tmp\nroles:\n  dispatcher:\n  designer:\n" +
		"  codex-dev:\n    runtime: codex\n" +
		"  codex-rev:\n    runtime: codex\n    codex_home: ~/.codex-rev\n" +
		"  codex-two:\n    runtime: codex\n    codex_home: ~/.codex-rev\n"

	c, err := Load(write(t, body))
	require.NoError(t, err)

	home := ExpandHome("~")
	assert.Equal(t, []limits.Home{
		{Path: filepath.Join(home, ".codex"), Label: "codex-dev"},
		{Path: filepath.Join(home, ".codex-rev"), Label: "codex-rev, codex-two"},
	}, c.CodexHomes())

	none, err := Load(write(t, limitsBase+"  designer:\n"))
	require.NoError(t, err)
	assert.Equal(t, []limits.Home{{Path: filepath.Join(home, ".codex"), Label: "~/.codex"}}, none.CodexHomes(),
		"no Codex seat: the default home is still looked at, under its own name")
}
