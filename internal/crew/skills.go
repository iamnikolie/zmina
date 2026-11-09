package crew

import (
	"fmt"
	"strconv"
	"strings"
)

// Skills trims the skill list a Claude role is sent with every request (skills:, for the crew and per role): the
// list is paid at every start, compaction and cache re-read. Claude Code shortens the descriptions to fit the
// listing's budget; a skill can be listed by name only, or left out.
type Skills struct {
	// Listing is the most characters the list takes ("9k"); Claude Code's own budget is 1% of the context window
	// in characters, often raised by the server. zmina passes it as SLASH_COMMAND_TOOL_CHAR_BUDGET.
	Listing string `yaml:"listing"`
	// DescMax cuts each skill's description at this many characters (Claude Code's default 1536).
	DescMax int `yaml:"desc_max"`
	// Off leaves these skills out of the list; NameOnly lists them without a description. Exact names as the
	// list shows them (kreida, anthropic-skills:docx): Claude Code takes no pattern.
	Off      []string `yaml:"off"`
	NameOnly []string `yaml:"name_only"`
}

// SkillList is a role's skills setting resolved: its own fields over the crew's.
type SkillList struct {
	Listing  int // characters; 0: Claude Code's own budget
	DescMax  int // 0: Claude Code's default
	Off      []string
	NameOnly []string
}

// Empty: nothing to pass, Claude Code's own list.
func (s SkillList) Empty() bool {
	return s.Listing == 0 && s.DescMax == 0 && len(s.Off) == 0 && len(s.NameOnly) == 0
}

// Settings are the Claude Code settings it sets: skillListingMaxDescChars and skillOverrides.
func (s SkillList) Settings() map[string]any {
	out := map[string]any{}
	if s.DescMax > 0 {
		out["skillListingMaxDescChars"] = s.DescMax
	}

	over := map[string]any{}
	for _, n := range s.NameOnly {
		over[n] = "name-only"
	}

	for _, n := range s.Off {
		over[n] = "off"
	}

	if len(over) > 0 {
		out["skillOverrides"] = over
	}

	return out
}

// Env is the environment it sets: the listing's budget in characters.
func (s SkillList) Env() []string {
	if s.Listing == 0 {
		return nil
	}

	return []string{"SLASH_COMMAND_TOOL_CHAR_BUDGET=" + strconv.Itoa(s.Listing)}
}

// listingFloor is the least listing: below it Claude Code lists names only anyway.
const listingFloor = 1000

// resolve is the role's own skills over the crew's, field by field; a list the role sets (off: [] too) replaces
// the crew's.
func resolveSkills(crew, own *Skills) (SkillList, error) {
	var out SkillList

	for _, s := range []*Skills{crew, own} {
		if s == nil {
			continue
		}

		if s.Listing != "" {
			n, err := parseChars(s.Listing)
			if err != nil {
				return SkillList{}, fmt.Errorf("skills.listing %q: %w", s.Listing, err)
			}

			out.Listing = n
		}

		if s.DescMax < 0 {
			return SkillList{}, fmt.Errorf("skills.desc_max %d: want a positive number of characters", s.DescMax)
		}

		if s.DescMax > 0 {
			out.DescMax = s.DescMax
		}

		if s.Off != nil {
			out.Off = s.Off
		}

		if s.NameOnly != nil {
			out.NameOnly = s.NameOnly
		}
	}

	seen := map[string]string{}

	for _, l := range []struct {
		key   string
		names []string
	}{{"off", out.Off}, {"name_only", out.NameOnly}} {
		for _, n := range l.names {
			switch {
			case strings.TrimSpace(n) == "":
				return SkillList{}, fmt.Errorf("skills.%s: an empty name", l.key)
			case strings.ContainsAny(n, "*?"):
				return SkillList{}, fmt.Errorf("skills.%s %q: exact names only, Claude Code takes no pattern", l.key, n)
			case seen[n] != "":
				return SkillList{}, fmt.Errorf("skills: %s is in %s and %s", n, seen[n], l.key)
			}

			seen[n] = l.key
		}
	}

	return out, nil
}

// parseChars reads a character count: 9000, 9k, 9_000.
func parseChars(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))

	mult := 1.0
	if num, ok := strings.CutSuffix(s, "k"); ok {
		s, mult = num, 1e3
	}

	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil || !(f*mult >= listingFloor && f*mult <= 1e6) {
		return 0, fmt.Errorf("a number of characters from %dk to 1000k, such as 9k", listingFloor/1000)
	}

	return int(f * mult), nil
}
