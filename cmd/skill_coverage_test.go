package cmd

import (
	_ "embed"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

// skillExempt lists the commands and flags cmd/skill.md need not name, one a line with its reason:
// `zmina a b` or `zmina a b --flag`, then ` # why`.
//
//go:embed skill_exempt.txt
var skillExempt string

// TestSkillNamesEveryCommandAndFlag: cmd/skill.md is how roles learn the CLI, so every command and flag a user can
// type is in it — or in skill_exempt.txt with a reason. A command counts as named by its whole path (`zmina node
// sign`), by its last two words in backticks (`node sign`), or as a word in backticks on a line that names its
// parent (`zmina lesson ls` / `show <id>`); a flag by its long name anywhere (`--full`), a seat flag by its crew-file
// key (`read_guard:`).
func TestSkillNamesEveryCommandAndFlag(t *testing.T) {
	exempt := map[string]bool{}

	for _, line := range strings.Split(skillExempt, "\n") {
		what, why, _ := strings.Cut(line, "#")
		if what = strings.TrimSpace(what); what != "" {
			assert.NotEmpty(t, strings.TrimSpace(why), "%s: an exemption says why", what)
			exempt[what] = true
		}
	}

	lines := strings.Split(skillDoc, "\n")
	used := map[string]bool{}

	named := func(c *cobra.Command) bool {
		path := c.CommandPath()
		if strings.Contains(skillDoc, path) {
			return true
		}

		words := strings.Fields(path)
		if len(words) >= 3 && strings.Contains(skillDoc, "`"+strings.Join(words[len(words)-2:], " ")) {
			return true
		}

		parent := strings.Join(words[:len(words)-1], " ")
		leaf := regexp.MustCompile("(`|\\| )" + regexp.QuoteMeta(c.Name()) + "[` ]")

		for _, l := range lines {
			if strings.Contains(l, "`"+parent+" ") && leaf.MatchString(l) {
				return true
			}
		}

		return false
	}

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range c.Commands() {
			if s.Hidden || s.Name() == "help" || s.Name() == "completion" {
				continue
			}

			path := s.CommandPath()
			if exempt[path] {
				used[path] = true
			} else {
				assert.True(t, named(s), "cmd/skill.md does not name %s (name it, or exempt it in skill_exempt.txt)", path)
			}

			s.LocalFlags().VisitAll(func(f *pflag.Flag) {
				key := path + " --" + f.Name
				switch {
				case f.Hidden:
				case exempt[key]:
					used[key] = true
				case path == "zmina seat add" && strings.Contains(skillDoc, f.Name+":"):
				default:
					assert.Contains(t, skillDoc, "--"+f.Name, "cmd/skill.md does not name %s (name it, or exempt it)", key)
				}
			})

			walk(s)
		}
	}

	walk(NewRoot(io.Discard, io.Discard))

	for what := range exempt {
		assert.True(t, used[what], "skill_exempt.txt: %s is no command or flag any more", what)
	}
}
