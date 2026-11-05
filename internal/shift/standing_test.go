package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

func TestRetiredCrewTellsEveryRoleAtBoot(t *testing.T) {
	s, _, _, _ := fixture(t)
	s.Crew.Retired, s.Crew.MovedTo = "2026-11-05", "box/ops"

	seat := s.BootText(&crew.Role{Name: "dev", Boot: "read AGENTS.md"})
	assert.Contains(t, seat, "This crew is retired since 2026-11-05: its live crew is box/ops")
	assert.Contains(t, seat, "never report them as the state of the work")

	d := s.Crew.Roles[s.Crew.Dispatcher]
	assert.Contains(t, s.BootText(d), "its live crew is box/ops")

	require.ErrorContains(t, s.Start(), "zmina retire --off")
}

// A ticker that stood still is told to the dispatcher, who reads the ledger first; a crew at work boots as before.
func TestStoodStillCrewTellsItsDispatcher(t *testing.T) {
	s, _, _, now := fixture(t)
	d := s.Crew.Roles[s.Crew.Dispatcher]
	d.Boot = "boot"

	assert.NotContains(t, s.BootText(d), "ago.", "no beat yet: a new crew, nothing to say")

	s.State.LastTick = now.Add(-30 * time.Minute)
	assert.Empty(t, s.Standing())

	s.State.LastTick = now.Add(-5 * 24 * time.Hour)
	s.OtherCrews = func() []string { return []string{"box/ops (running)"} }
	boot := s.BootText(d)
	assert.Contains(t, boot, "This crew's ticker last beat")
	assert.Contains(t, boot, "5d ago")
	assert.Contains(t, boot, "Other crews of this name: box/ops (running); one name does not prove one crew.")

	assert.NotContains(t, s.BootText(&crew.Role{Name: "dev", Boot: "x"}), "last beat", "a seat reports to the dispatcher")
}

// The boot zmina start goes on to still says the shift stood still: the stop it lifted is not forgotten by then.
func TestStartTellsOfTheStopItLifted(t *testing.T) {
	s, _, _, now := fixture(t)
	s.State.LastTick = *now
	s.State.Stopped = now.Add(-49 * time.Hour)

	require.NoError(t, s.Start())
	assert.True(t, s.State.Stopped.IsZero())

	assert.Contains(t, s.Standing(), "This crew's shift was stopped")
	assert.Contains(t, s.Standing(), "2d ago")
}

// The beat that finds the dispatcher down starts it on what it knew before that beat: a ticker back after days has
// just written over when it last beat.
func TestTickTellsTheDispatcherItStartsOfTheSilence(t *testing.T) {
	s, tm, _, now := fixture(t)
	s.State.LastTick = now.Add(-72 * time.Hour)

	s.Tick()

	assert.Contains(t, tm.starts, s.Crew.Dispatcher)
	assert.Contains(t, strings.Join(tm.argv[s.Crew.Dispatcher], " "), "This crew's ticker last beat")
	assert.True(t, s.State.LastTick.Equal(*now))
}

// A beat on time says nothing to whoever it starts.
func TestTickOnTimeAddsNothing(t *testing.T) {
	s, tm, _, now := fixture(t)
	s.State.LastTick = now.Add(-time.Minute)

	s.Tick()

	assert.NotContains(t, strings.Join(tm.argv[s.Crew.Dispatcher], " "), "last beat")
}

func TestRetiredCrewStartsNothing(t *testing.T) {
	s, tm, _, _ := fixture(t)
	s.Crew.MovedTo = "box/ops"

	s.Tick()
	assert.Empty(t, tm.starts)
}
