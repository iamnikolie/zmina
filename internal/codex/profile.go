package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pelletier/go-toml/v2"
)

var profileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// LoadProfile reads a modern Codex profile without changing the base config or exposing values in argv.
// App Server has no --profile flag; the returned map is supplied as thread config overrides.
func LoadProfile(home, name string) (map[string]any, error) {
	if name == "" {
		return nil, nil
	}
	if !profileName.MatchString(name) {
		return nil, fmt.Errorf("invalid Codex profile name %q", name)
	}
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".codex")
	}
	path := filepath.Join(home, name+".config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("codex profile %s: %w", name, err)
	}
	var cfg map[string]any
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("codex profile %s: invalid TOML", name)
	}
	if _, old := cfg["profiles"]; old {
		return nil, fmt.Errorf("codex profile %s: use top-level config keys, not [profiles.name]", name)
	}
	return cfg, nil
}

// DefaultServiceTier is the tier a Codex seat runs on unless its role or its profile names one. App Server takes
// "default" as the normal tier; without it the seat would inherit the owner's global service_tier (priority is
// Codex's fast mode, twice the price).
const DefaultServiceTier = "default"

// ServiceTier is the tier a seat's thread is started and resumed with: the role's, else its profile's, else the
// default.
func ServiceTier(role string, profile map[string]any) string {
	if role != "" {
		return role
	}
	if t, ok := profile["service_tier"].(string); ok && t != "" {
		return t
	}
	return DefaultServiceTier
}

func (o Options) threadConfig() map[string]any {
	cfg := make(map[string]any, len(o.Config)+6)
	for k, v := range o.Config {
		cfg[k] = v
	}
	cfg["service_tier"] = ServiceTier(o.ServiceTier, o.Config)
	// Explicit role settings win over the selected profile, also when resuming a thread.
	for _, p := range [][2]string{{"model", o.Model}, {"model_reasoning_effort", o.Effort}, {"sandbox_mode", o.Sandbox}, {"approval_policy", o.Approval}} {
		if p[1] != "" {
			cfg[p[0]] = p[1]
		}
	}
	return cfg
}
