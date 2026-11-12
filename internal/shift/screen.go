package shift

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/iamnikolie/zmina/internal/tmux"
)

const (
	// blockedAfter is how long a role's screen may show no input line before the dispatcher hears of it: a
	// dialog (trust this folder, a permission), a login, a screen claude could not draw.
	blockedAfter = 3 * time.Minute
	// bootBlockedAfter is how long a booting role's screen may show no input line: Claude Code draws it within
	// seconds of its start, so past this a dialog holds the boot.
	bootBlockedAfter = 45 * time.Second
	// confirmAfter is how long a typed line has to show that it landed — the role's status changes when it takes
	// a line — before its own remaining draft gets Enter again.
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
	if s.isCodex(name) {
		// The bridge hides its input line during normal work. Only a structured
		// waiting status proves that an approval or question blocks the turn.
		sess := s.session(name)
		ok, err = sess == nil || sess.Status != "waiting", nil
	}
	if err == nil && !ok && !s.isCodex(name) && s.trust(name) {
		// Answered at once: the boot goes on.
		return nil
	}

	// What the role says it is doing, also while it boots.
	if text, err := s.Tmux.Capture(name, nowLines); err == nil {
		if line := NowLine(text); line != "" && line != rs.Now {
			rs.Now, rs.NowAt = line, now
		}
	}

	if !s.booted(name, now) {
		// A boot with no input line on screen waits on a dialog (a folder not trusted, a login, a permission) and
		// reads nothing queued for it: the dispatcher hears of it now, not boot_grace and blockedAfter later.
		if err == nil && !ok && !rs.BlockedNoted && now.Sub(rs.Started) >= bootBlockedAfter {
			rs.BlockedNoted, rs.NoPrompt = true, rs.Started
			note := fmt.Sprintf("%s: up %d s and still no input line, %s holds its boot and its queued lines "+
				"wait: %s (zmina peek %s; zmina keys %s ...)", name, int(now.Sub(rs.Started).Seconds()),
				waitNoun(waitKind(s.session(name))), s.excerpt(name), name, name)
			s.log(name, "blocked", "", note)

			return []string{note}
		}

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
		// Which of the two it waits on is the session file's word when it has one; otherwise only the screen says.
		on := "waiting on"
		if kind := waitKind(s.session(name)); kind != "" {
			on += " " + waitNoun(kind)
		}

		note := fmt.Sprintf("%s: no input line on its screen for %d min, %s: %s (zmina peek %s; "+
			"zmina keys %s ...)", name, int(now.Sub(rs.NoPrompt).Minutes()), on, s.excerpt(name), name, name)
		s.log(name, "blocked", "", note)
		notes = append(notes, note)
	}

	// A last turn that ended in an API error: from its StopFailure hook when it has one, else from the transcript.
	if note := s.apiErrorNote(name, rs); note != "" {
		notes = append(notes, note)
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

// landed reports whether the line typed into a role last has landed, so the next may be typed. A line still in the
// role's input line has not, whatever its status says (a message through Remote Control moves the status too): after
// confirmAfter it gets Enter again. An empty input without an observed status change is ambiguous: keep the
// line as failed for inspection instead of replaying a possibly accepted request. After typeTries a remaining
// own draft is cleared and kept as failed.
func (s *Shift) landed(name string, now time.Time) bool {
	rs := s.State.Role(name)
	if rs.Sent == nil {
		return true
	}

	sent := rs.Sent

	landed, unsent := s.typedLanded(name, sent)
	if landed {
		s.closeTyped(name)

		return true
	}

	if now.Sub(sent.At) < confirmAfter || !unsent && !s.atPrompt(name, now) {
		return false
	}

	sent.Tries++

	reason := "typed, never taken"
	if !unsent {
		reason = UnclearUnconfirmed
	}
	if !unsent || sent.Tries >= typeTries {
		if unsent {
			s.clearInput(name, sent.Line)
		}

		// Acked meanwhile, every id of it by name (clearInFlight recorded them): its readers have it, so there is
		// nothing to inspect or retry. An id that is merely not among the acks (evicted, dropped) says nothing: nobody
		// read the line.
		if ackedExplicitly(sent) {
			rs.Sent = nil
			s.log(name, "acked-cleared", "", "typed: "+sent.Line)

			return true
		}

		rs.Failed = append(rs.Failed, Failed{Line: sent.Line, Error: reason, At: now, IDs: sent.IDs, Parts: sent.Parts})
		rs.Sent = nil
		s.log(name, "type-failed", "", reason+": "+sent.Line)
		s.deliveryFailure(name, sent.Line, reason)

		return true
	}

	sent.At = now

	_ = s.Tmux.Keys(name, "Enter")
	s.log(name, "pressed-again", "", sent.Line)

	return false
}

// typedLanded reports whether the line typed last has been taken: it is no longer in the role's input line and the
// role's status changed since it was typed. unsent is that it still sits in the input line.
func (s *Shift) typedLanded(name string, sent *Sent) (landed, unsent bool) {
	draft, ok, err := s.Tmux.Prompt(name)
	if err == nil && ok && holds(draft, sent.Line) {
		return false, true
	}

	sess := s.session(name)

	return sess != nil && sess.StatusUpdatedAt > sent.At.UnixMilli(), false
}

// closeTakenTyped closes the record of the line typed into a role last when the role is seen to have taken it (typedLanded):
// nothing else closes it while no other line waits, and a record left open lists a line long processed as one in flight
// (queue ls), blocks zmina send and, when the session ends, would be judged as a line that never landed.
func (s *Shift) closeTakenTyped(name string) {
	if sent := s.State.Role(name).Sent; sent != nil {
		if landed, _ := s.typedLanded(name, sent); landed {
			s.closeTyped(name)
		}
	}
}

// typedTaken reports a sign that a role took the line typed into it last, where the role may be gone and no status can be
// read: its status changed after the line (typedLanded), or its conversation shows a turn that ended after it. A line
// still in the input line was not taken.
func (s *Shift) typedTaken(name string, sent *Sent) bool {
	landed, unsent := s.typedLanded(name, sent)
	if landed || unsent {
		return landed
	}

	id := s.State.Role(name).LastSession
	if id == "" {
		return false
	}

	turn, err := s.registryFor(name).Last(id)

	return err == nil && turn.Ended && turn.At.After(sent.At)
}

// closeTyped closes the record of a typed line that landed. A typed line shows no seen: the info lines it carried are
// closed when it lands.
func (s *Shift) closeTyped(name string) {
	rs := s.State.Role(name)

	if name == s.Crew.Dispatcher {
		s.closeInfo(name, rs.Sent.Parts, "typed")
	}

	rs.Sent = nil
}

// failUnclear keeps a line that may have reached its role as a failed one that needs attention (Failed.Unclear), says so
// in the log, and tells the dispatcher once (deliveryFailure). It is never queued again by itself.
func (s *Shift) failUnclear(name string, f Failed) {
	rs := s.State.Role(name)
	rs.Failed = append(rs.Failed, f)
	s.log(name, "type-failed", "", "unclear, kept for a look: "+f.Error+": "+f.Line)
	s.deliveryFailure(name, f.Line, f.Error)
}

// deliveryFailure makes a stalled handoff visible without waiting for an idle notice.
func (s *Shift) deliveryFailure(name, line, reason string) {
	ids := append([]string(nil), s.held()[name]...)
	if m := regexp.MustCompile(`\bT-[0-9]+\b`).FindString(line); m != "" && !slices.Contains(ids, m) {
		ids = append(ids, m)
	}
	note := name + ": delivery failed"
	if len(ids) > 0 {
		note += " (" + strings.Join(ids, ", ") + ")"
	}
	text := note + ": " + reason + "; inspect zmina queue ls"
	s.addNotes([]string{text})
	meta := s.State.NoticeMeta[text]
	meta.Kind, meta.Role, meta.Line = "delivery", name, line
	s.State.NoticeMeta[text] = meta
}

const (
	// submitWait is how long a typed line is given to leave the input line after its Enter.
	submitWait = 700 * time.Millisecond
	// submitTries is how many times Enter is pressed again on a line that stays in the input line.
	submitTries = 2
)

// errUnsent is a typed line its Enter never sent: it was cleared from the input line.
var errUnsent = errors.New("typed, Enter did not send it; cleared from the input line")

// typeLine types a line and presses Enter, then makes sure the Enter sent it: Claude Code can take a long line typed
// in one burst, its Enter with it, as a paste, and leave it in the input line. A line still there gets Enter again;
// one that stays is cleared and errUnsent returned — a line zmina typed is sent, or not left typed. An input line
// that cannot be read (a dialog, a screen being drawn) is left to landed.
func (s *Shift) typeLine(name, line string) error {
	// Only the owner's own message keeps its line breaks: it goes to a Claude role's prompt, where Ctrl-J breaks a
	// line. Any other line is typed as one, as a Codex bridge or a role typed into takes it.
	typ := s.Tmux.Type
	if s.ownerChat(name, line) {
		typ = s.Tmux.TypeLines
	}

	if err := typ(name, line); err != nil {
		// None of it was typed (tmux was not asked to send a key, or refused the first): nothing is in the input line,
		// nothing reached the role, and no start is left to look for.
		if errors.Is(err, tmux.ErrNotTyped) {
			return err
		}

		return s.clearPartial(name, line, err)
	}

	// Typed whole into an input line read empty: nothing of an earlier broken line is left to look for.
	s.State.Role(name).Partial = ""

	for try := 0; ; try++ {
		s.pause(submitWait)

		draft, ok, err := s.Tmux.Prompt(name)
		if err != nil || !ok || !holds(draft, line) {
			return nil
		}

		if try == submitTries {
			s.clearInput(name, line)

			return errUnsent
		}

		_ = s.Tmux.Keys(name, "Enter")
		s.log(name, "pressed-again", "", line)
	}
}

// clearPartial is what is left of a line whose typing failed, cleared from the input line, and returns typeErr, as it
// is or marked as unclear. Typing goes in pieces, so a failure half way leaves the typed start in the input line, where
// it would pass for the owner's draft and hold the queue for good. Both callers type only into an input line they read
// empty, so what sits there now is zmina's own when it is the start of the line it was typing (holds); anything else is
// the owner's and stays.
//
// Whether any of the line reached the session is known only from the input line. typeErr stays as it is, a failure after
// which a counted retry is honest, only when the start was seen there, cleared, and the input line read again without
// it: nothing was sent, and the line is not in the pane (a failure that tmux says came before any key, tmux.ErrNotTyped,
// never gets here). Every other way out is unclear (unclearTyping): the input line
// could not be read (before or after the clear); it was read without the line at all, which is the picture of a typing
// that never started and of one that went whole with its Enter; or the start is there and could not be cleared, where
// any Enter sends it.
//
// The line is kept as the role's Partial until the input line is seen without it: the tmux that failed the typing
// often cannot be read either, and a send-keys that timed out can still reach the pane later, so the delivery that
// comes next looks again (clearLeftover). When the start is seen and cannot be cleared, the error says so.
func (s *Shift) clearPartial(name, line string, typeErr error) error {
	s.State.Role(name).Partial = line

	// The screen is given the time to show what was typed, as after an Enter.
	s.pause(submitWait)

	draft, ok, err := s.Tmux.Prompt(name)
	if err != nil || !ok || !holds(draft, line) {
		return unclearTyping(typeErr)
	}

	s.clearInput(name, line)
	s.pause(submitWait)

	if draft, ok, err = s.Tmux.Prompt(name); err != nil || !ok {
		return unclearTyping(typeErr)
	}

	if holds(draft, line) {
		return unclearTyping(fmt.Errorf("%w; its start could not be cleared from the input line", typeErr))
	}

	return typeErr
}

// clearLeftover looks again for the start of a line whose typing failed (Partial), before a delivery reads the input
// line for the owner's draft: a start still there is cleared, and the mark goes once the input line is read without
// it, empty or holding the owner's own words. An input line that cannot be read keeps the mark.
func (s *Shift) clearLeftover(name string) {
	rs := s.State.Role(name)
	if rs.Partial == "" {
		return
	}

	draft, ok, err := s.Tmux.Prompt(name)
	if err != nil || !ok {
		return
	}

	if holds(draft, rs.Partial) {
		s.clearInput(name, rs.Partial)

		return
	}

	rs.Partial = ""
}

// clearInput takes a typed line zmina could not send out of the role's input line: C-e, then C-u (line end, then
// delete to its start), never C-c, which ends a claude whose input line is already empty.
func (s *Shift) clearInput(name, line string) {
	_ = s.Tmux.Keys(name, "C-e", "C-u")
	s.log(name, "unsent-cleared", "", line)
}

// holds reports whether an input line holds a line zmina typed: its start, as the input line shows it.
func holds(draft, line string) bool {
	return draft != "" && strings.HasPrefix(oneLine(line), firstWords(draft))
}

func (s *Shift) pause(d time.Duration) {
	if s.Pause != nil {
		s.Pause(d)

		return
	}

	time.Sleep(d)
}

// firstWords is the start of a draft as the input line shows it: wrapped lines and the cut-off end left out.
func firstWords(draft string) string {
	r := []rune(oneLine(draft))
	if len(r) > 40 {
		r = r[:40]
	}

	return string(r)
}
