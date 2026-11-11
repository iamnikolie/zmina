package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
)

// A Tuesday evening on a clock of its own, so no test depends on the machine's zone or the day it runs.
var limitsNow = time.Date(2030, 1, 8, 20, 58, 0, 0, time.FixedZone("cet", 3600))

func at2(t time.Time) *time.Time { return &t }

func TestLimitsTextIsOneLineAPlanAndThePolicy(t *testing.T) {
	r := limits.Report{
		Claude: &limits.Claude{
			FiveHour: &limits.Window{Used: 12, Resets: at2(limitsNow.Add(42 * time.Minute))},
			SevenDay: &limits.Window{Used: 41, Resets: at2(limitsNow.Add(5*24*time.Hour + 16*time.Hour + 17*time.Minute))},
			At:       limitsNow.Add(-2 * time.Minute), Source: "statusline",
		},
		Codex: &limits.Codex{
			Week: &limits.Window{Used: 50, Resets: at2(limitsNow.Add(2*24*time.Hour + 12*time.Hour))},
			Plan: "pro", At: limitsNow.Add(-3 * time.Hour), Source: "rollout codex-dev",
		},
		Policy: limits.DefaultPolicy(),
	}

	var out strings.Builder
	require.NoError(t, writeLimits(&out, r, limitsNow))

	assert.Equal(t, `claude   5h 12%  7d 41%   5h resets 21:40 · 7d resets Mon 13:15   seen 2m ago
codex    week 50%         resets Fri 08:58                        seen 3h ago (codex-dev)
policy   claude: pinned work only from 80%, nothing from 99% · codex: pinned work only from 80%
`, out.String())
}

// A window whose reset passed or whose reading is old is never printed as a figure: unknown, with what it last had.
func TestLimitsTextPrintsAStaleWindowAsUnknown(t *testing.T) {
	r := limits.Report{
		Claude: &limits.Claude{
			FiveHour: &limits.Window{Used: 12, Resets: at2(limitsNow.Add(-time.Hour)), Stale: true},
			SevenDay: &limits.Window{Used: 41, Resets: at2(limitsNow.Add(72 * time.Hour))},
			At:       limitsNow.Add(-2 * time.Hour), Source: "statusline",
		},
		Codex: &limits.Codex{
			Week: &limits.Window{Used: 50, Resets: at2(limitsNow.Add(24 * time.Hour)), Stale: true},
			At:   limitsNow.Add(-30 * time.Hour), Source: "rollout ~/.codex",
		},
		Policy: limits.DefaultPolicy(),
	}

	var out strings.Builder
	require.NoError(t, writeLimits(&out, r, limitsNow))

	lines := strings.Split(out.String(), "\n")
	assert.Contains(t, lines[0], "5h unknown (last 12% at 18:58)")
	assert.Contains(t, lines[0], "7d 41%")
	assert.NotContains(t, lines[0], "5h resets", "the reset of a window that is unknown says nothing")
	assert.Contains(t, lines[0], "7d resets Fri 20:58")
	assert.Contains(t, lines[1], "week unknown (last 50% at Mon 14:58)")
	assert.NotContains(t, lines[1], "resets")
	assert.Contains(t, lines[1], "seen 30h ago (~/.codex)")
}

func TestLimitsTextForPlansNeverSeen(t *testing.T) {
	var out strings.Builder
	require.NoError(t, writeLimits(&out, limits.Report{Policy: limits.DefaultPolicy()}, limitsNow))

	assert.Equal(t, `claude   unknown (no status line has drawn the limits yet)
codex    unknown (no Codex rollout with the limits found)
policy   claude: pinned work only from 80%, nothing from 99% · codex: pinned work only from 80%
`, out.String())
}

func TestLimitsClockNamesTheDayOnlyWhenItIsNotToday(t *testing.T) {
	assert.Equal(t, "21:40", clock(limitsNow.Add(42*time.Minute), limitsNow))
	assert.Equal(t, "09:00", clock(limitsNow.Add(-11*time.Hour-58*time.Minute), limitsNow))
	assert.Equal(t, "Wed 20:58", clock(limitsNow.Add(24*time.Hour), limitsNow))
	assert.Equal(t, "Mon 20:58", clock(limitsNow.Add(-24*time.Hour), limitsNow))
	assert.Equal(t, "15 Jan 20:58", clock(limitsNow.Add(7*24*time.Hour), limitsNow), "a week away is a date, not today's weekday")
	assert.Equal(t, "21:40", clock(limitsNow.UTC().Add(42*time.Minute), limitsNow), "shown on the reader's clock")
}

// limitsRollout writes a Codex rollout line under the sandbox's ~/.codex, as the Codex CLI does.
func limitsRollout(t *testing.T, home string, at time.Time, used float64, resets time.Time) {
	t.Helper()

	dir := filepath.Join(home, "sessions", "2030", "01", "08")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	line := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":`+
		`{"used_percent":%v,"window_minutes":10080,"resets_at":%d},"secondary":null,"plan_type":"pro"}}}`+"\n",
		at.UTC().Format("2006-01-02T15:04:05.000Z"), used, resets.Unix())
	path := filepath.Join(dir, "rollout-2030-01-08T10-00-00-aaa.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))
	require.NoError(t, os.Chtimes(path, at, at))
}

func TestLimitsFlowReadsTheTapFileAndTheCodexRollouts(t *testing.T) {
	crewFile := testCrew(t, "timezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	// Nothing seen yet: both unknown, nothing guessed, exit 0.
	out, _, err := zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Contains(t, out, "claude   unknown")
	assert.Contains(t, out, "codex    unknown")
	assert.Contains(t, out, "policy   claude: pinned work only from 80%, nothing from 99% · codex: pinned work only from 80%")

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)

	var none map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &none))
	assert.Nil(t, none["claude"])
	assert.Nil(t, none["codex"])
	assert.Equal(t, map[string]any{"claude": map[string]any{"pinned": 80.0, "stop": 99.0}, "codex": map[string]any{"pinned": 80.0}}, none["policy"])

	// A status line drew two minutes ago; a Codex session wrote its limits an hour ago.
	now := time.Now()
	resets5, resets7 := now.Add(90*time.Minute), now.Add(3*24*time.Hour)

	_, err = limits.Tap(c.Layout().Limits(), []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":12,"resets_at":%d},`+
		`"seven_day":{"used_percentage":41,"resets_at":%d}}}`, resets5.Unix(), resets7.Unix())), now.Add(-2*time.Minute))
	require.NoError(t, err)
	limitsRollout(t, filepath.Join(os.Getenv("HOME"), ".codex"), now.Add(-time.Hour), 50, now.Add(2*24*time.Hour))

	out, _, err = zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Regexp(t, `claude +5h 12% +7d 41% +5h resets \S+ · 7d resets .* seen 2m ago\n`, out)
	assert.Regexp(t, `codex +week 50% +resets .* seen 1h ago \(~/\.codex\)\n`, out)

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)

	var got struct {
		Claude struct {
			FiveHour struct {
				Used   float64
				Resets string
				Stale  bool
			} `json:"five_hour"`
			SevenDay struct{ Used float64 } `json:"seven_day"`
			At       string
			Source   string
		}
		Codex struct {
			Week struct {
				Used  float64
				Stale bool
			}
			Plan, Source string
		}
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, 12.0, got.Claude.FiveHour.Used)
	assert.False(t, got.Claude.FiveHour.Stale)
	assert.Equal(t, 41.0, got.Claude.SevenDay.Used)
	assert.Equal(t, "statusline", got.Claude.Source)
	assert.Equal(t, 50.0, got.Codex.Week.Used)
	assert.Equal(t, "pro", got.Codex.Plan)
	assert.Equal(t, "rollout ~/.codex", got.Codex.Source)
	assert.True(t, strings.HasSuffix(got.Claude.FiveHour.Resets, "Z"), "times are on the crew's clock (UTC here): %s", got.Claude.FiveHour.Resets)
}

func TestLimitsFlowMarksWhatIsStale(t *testing.T) {
	crewFile := testCrew(t, "timezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Now()

	// Seen eight hours ago: no figure is the plan's present state.
	_, err = limits.Tap(c.Layout().Limits(), []byte(fmt.Sprintf(`{"rate_limits":{"seven_day":{"used_percentage":41,"resets_at":%d}}}`,
		now.Add(72*time.Hour).Unix())), now.Add(-8*time.Hour))
	require.NoError(t, err)

	out, _, err := zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Contains(t, out, "7d unknown (last 41% at ")
	assert.NotContains(t, out, "7d 41%")

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)

	var got struct {
		Claude struct {
			SevenDay struct {
				Used  float64
				Stale bool
			} `json:"seven_day"`
		}
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.True(t, got.Claude.SevenDay.Stale)
	assert.Contains(t, out, `"stale": true`)
	assert.Equal(t, 41.0, got.Claude.SevenDay.Used)
}

func TestLimitsFlowUsesThePolicyOfTheCrewFileAndTheSeatsCodexHomes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex-rev-home")

	// A seat of its own Codex home: its rollouts are read, and the reading carries the seat's name.
	crewFile := testCrew(t, "  codex-rev:\n    runtime: codex\n    codex_home: "+home+"\n"+
		"limits:\n  claude: {pinned: 60, stop: 90}\n  codex: {pinned: 50}\n")

	now := time.Now()
	limitsRollout(t, home, now.Add(-10*time.Minute), 22, now.Add(24*time.Hour))

	out, _, err := zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Contains(t, out, "week 22%")
	assert.Contains(t, out, "(codex-rev)")
	assert.Contains(t, out, "policy   claude: pinned work only from 60%, nothing from 90% · codex: pinned work only from 50%")
}

func TestLimitsIsListedWithTheOwnersCommands(t *testing.T) {
	out, _, err := zm(t, testCrew(t, ""), "--help")
	require.NoError(t, err)

	owner, _, _ := strings.Cut(out, "For the dispatcher:")
	assert.Contains(t, owner, "limits")
}
