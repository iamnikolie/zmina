package crew

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestRuntimeSettings(t *testing.T) {
	p := write(t, "roles:\n  dispatcher: {}\n  dev:\n    runtime: codex\n    model: test-model\n    profile: reviewer\n    budget: 5\n    reasoning_effort: high\n    sandbox: workspace-write\n    approval_policy: on-request\n    codex_home: ./codex-config\n")
	c, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, "claude", c.Roles["dispatcher"].Runtime)
	r := c.Roles["dev"]
	assert.Equal(t, "reviewer", r.Profile)
	assert.Equal(t, float64(5), r.Budget)
	assert.True(t, r.IsCodex())
	assert.True(t, strings.HasSuffix(r.CodexHome, "/codex-config"))
	assert.True(t, r.Instance(2).IsCodex())
	e, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, e.Set("dev", map[string]string{"reasoning_effort": "medium", "sandbox": "read-only"}))
	require.NoError(t, e.Save())
	c, err = Load(p)
	require.NoError(t, err)
	assert.Equal(t, "medium", c.Roles["dev"].ReasoningEffort)
	for _, body := range []string{"runtime: invalid", "runtime: codex\n    codex_transport: invalid", "runtime: codex\n    codex_transport: shared\n    args: [--stdio]", "sandbox: read-only", "runtime: codex\n    sandbox: invalid", "runtime: codex\n    remote_control: true", "runtime: codex\n    drop_tools: [Bash]"} {
		_, err := Load(write(t, "roles:\n  dispatcher: {}\n  dev:\n    "+body+"\n"))
		assert.Error(t, err, body)
	}
}
