package shift

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	// blockedAfter is how long a role's screen may show no input line before the dispatcher hears of it: a
	// dialog (trust this folder, a permission), a login, a screen claude could not draw.
	blockedAfter = 3 * time.Minute
	// confirmAfter is how long a typed line has to show that it landed — the role's status changes when it takes
	// a line — before it is pressed again or typed again.
	confirmAfter = 90 * time.Second
	// excerptMax caps the screen lines a note quotes.
	excerptMax = 240
)

// limitRe finds Claude Code telling that a usage limit stops the role.
var limitRe = regexp.MustCompile(`(?i)(usage limit reached|usage credit limit|hit your (\w+ )?limit|` +
	`limit (reached|will reset)|out of extra usage|/rate-limit-options)`)

// watchScreen looks at an up role's screen and the end of its transcript: a screen with no input line for
// blockedAfter, a last turn that ended in an API error, a usage limit on screen. Each is told once per stretch.
func (s *Shift) watchScreen(name string, now time.Time) []string {
	rs := s.State.Role(name)

	_, ok, err := s.Tmux.Prompt(name)
	if err == nil && !ok && s.trust(name) {
		// Answered at once: the boot goes on.
		return nil
	}

	// What the role says it is doing, also while it boots.
	if text, err := s.Tmux.Capture(name, nowLines); err == nil {
		if line := NowLine(text); line != rs.Now {
			rs.Now, rs.NowAt = line, now
		}
	}

	if now.Sub(rs.Started) < s.Crew.BootGraceD() {
		return nil
	}

	var notes []string

	switch {
	case err != nil:
	case ok:
		rs.NoPrompt, rs.BlockedNoted = time.Time{}, false
	case rs.NoPrompt.IsZero():
		rs.NoPrompt = now
	case !rs.BlockedNoted && now.Sub(rs.NoPrompt) >= blockedAfter:
		rs.BlockedNoted = true
		note := fmt.Sprintf("%s: no input line on its screen for %d min, waiting on: %s (zmina peek %s; "+
			"zmina keys %s ...)", name, int(now.Sub(rs.NoPrompt).Minutes()), s.excerpt(name), name, name)
		s.log(name, "blocked", "", note)
		notes = append(notes, note)
	}

	if sess := s.session(name); sess != nil {
		if turn, err := s.Reg.Last(sess.SessionID); err == nil && turn.Error != "" && turn.ErrorID != rs.ErrorNoted {
			rs.ErrorNoted = turn.ErrorID
			note := name + ": its last turn ended in an API error: " + clip(turn.Error)
			s.log(name, "api-error", "", note)
			notes = append(notes, note)
		}
	}

	// Claude Code shows a limit as a notice at the bottom of the screen; a reply that talks about limits is
	// further up or sits after a ❯ prompt.
	if text, err := s.Tmux.Capture(name, 8); err == nil {
		limit := ""
		for _, l := range strings.Split(text, "\n") {
			if l = strings.TrimSpace(l); limitRe.MatchString(l) && !strings.HasPrefix(l, "❯") {
				limit = clip(l)
			}
		}

		if limit != "" && limit != rs.LimitNoted {
			note := name + ": stopped by a usage limit: " + limit
			s.log(name, "limit", "", note)
			notes = append(notes, note)
		}

		rs.LimitNoted = limit
	}

	return notes
}

// nowLines is how much of the screen NowLine reads.
const nowLines = 80

var (
	// toolRe matches a ⏺ line that is a tool call rather than words: Bash(…), Update(…), mcp__x__y (MCP)(…),
	// "Read 3 files (ctrl+o to expand)", "Searched for 2 patterns", a subagent's "main ◯ …" line, "Stop Task".
	toolRe = regexp.MustCompile(`^([\w.:-]+\(|.*\(MCP\)|.*ctrl\+o to expand|(Read|Reading|Searched|Searching|Listed|` +
		`Wrote|Writing|Fetched|Ran|Running|Called|Found|Updated|Updating|Making|Editing|Edited) .*(\d|…)|.*◯|` +
		`.*↑ [\d.]+k? tokens|(Stop Task|Task Output)\b|Agent ")`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// NowLine is the last thing the role said in words on its screen: the last "⏺ …" line that is not a tool call,
// with the lines it wraps onto, on one line and clipped. Empty when the screen has none.
func NowLine(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "⏺")
		rest = strings.TrimSpace(rest)

		if !ok || rest == "" || toolRe.MatchString(rest) {
			continue
		}

		// A long line wraps onto lines indented under it.
		for _, l := range lines[i+1:] {
			if !strings.HasPrefix(l, "  ") || strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "⎿") {
				break
			}

			rest += " " + strings.TrimSpace(l)
		}

		return clip(spaceRe.ReplaceAllString(rest, " "))
	}

	return ""
}

// trust answers Claude Code's "trust this folder" for a role zmina started in a task worktree it made itself
// (zmina up --task), once per start: zmina created that checkout from the owner's repository for this role. Any
// other folder's question is left to a person. It reports whether it answered.
func (s *Shift) trust(name string) bool {
	rs := s.State.Role(name)
	wt := s.Crew.Layout().Worktrees()

	if rs.Trusted || rs.Dir == "" || !strings.HasPrefix(filepath.Clean(rs.Dir), filepath.Clean(wt)+"/") {
		return false
	}

	text, err := s.Tmux.Capture(name, 30)
	if err != nil || !strings.Contains(text, "trust") {
		return false
	}

	var opts []string

	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); trustOpt.MatchString(l) {
			opts = append(opts, l)
		}
	}

	sel, yes := -1, -1

	for i, o := range opts {
		if strings.HasPrefix(o, "❯") {
			sel = i
		}

		if strings.Contains(strings.ToLower(o), "yes") {
			yes = i
		}
	}

	if sel < 0 || yes < 0 {
		return false
	}

	var keys []string

	for i := sel; i < yes; i++ {
		keys = append(keys, "Down")
	}

	for i := sel; i > yes; i-- {
		keys = append(keys, "Up")
	}

	if err := s.Tmux.Keys(name, append(keys, "Enter")...); err != nil {
		return false
	}

	rs.Trusted = true
	s.log(name, "trusted", "", rs.Dir)

	return true
}

// trustOpt matches the answers of Claude Code's folder-trust question.
var trustOpt = regexp.MustCompile(`(?i)^(❯\s*)?(\d\.\s*)?(yes, i trust this folder|yes, proceed|no, exit)`)

// excerpt is the last few lines of text on the role's screen, frames and rules left out, as one line.
func (s *Shift) excerpt(name string) string {
	text, err := s.Tmux.Capture(name, 30)
	if err != nil {
		return "(screen unreadable: " + err.Error() + ")"
	}

	var keep []string

	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(strings.Trim(l, "│╭╮╰╯─ ")); strings.IndexFunc(l, unicode.IsLetter) >= 0 {
			keep = append(keep, l)
		}
	}

	if len(keep) > 5 {
		keep = keep[len(keep)-5:]
	}

	return clip(strings.Join(keep, " / "))
}

func clip(s string) string {
	s = oneLine(s)
	if r := []rune(s); len(r) > excerptMax {
		return string(r[:excerptMax]) + "…"
	}

	return s
}

// landed checks the line last typed into a role. It reports false while that line still has to show it was
// taken: then nothing new is typed. A line not taken within confirmAfter is pressed again when it still sits in
// the input line, else typed again; after typeTries it moves to the failed list.
func (s *Shift) landed(name string, now time.Time) bool {
	rs := s.State.Role(name)
	if rs.Sent == nil {
		return true
	}

	if sess := s.session(name); sess != nil && sess.StatusUpdatedAt > rs.Sent.At.UnixMilli() {
		rs.Sent = nil

		return true
	}

	if now.Sub(rs.Sent.At) < confirmAfter {
		return false
	}

	sent := rs.Sent
	sent.Tries++

	if sent.Tries >= typeTries {
		rs.Failed = append(rs.Failed, Failed{Line: sent.Line, Error: "typed, never taken", At: now})
		rs.Sent = nil
		s.log(name, "type-failed", "", "never taken: "+sent.Line)

		return true
	}

	if draft, ok, err := s.Tmux.Prompt(name); err == nil && ok && draft != "" &&
		strings.HasPrefix(oneLine(sent.Line), firstWords(draft)) {
		// Typed but not sent: press Enter.
		_ = s.Tmux.Keys(name, "Enter")
		sent.At = now
		s.log(name, "pressed-again", "", sent.Line)

		return false
	}

	if err := s.Tmux.Type(name, sent.Line); err != nil {
		s.log(name, "type-failed", "", fmt.Sprintf("try %d: %s: %s", sent.Tries, sent.Line, err))
	} else {
		s.log(name, "typed-again", "", sent.Line)
	}

	sent.At = now

	return false
}

// firstWords is the start of a draft as the input line shows it: wrapped lines and the cut-off end left out.
func firstWords(draft string) string {
	r := []rune(oneLine(draft))
	if len(r) > 40 {
		r = r[:40]
	}

	return string(r)
}
