package lesson

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// prompt is the sorting seat's whole brief: the lessons, the files it may aim them at, the verdicts and the rules of
// an edit. The seat only reads the repository (its working directory) and answers with the plan.
func prompt(cfg Config, lessons []Lesson, allowed []string) string {
	type in struct {
		ID      string `json:"id"`
		Rule    string `json:"rule"`
		Why     string `json:"why"`
		Applies string `json:"applies_to"`
		InFile  string `json:"in_file,omitempty"`
		Role    string `json:"role,omitempty"`
		Task    string `json:"task,omitempty"`
	}

	items := make([]in, 0, len(lessons))
	for _, l := range lessons {
		items = append(items, in{l.ID, l.Rule, l.Why, string(l.Applies), l.InFile, l.Role, l.Task})
	}

	raw, _ := json.MarshalIndent(items, "", "  ")

	rel, err := filepath.Rel(cfg.Repo, cfg.Agents)
	if err != nil {
		rel = cfg.Agents
	}

	var roles []string
	for _, r := range cfg.Roles {
		roles = append(roles, fmt.Sprintf("%s -> %s/%s/AGENT.md", r, filepath.ToSlash(rel), r))
	}

	return strings.Join([]string{
		"You sort lessons that the roles of an agent crew learned into its instruction files. The working directory is",
		"the crew's instruction repository at the branch the edits go to. Read the files you need (Read, Grep, Glob);",
		"you cannot change anything: you answer with a plan, and a program applies it after its own checks, one commit",
		"per lesson, as a draft merge request the owner reviews. Nothing applies until he merges it.",
		"",
		"Give every lesson exactly one verdict:",
		"- every: a rule for every seat -> the repository's CLAUDE.md (or AGENTS.md where that is the file in use).",
		"- role: a rule for one role -> that role's AGENT.md (roles: " + strings.Join(roles, "; ") + ").",
		"- skill: belongs in a skill -> that skill's SKILL.md.",
		"- written: the files already say it (reason: quote the file and the words).",
		"- stale: a one-off, or no longer true.",
		"- drop: not a rule for these files, or about models, budgets, permissions, hooks, credentials or security",
		"  settings (those change by the owner's hand only), or carries anything secret or about a customer.",
		"- conflict: it goes against an instruction or a recorded decision: give the owner a question; never an edit.",
		"",
		"For every, role and skill give the edit: file (a path relative to the repository, one of: " +
			strings.Join(allowed, ", ") + "), old (text copied exactly from the file that occurs in it once; empty",
		"to append at the end) and new (what replaces it). Keep the edit as small as it can be: one bullet or",
		"sentence in the file's own words, style and section; never rewrite around it; at most 40 lines. Merge a",
		"lesson into an existing rule when one is close rather than adding a near-duplicate. Two lessons that say the",
		"same: one edit, the other written. When applies_to says a role but the rule holds for every seat, or the",
		"other way round, follow the rule's content and say why in reason. Write reason in one sentence.",
		"Put questions you have for the owner beyond the conflicts in questions.",
		"",
		"Lessons:",
		string(raw),
	}, "\n")
}

// checkPrompt is the checker's brief: each proposed edit, the lessons, and what to look for. It reads the files
// itself and does not see the sorting seat's reasons, so its reading is its own.
func checkPrompt(cfg Config, lessons []Lesson, edits []Item) string {
	type edit struct {
		ID   string `json:"id"`
		Rule string `json:"rule"`
		File string `json:"file"`
		Old  string `json:"old"`
		New  string `json:"new"`
	}

	rule := map[string]string{}
	for _, l := range lessons {
		rule[l.ID] = l.Rule
	}

	items := make([]edit, 0, len(edits))
	for _, e := range edits {
		items = append(items, edit{e.ID, rule[Normalize(e.ID)], e.File, e.Old, e.New})
	}

	raw, _ := json.MarshalIndent(items, "", "  ")

	var all []string
	for _, l := range lessons {
		all = append(all, l.ID+": "+l.Rule)
	}

	return strings.Join([]string{
		"You check proposed edits to an agent crew's instruction files before they are offered to the owner. The",
		"working directory is the instruction repository; read the files (Read, Grep, Glob); you change nothing.",
		"For each edit answer ok true only when all hold: the file is the right place for the rule (a rule for every",
		"seat in CLAUDE.md/AGENTS.md, one role's in its AGENT.md, a skill's in its SKILL.md); the file (any file of",
		"the repository) does not already say it, and no other edit below says the same (else problem duplicate);",
		"it does not go against anything the files say or a decision they record (else problem conflict); the new",
		"text is clear on its own (else problem unclear). Give the reason in one sentence; on a problem, what the",
		"owner should decide.",
		"",
		"All lessons of this run: " + strings.Join(all, "; "),
		"",
		"Proposed edits:",
		string(raw),
	}, "\n")
}
