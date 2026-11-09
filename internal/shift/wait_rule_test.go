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
