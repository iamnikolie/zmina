package crew

import (
	"fmt"
	"regexp"
	"slices"
)

var codexProfileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// IsCodex reports whether this seat uses the Codex adapter.
func (r *Role) IsCodex() bool { return r.Runtime == "codex" }

// CheckRuntime rejects settings that would otherwise be silently ignored.
func (r *Role) CheckRuntime() error {
	if !slices.Contains([]string{"", "claude", "codex"}, r.Runtime) {
		return fmt.Errorf("runtime is claude or codex, not %q", r.Runtime)
	}
	if !r.IsCodex() {
		if r.CodexTransport+r.Profile+r.CodexHome+r.ReasoningEffort+r.Sandbox+r.ApprovalPolicy+r.ServiceTier != "" {
			return fmt.Errorf("codex_transport, profile, codex_home, reasoning_effort, sandbox, approval_policy and service_tier require runtime: codex")
		}
		return nil
	}

	if !slices.Contains([]string{"", "stdio", "shared"}, r.CodexTransport) {
		return fmt.Errorf("codex_transport is stdio or shared, not %q", r.CodexTransport)
	}
	if r.CodexTransport == "shared" && len(r.Args) > 0 {
		return fmt.Errorf("shared Codex transport does not accept App Server process args; use a profile")
	}
	if r.Profile != "" && !codexProfileName.MatchString(r.Profile) {
		return fmt.Errorf("invalid Codex profile name %q", r.Profile)
	}
	for key, pair := range map[string]struct {
		value   string
		allowed []string
	}{
		"sandbox":         {r.Sandbox, []string{"", "read-only", "workspace-write", "danger-full-access"}},
		"approval_policy": {r.ApprovalPolicy, []string{"", "on-request", "never", "untrusted", "on-failure"}},
		"service_tier":    {r.ServiceTier, []string{"", "default", "priority"}},
	} {
		if !slices.Contains(pair.allowed, pair.value) {
			return fmt.Errorf("invalid %s %q", key, pair.value)
		}
	}
	if len(r.DropTools)+len(r.KeepTools) > 0 || (r.RemoteControl != nil && *r.RemoteControl) {
		return fmt.Errorf("drop_tools, keep_tools and remote_control: true are Claude-only settings")
	}
	return nil
}
