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

func TestThresholdsAreOnByDefaultForBothPlans(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	want := limits.DefaultThresholds()
	assert.Equal(t, limits.Shares{Warn: 75, Checkpoint: 90, Hold: 97}, want.Claude)
	assert.Equal(t, limits.Shares{Warn: 75, Checkpoint: 90, Hold: 97}, want.Codex)

	for name, body := range map[string]string{
		"no key":         "",
		"routing only":   "limits:\n  claude: {pinned: 70, stop: 95}\n  codex: {pinned: 60}\n",
		"an empty block": "limits: {}\n",
	} {
		c, err := Load(write(t, body+limitsBase))
		require.NoError(t, err, name)
		assert.Equal(t, want, c.LimitsThresholds(), name)
		assert.Empty(t, c.Warnings, name)
	}
}

func TestThresholdsOverrideOneShareAtATime(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	cases := []struct {
		name, yaml string
		want       limits.Thresholds
	}{
		{"every share of both", "limits:\n  claude: {pinned: 80, stop: 99, warn: 60, checkpoint: 80, hold: 95}\n  codex: {pinned: 80, warn: 50, checkpoint: 70, hold: 90}\n",
			limits.Thresholds{Claude: limits.Shares{Warn: 60, Checkpoint: 80, Hold: 95}, Codex: limits.Shares{Warn: 50, Checkpoint: 70, Hold: 90}}},
		{"hold only", "limits:\n  claude: {hold: 100}\n",
			limits.Thresholds{Claude: limits.Shares{Warn: 75, Checkpoint: 90, Hold: 100}, Codex: limits.DefaultShares()}},
		{"codex off", "limits:\n  codex: {warn: off, checkpoint: off, hold: off}\n",
			limits.Thresholds{Claude: limits.DefaultShares(), Codex: limits.Shares{}}},
		{"one off", "limits:\n  claude: {checkpoint: off}\n",
			limits.Thresholds{Claude: limits.Shares{Warn: 75, Hold: 97}, Codex: limits.DefaultShares()}},
		{"off quoted, in any case", "limits:\n  claude: {warn: 'Off', hold: \"off\"}\n",
			limits.Thresholds{Claude: limits.Shares{Checkpoint: 90}, Codex: limits.DefaultShares()}},
		{"warn equal to checkpoint", "limits:\n  claude: {warn: 90}\n",
			limits.Thresholds{Claude: limits.Shares{Warn: 90, Checkpoint: 90, Hold: 97}, Codex: limits.DefaultShares()}},
		{"warn above the default checkpoint, with checkpoint off", "limits:\n  claude: {warn: 95, checkpoint: off}\n",
			limits.Thresholds{Claude: limits.Shares{Warn: 95, Hold: 97}, Codex: limits.DefaultShares()}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Load(write(t, c.yaml+limitsBase))
			require.NoError(t, err)
			assert.Equal(t, c.want, got.LimitsThresholds())
			assert.Equal(t, limits.DefaultPolicy().Claude.Stop, got.LimitsPolicy().Claude.Stop, "routing is untouched")
			assert.Empty(t, got.Warnings)
		})
	}
}

func TestThresholdsAreChecked(t *testing.T) {
	cases := map[string]string{
		"limits:\n  claude: {warn: 0}\n":                        "limits.claude.warn is a share of the window, 1 to 100, or off, not 0",
		"limits:\n  claude: {checkpoint: 101}\n":                "limits.claude.checkpoint is a share of the window, 1 to 100, or off, not 101",
		"limits:\n  codex: {hold: -1}\n":                        "limits.codex.hold is a share of the window, 1 to 100, or off, not -1",
		"limits:\n  claude: {warn: 95, checkpoint: 90}\n":       "limits.claude: warn 95 is above checkpoint 90",
		"limits:\n  claude: {warn: 95}\n":                       "limits.claude: warn 95 is above checkpoint 90",
		"limits:\n  codex: {checkpoint: 98}\n":                  "limits.codex: checkpoint 98 is above hold 97",
		"limits:\n  codex: {warn: 99, checkpoint: off}\n":       "limits.codex: warn 99 is above hold 97",
		"limits:\n  claude: {warn: sometimes}\n":                "a share of the window is a number from 1 to 100, or off",
		"limits:\n  claude: {hold: [97]}\n":                     "a share of the window is a number from 1 to 100, or off",
		"limits:\n  claude: {hold: 97.5}\n":                     "a share of the window is a number from 1 to 100, or off",
		"limits:\n  claude: {pinned: 95, stop: 90, hold: 50}\n": "limits.claude: pinned 95 is above stop 90",
	}

	for in, want := range cases {
		_, err := Load(write(t, in+limitsBase))
		require.Error(t, err, in)
		assert.Contains(t, err.Error(), want, in)
	}
}

func TestThresholdKeysAreNoUnknownKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	c, err := Load(write(t, "limits:\n  claude: {warn: 70, hold_at: 98}\n  codex: {stop: 99}\n"+limitsBase))
	require.NoError(t, err)

	assert.Equal(t, 70, int(c.LimitsThresholds().Claude.Warn))
	require.Len(t, c.Warnings, 2)
	assert.Contains(t, c.Warnings[0]+c.Warnings[1], `unknown key "hold_at"`)
	assert.Contains(t, c.Warnings[0]+c.Warnings[1], `unknown key "stop"`)
}
