package crew

import (
	"fmt"
	"strconv"
	"strings"
)

// OutputCap caps what one tool result puts into a Claude role's context (output_cap:, for the crew and per role):
// a result is paid again on every later turn until a compaction. Past the cap Claude Code saves a Bash output to a
// file and gives a preview with its path, and refuses a whole Read with a hint to read a part.
type OutputCap struct {
	// Bash is the most characters of a Bash command's output given inline ("12k"); Claude Code's own default is 30k.
	// zmina passes it as the setting bashOutputMaxChars (4k to 128k).
	Bash string `yaml:"bash"`
	// ReadTokens is the most tokens one Read returns; zmina passes it as CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS.
	ReadTokens string `yaml:"read_tokens"`
	// MCPTokens is the most tokens one MCP tool result takes; zmina passes it as MAX_MCP_OUTPUT_TOKENS.
	MCPTokens string `yaml:"mcp_tokens"`
}

// OutputCaps is a role's output_cap resolved: its own fields over the crew's; 0 leaves Claude Code's own.
type OutputCaps struct {
	Bash, ReadTokens, MCPTokens int
}

// Settings are the Claude Code settings it sets: bashOutputMaxChars.
func (o OutputCaps) Settings() map[string]any {
	if o.Bash == 0 {
		return nil
	}

	return map[string]any{"bashOutputMaxChars": o.Bash}
}

// Env is the environment it sets.
func (o OutputCaps) Env() []string {
	var env []string
	if o.ReadTokens > 0 {
		env = append(env, "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS="+strconv.Itoa(o.ReadTokens))
	}

	if o.MCPTokens > 0 {
		env = append(env, "MAX_MCP_OUTPUT_TOKENS="+strconv.Itoa(o.MCPTokens))
	}

	return env
}

// resolveOutputCap is the role's own output_cap over the crew's, field by field; off drops the crew's value.
func resolveOutputCap(crew, own *OutputCap) (OutputCaps, error) {
	var out OutputCaps

	for _, o := range []*OutputCap{crew, own} {
		if o == nil {
			continue
		}

		for _, f := range []struct {
			key, value string
			min, max   int
			to         *int
		}{
			{"bash", o.Bash, 4000, 128000, &out.Bash},
			{"read_tokens", o.ReadTokens, 1000, 200000, &out.ReadTokens},
			{"mcp_tokens", o.MCPTokens, 1000, 200000, &out.MCPTokens},
		} {
			if f.value == "" {
				continue
			}

			n, err := parseCap(f.value, f.min, f.max)
			if err != nil {
				return OutputCaps{}, fmt.Errorf("output_cap.%s %q: %w", f.key, f.value, err)
			}

			*f.to = n
		}
	}

	return out, nil
}

// parseCap reads a cap: 12000, 12k, 12_000, or off (0: Claude Code's own).
func parseCap(s string, lo, hi int) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "off" {
		return 0, nil
	}

	mult := 1.0
	if num, ok := strings.CutSuffix(s, "k"); ok {
		s, mult = num, 1e3
	}

	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil || !(f*mult >= float64(lo) && f*mult <= float64(hi)) {
		return 0, fmt.Errorf("a number from %dk to %dk, such as 12k, or off", lo/1000, hi/1000)
	}

	return int(f * mult), nil
}
