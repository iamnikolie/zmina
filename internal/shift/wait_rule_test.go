package shift

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every role hears how to wait: a Claude role in the background, a Codex role inside one command.
func TestBootCarriesTheWaitRule(t *testing.T) {
	s, _, _, _ := fixture(t)
	r, err := s.Role("designer")
	require.NoError(t, err)
	r.Boot = "read AGENTS.md"

	claude := s.BootText(r)
	assert.Contains(t, claude, WaitRule)
	assert.Contains(t, claude, "run_in_background")
	assert.NotContains(t, claude, CodexWaitRule)
	assert.Contains(t, s.BootText(s.Crew.Roles["dispatcher"]), WaitRule, "the dispatcher too")

	r.Runtime = "codex"

	codex := s.BootText(r)
	assert.Contains(t, codex, CodexWaitRule)
	assert.NotContains(t, codex, "run_in_background", "no such tool in Codex")
	assert.Contains(t, codex, OwnerRules+"\n\n"+CodexWaitRule)
}

// A crew with decide on tells every role, Claude, Codex and the dispatcher, to take its own recommendation and record it for
// the owner's review; a crew without it says nothing of deciding, and its boot text is what it was.
func TestBootCarriesTheDecideRuleWhenOn(t *testing.T) {
	s, _, _, _ := fixture(t)
	r, err := s.Role("designer")
	require.NoError(t, err)
	r.Boot = "read AGENTS.md"

	before := s.BootText(r)
	assert.NotContains(t, before, "zmina todo decided")
	assert.NotContains(t, s.BootText(s.Crew.Roles["dispatcher"]), DecideRule)
	assert.Equal(t, "read AGENTS.md\n\n"+RoleRules+"\n\n"+BellRule, before, "off: as it was")

	on := true
	s.Crew.Decide = &on

	claude := s.BootText(r)
	assert.Contains(t, claude, RoleRules+"\n\n"+DecideRule)
	assert.Contains(t, claude, `zmina todo decided "<what you decided>" --reason "<why>"`)
	assert.Contains(t, s.BootText(s.Crew.Roles["dispatcher"]), DecideRule, "the dispatcher too")
	assert.Greater(t, s.RulesSize(r), len(DecideRule), "its size is counted in the rules a fresh start carries")

	r.Runtime = "codex"
	assert.Contains(t, s.BootText(r), CodexWaitRule+"\n\n"+CrossingRule+"\n\n"+DecideRule)

	off := false
	s.Crew.Decide = &off
	assert.NotContains(t, s.BootText(r), DecideRule)
}

// A crew with lessons on tells its roles where a lasting rule goes; one without says nothing of lessons.
func TestBootCarriesTheLessonRuleWhenOn(t *testing.T) {
	s, _, _, _ := fixture(t)
	r, err := s.Role("designer")
	require.NoError(t, err)
	r.Boot = "read AGENTS.md"

	assert.NotContains(t, s.BootText(r), "zmina lesson add")

	s.Crew.Lessons.On = true
	assert.Contains(t, s.BootText(r), LessonRule)
}
