package codex

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileConfig(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "review.config.toml"), []byte(`model = "profile-model"
model_reasoning_effort = "medium"
sandbox_mode = "read-only"
[features]
web_search = true
[mcp_servers."example.net"]
command = "example"
args = ["--stdio"]
`), 0600))
	cfg, err := LoadProfile(home, "review")
	require.NoError(t, err)
	assert.Equal(t, "profile-model", cfg["model"])
	o := Options{Config: cfg, Model: "role-model", Effort: "high", Sandbox: "workspace-write", Approval: "on-request"}
	got := o.threadConfig()
	assert.Equal(t, "role-model", got["model"])
	assert.Equal(t, "high", got["model_reasoning_effort"])
	assert.Equal(t, "workspace-write", got["sandbox_mode"])
	assert.Equal(t, "on-request", got["approval_policy"])
	assert.Equal(t, "default", got["service_tier"], "neither role nor profile names a tier: the normal one, never the global one")
	assert.Equal(t, cfg["mcp_servers"], got["mcp_servers"])
	assert.Equal(t, "profile-model", cfg["model"], "overrides cannot mutate the parsed profile")
	for _, name := range []string{"../review", "review.toml", "missing", "/tmp/other"} {
		_, err := LoadProfile(home, name)
		assert.Error(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "bad.config.toml"), []byte(`secret = "invalid`), 0600))
	_, err = LoadProfile(home, "bad")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret")
	cfg, err = LoadProfile(home, "")
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestServiceTier(t *testing.T) {
	assert.Equal(t, "default", Options{}.threadConfig()["service_tier"], "no profile, no role tier")
	fast := map[string]any{"service_tier": "priority"}
	assert.Equal(t, "priority", Options{Config: fast}.threadConfig()["service_tier"], "a profile may opt in")
	assert.Equal(t, "default", Options{Config: fast, ServiceTier: "default"}.threadConfig()["service_tier"], "the role wins over its profile")
	assert.Equal(t, "priority", Options{ServiceTier: "priority"}.threadConfig()["service_tier"])
	assert.Equal(t, "priority", fast["service_tier"], "the parsed profile is not changed")
}
