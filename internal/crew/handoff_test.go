package crew

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestRoleShapedHandoffDefaultsOverridesAndCopies(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	c, err := Load(write(t, `dir: /tmp
handoff:
  sections:
    - name: summary
      guidance: Crew summary
roles:
  dispatcher: {}
  designer:
    handoff:
      sections:
        - name: decisions
          guidance: Explain the chosen direction
  support: {}
`))
	require.NoError(t, err)
	assert.Empty(t, c.Warnings)
	assert.Equal(t, "decisions", c.HandoffFor(c.Roles["designer"].Instance(2)).Sections[0].Name)
	assert.Equal(t, "Crew summary", c.HandoffFor(c.Roles["support"]).Sections[0].Guidance)
	assert.Contains(t, c.HandoffFor(c.Roles["designer"]).Request("result"), "decisions: Explain")
	for _, v := range []string{"sections: []", "sections: [{name: bad.name}]", "sections: [{name: summary}, {name: summary}]"} {
		_, err := Load(write(t, "roles:\n  dispatcher: {}\n  designer:\n    handoff: {"+v+"}\n"))
		require.Error(t, err)
	}
	c.Handoff = nil
	shape := c.HandoffFor(c.Roles["support"])
	require.Len(t, shape.Sections, 6)
	assert.NotContains(t, strings.ToLower(shape.Request("result")), "commit")
}
