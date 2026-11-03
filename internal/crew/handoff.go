package crew

import (
	"fmt"
	"regexp"
	"strings"
)

// Section is a role-defined part of a handoff, result or checkpoint.
type Section struct {
	Name     string `yaml:"name" json:"name"`
	Guidance string `yaml:"guidance" json:"guidance"`
}

// HandoffShape replaces the crew default when defined on a role.
type HandoffShape struct {
	Sections []Section `yaml:"sections" json:"sections"`
}

// HandoffFor returns an independent shape for a role, including copies.
func (c *Crew) HandoffFor(r *Role) HandoffShape {
	shape := r.Handoff
	if shape == nil {
		shape = c.Handoff
	}
	if shape == nil {
		shape = &HandoffShape{Sections: []Section{
			{"summary", "Brief account of the task and current state."},
			{"done", "What is complete, with evidence where relevant."},
			{"remaining", "What remains or is blocked."},
			{"open_questions", "Unresolved questions and who can answer them."},
			{"artifacts", "Links or paths to the outputs; verify they are accessible."},
			{"next_step", "The next concrete action."},
		}}
	}
	return HandoffShape{Sections: append([]Section(nil), shape.Sections...)}
}

// Request renders role guidance without inventing conclusions.
func (h HandoffShape) Request(kind string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write a %s as a JSON object with these named sections (string values; say unknown when unknown):\n", kind)
	for _, s := range h.Sections {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Guidance)
	}
	return b.String()
}

func (h *HandoffShape) check() error {
	if h == nil {
		return nil
	}
	if len(h.Sections) == 0 || len(h.Sections) > 32 {
		return fmt.Errorf("handoff.sections: use 1-32 named sections")
	}
	seen := map[string]bool{}
	for _, s := range h.Sections {
		if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`).MatchString(s.Name) || seen[s.Name] {
			return fmt.Errorf("handoff.sections: invalid or duplicate name %q", s.Name)
		}
		if len(s.Guidance) > 1000 {
			return fmt.Errorf("handoff section %s: guidance exceeds 1000 bytes", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}
