package shift

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
)

// hooksNone is RoleState.Hooks of a role started with no hooks of zmina's.
const hooksNone = "-"

// startHooks is what a role's start records of its hooks: the zmina they run, hooksNone for none, "" for a role
// whose lines are not on its own channel (a Codex bridge, a typed role), which has no such hooks to miss.
func (s *Shift) startHooks(r *crew.Role) string {
	if r.IsCodex() || !s.inbound(r) {
		return ""
	}

	if s.Self == "" {
		return hooksNone
	}

	return s.Self
}

// hooksNote is the one note for the dispatcher about a role whose session runs without working hooks of zmina's: none
// at all (started by a zmina without them), or of a zmina whose executable is gone. Such a role's lines wait for the
// beat after each turn instead of going a second after it, and nothing is said again after a compaction. A role
// started by an earlier zmina that did not record its hooks has its process asked (ProcArgs). Once per session.
func (s *Shift) hooksNote(name string, r *crew.Role) string {
	rs := s.State.Role(name)
	if !rs.Inbound || r.IsCodex() || s.Self == "" {
		return ""
	}

	sess := s.session(name)
	if sess == nil || rs.HooksFlagged == sess.SessionID {
		return ""
	}

	if rs.Hooks == "" {
		if s.ProcArgs == nil {
			return ""
		}

		args, err := s.ProcArgs(sess.PID)
		if err != nil || args == "" {
			return ""
		}

		rs.Hooks = hooksIn(args)
	}

	problem := ""

	if rs.Hooks == hooksNone {
		problem = "runs without zmina's turn-end hook"
	} else if _, err := os.Stat(rs.Hooks); err != nil {
		problem = "runs the hooks of a zmina that is gone (" + rs.Hooks + ")"
	}

	if problem == "" {
		return ""
	}

	rs.HooksFlagged = sess.SessionID
	s.log(name, "hooks-missing", "", problem)

	return name + ": " + problem + ": its lines wait for the next beat after each turn and are not said again after " +
		"a compaction; restart it (zmina restart " + name + ") to start it with this zmina's hooks"
}

// hooksIn reads the zmina a claude command line's --settings run as its turn-end hook, hooksNone when there is none.
func hooksIn(args string) string {
	i := strings.Index(args, "--settings ")
	if i < 0 {
		return hooksNone
	}

	var settings struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}

	dec := json.NewDecoder(strings.NewReader(args[i+len("--settings "):]))
	if dec.Decode(&settings) != nil {
		return hooksNone
	}

	for _, group := range settings.Hooks.Stop {
		for _, h := range group.Hooks {
			if strings.Contains(h.Command, " _turn-end ") {
				return firstWord(h.Command)
			}
		}
	}

	return hooksNone
}

// firstWord is the first word of a shell command zmina wrote (shellWord quotes it with single quotes).
func firstWord(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if rest, ok := strings.CutPrefix(cmd, "'"); ok {
		var b strings.Builder

		for len(rest) > 0 {
			end := strings.IndexByte(rest, '\'')
			if end < 0 {
				return b.String() + rest
			}

			b.WriteString(rest[:end])
			rest = rest[end+1:]

			// shellWord writes a quote inside a word as '"'"'.
			if q, ok := strings.CutPrefix(rest, `"'"'`); ok {
				b.WriteByte('\'')
				rest = q

				continue
			}

			return b.String()
		}

		return b.String()
	}

	word, _, _ := strings.Cut(cmd, " ")

	return word
}
