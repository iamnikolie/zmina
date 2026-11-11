package limits

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var noon = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

func at(t time.Time) *time.Time { return &t }

func TestAWindowIsStaleOnceItsResetPassedOrTheReadingIsOld(t *testing.T) {
	cases := []struct {
		name   string
		seen   time.Duration // how long before noon the reading was taken
		resets *time.Time
		stale  bool
	}{
		{"fresh, reset ahead", 2 * time.Minute, at(noon.Add(3 * time.Hour)), false},
		{"reset passed since the reading", time.Hour, at(noon.Add(-time.Minute)), true},
		{"reset exactly now", time.Hour, at(noon), true},
		{"older than six hours", 6*time.Hour + time.Second, at(noon.Add(24 * time.Hour)), true},
		{"six hours is not older", 6 * time.Hour, at(noon.Add(24 * time.Hour)), false},
		{"no reset given, fresh", time.Hour, nil, false},
		{"no reset given, old", 7 * time.Hour, nil, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := &Claude{SevenDay: &Window{Used: 41, Resets: c.resets}, At: noon.Add(-c.seen)}
			cl.Judge(noon)
			assert.Equal(t, c.stale, cl.SevenDay.Stale)

			cx := &Codex{Week: &Window{Used: 41, Resets: c.resets}, At: noon.Add(-c.seen)}
			cx.Judge(noon)
			assert.Equal(t, c.stale, cx.Week.Stale)
		})
	}
}

func TestEachClaudeWindowIsJudgedByItsOwnReset(t *testing.T) {
	c := &Claude{
		FiveHour: &Window{Used: 90, Resets: at(noon.Add(-time.Minute))},
		SevenDay: &Window{Used: 41, Resets: at(noon.Add(72 * time.Hour))},
		At:       noon.Add(-time.Hour),
	}
	c.Judge(noon)

	assert.True(t, c.FiveHour.Stale, "the 5-hour window rolled over")
	assert.False(t, c.SevenDay.Stale, "the week did not")
	assert.Equal(t, 90.0, c.FiveHour.Used, "the figure it last had is kept, not guessed over")
}

func TestPolicyDefaultsAreTodaysRules(t *testing.T) {
	p := DefaultPolicy()
	assert.Equal(t, ClaudePolicy{Pinned: 80, Stop: 99}, p.Claude)
	assert.Equal(t, CodexPolicy{Pinned: 80}, p.Codex)
	assert.Equal(t, "claude: pinned work only from 80%, nothing from 99% · codex: pinned work only from 80%", p.String())
}

func TestPolicyBands(t *testing.T) {
	p := DefaultPolicy()

	for used, want := range map[float64]Band{0: Open, 41: Open, 79.9: Open, 80: PinnedOnly, 98.9: PinnedOnly, 99: Stopped, 100: Stopped} {
		assert.Equal(t, want, p.Claude.Band(used), "claude at %v%%", used)
	}

	for used, want := range map[float64]Band{0: Open, 79: Open, 80: PinnedOnly, 99: PinnedOnly, 100: PinnedOnly} {
		assert.Equal(t, want, p.Codex.Band(used), "codex at %v%%: no stop for Codex", used)
	}

	custom := Policy{Claude: ClaudePolicy{Pinned: 60, Stop: 90}, Codex: CodexPolicy{Pinned: 50}}
	assert.Equal(t, PinnedOnly, custom.Claude.Band(60))
	assert.Equal(t, Stopped, custom.Claude.Band(90))
	assert.Equal(t, PinnedOnly, custom.Codex.Band(50))
}

func TestCollectJudgesBothPlansAndKeepsTheOneThatIsMissing(t *testing.T) {
	dir := t.TempDir()

	r, err := Collect(dir, nil, DefaultPolicy(), noon)
	require.NoError(t, err)
	assert.Nil(t, r.Claude, "nothing seen yet: unknown, not zero")
	assert.Nil(t, r.Codex)
	assert.Equal(t, DefaultPolicy(), r.Policy)

	_, err = Tap(dir, []byte(`{"rate_limits":{"seven_day":{"used_percentage":41,"resets_at":1894000000}}}`), noon.Add(-8*time.Hour))
	require.NoError(t, err)

	r, err = Collect(dir, nil, DefaultPolicy(), noon)
	require.NoError(t, err)
	require.NotNil(t, r.Claude)
	assert.True(t, r.Claude.SevenDay.Stale, "seen eight hours ago")
	assert.Equal(t, 41.0, r.Claude.SevenDay.Used)
}

func TestReportShowsTimesOnTheCallersClock(t *testing.T) {
	zone := time.FixedZone("test", 2*3600)
	r := Report{Claude: &Claude{SevenDay: &Window{Used: 1, Resets: at(noon)}, At: noon}}

	got := r.In(zone)
	_, off := got.Claude.SevenDay.Resets.Zone()
	assert.Equal(t, 2*3600, off)
	_, off = got.Claude.At.Zone()
	assert.Equal(t, 2*3600, off)
	assert.Equal(t, time.UTC, r.Claude.At.Location(), "the original is untouched")
}
