package shift

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/screenrules"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// Source is where a fact about a role came from. They are trusted in this order: a hook's record, Claude Code's session
// file (the registry), and — for what neither says — the screen. tmux says whether the role runs at all.
type Source string

const (
	SourceHook     Source = "hook"
	SourceRegistry Source = "registry"
	SourceScreen   Source = "screen"
	SourceTmux     Source = "tmux"
	// SourceNone is the decider of a status nothing says: an up role whose session file is not there yet.
	SourceNone Source = "none"
)

// Reasoning is what a look at a role read to make its View: each source's own word, kept beside the view that was made of
// it, never read again. Look returns it with the view and Views drops it, so that what explains a status is the look that
// made the status.
type Reasoning struct {
	// At is the moment of the look.
	At time.Time
	// Engine is the CLI the role runs on (screenrules.Claude or Codex): the rules that read its screen.
	Engine string
	// Pane and ProbeErr are what tmux said of the role's session.
	Pane     tmux.Pane
	ProbeErr error
	// Session is the registry entry of the role, nil when it has none (yet); SessionErr why it could not be read.
	Session    *registry.Session
	SessionErr error
	// InputKnown is that the screen's input line could be looked for (a Claude role); InputPresent that it is there.
	InputKnown, InputPresent bool
	// Screen is the capture the look read (nowLines rows), ScreenErr why there is none; ScreenDialog what ParseDialog read of it
	// when the screen shows no input line.
	Screen       string
	ScreenErr    error
	ScreenDialog *Dialog
	// Failure is the record the role's StopFailure hook left of this session, whatever turn it is of; FailureWhy why there is
	// none; FailureCurrent that it is the turn the session is idle since, which makes it part of the status.
	Failure        *TurnFailure
	FailureWhy     string
	FailureCurrent bool
	// Question is the record its PermissionRequest hook left of a question; QuestionCurrent that it is the one on the screen;
	// QuestionWhy why not. Nil unless the role is at a dialog and its session waits on a question.
	Question        *AskedQuestion
	QuestionCurrent bool
	QuestionWhy     string
}

// Said is what one source said of a role, and whether it decided the status shown.
type Said struct {
	Source Source `json:"source"`
	// Kind is what in the source said it: the hook's kind (StopFailure, PermissionRequest), the registry field (status), the
	// screen rule's id (claude.input.line), or tmux's own word (session).
	Kind string `json:"kind"`
	// Says is what the source said, in words.
	Says string `json:"says"`
	// Evidence is the value behind it: a field and its value, the screen line a rule matched (cut, control characters
	// stripped).
	Evidence string `json:"evidence,omitempty"`
	// At is when the source said it, AgeSeconds how long before the look.
	At         time.Time `json:"at,omitzero"`
	AgeSeconds int       `json:"age_seconds,omitempty"`
	// Why is, for a source that did not decide, why not.
	Why string `json:"why,omitempty"`
}

// Explained is why a role's status is what it is, and what bears on the lines queued for it. Every field comes from the
// look that Views takes (Look), from the state as saved, or from a record read for it: nothing is written.
type Explained struct {
	Role  string   `json:"role"`
	State Presence `json:"state"`
	// Summary is the status in plain words, with the source that decided it.
	Summary string `json:"summary"`
	// Decided is the source that decided the status shown; Lost the other sources consulted, in the order they are
	// trusted (hook, registry, screen), each with what it said and why it did not decide.
	Decided Said   `json:"decided"`
	Lost    []Said `json:"lost,omitempty"`
	// Screen is what the screen says apart from the status: its input line, a limit notice, the words last said, the dialog.
	Screen  ScreenFacts  `json:"screen"`
	Queue   QueueFacts   `json:"queue"`
	Context ContextFacts `json:"context"`
	Restart RestartFacts `json:"restart"`
	// LastKnown is what is kept of a role that is not up.
	LastKnown *LastKnown `json:"last_known,omitempty"`
}

// ScreenFacts are what the rules read of a Claude or Codex screen that is not itself the status.
type ScreenFacts struct {
	// Read is that a capture was taken; Error why not.
	Read  bool   `json:"read"`
	Error string `json:"error,omitempty"`
	// Input is the input line: its presence (the rule that reads it is a builtin).
	Input *Said `json:"input,omitempty"`
	// Draft says what stands in the input line: none, a draft of the owner's, or a dim suggestion.
	Draft string `json:"draft,omitempty"`
	// Limit is the usage-limit notice on the bottom rows, with the rule and the line.
	Limit *Said `json:"limit,omitempty"`
	// Now is the last words the role said.
	Now *Said `json:"now,omitempty"`
	// Dialog is the question shown instead of the input line, with where its words came from.
	Dialog *DialogFact `json:"dialog,omitempty"`
}

// DialogFact is the question a role shows instead of its input line.
type DialogFact struct {
	// From is hook (the question its PermissionRequest hook recorded) or screen (what the screen reads as).
	From     Source `json:"from"`
	Question string `json:"question"`
	Options  int    `json:"options"`
	// Why says why a recorded question was not used.
	Why string `json:"why,omitempty"`
}

// QueueFacts are what bears on the lines queued for a role.
type QueueFacts struct {
	// Lines wait in its inbox; Failed could not be typed, Attention of them may have reached it.
	Lines     int `json:"lines"`
	Failed    int `json:"failed,omitempty"`
	Attention int `json:"attention,omitempty"`
	// InFlight is the line typed into it that has not landed yet: what it is and how long, never its words.
	InFlight *InFlightFact `json:"in_flight,omitempty"`
	// Held is the failed turn that holds the queue, while one does: why and until when.
	Held *HoldFact `json:"held,omitempty"`
	// SettlingUntil is when a slash command typed into it has settled; nothing is delivered before.
	SettlingUntil time.Time `json:"settling_until,omitzero"`
	// Waits are the other things in the way of a line typed now, in words.
	Waits []string `json:"waits,omitempty"`
}

// InFlightFact is a line typed into a role that has not landed: its kind (a line, a doorbell, lines joined into one) and its length
// in characters. Its words are not printed: they are the owner's or another role's, and an explanation is read by anyone who may
// read the crew's status.
type InFlightFact struct {
	Kind   string `json:"kind"`
	Length int    `json:"length"`
}

// HoldFact is a queue held for a failed turn.
type HoldFact struct {
	// Kind is what kind of error it was (transient, limit, account), Text the words of it, Why what is done about it.
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`
	Why   string    `json:"why"`
	Until time.Time `json:"until"`
	// At is when the failed turn was recorded.
	At time.Time `json:"at"`
}

// ContextFacts are how full the role's conversation is against its thresholds. A threshold of 0 is off.
type ContextFacts struct {
	Tokens int `json:"tokens"`
	// ContextMax is context_max (the ticker types ContextAction into an idle role past it), RestartAt restart_at (a role
	// holding a task is restarted fresh with its note past it), RestartDone restart_done (a role whose task is done).
	ContextMax    int    `json:"context_max"`
	ContextAction string `json:"context_action,omitempty"`
	RestartAt     int    `json:"restart_at"`
	RestartDone   int    `json:"restart_done"`
	// Words says it in a line.
	Words string `json:"words"`
}

// RestartFacts are the restarts and trims begun and not finished.
type RestartFacts struct {
	// Pending is a restart or release the ticker began and has not finished.
	Pending string `json:"pending,omitempty"`
	// Asked is when the role was asked for its note before a restart, and why.
	Asked    time.Time `json:"asked,omitzero"`
	AskedWhy string    `json:"asked_why,omitempty"`
	// Due is that a restart waits for the role to settle (it passed restart_at holding a task).
	Due bool `json:"due,omitempty"`
	// Trim is that a compaction or a clear typed into it has not shown yet.
	Trim bool `json:"trim,omitempty"`
	// Rotation says a rotation is in flight.
	Rotation string `json:"rotation,omitempty"`
	// Words says it in a line; empty when nothing is pending.
	Words string `json:"words,omitempty"`
}

// LastKnown is what the state keeps of a role that is not up.
type LastKnown struct {
	Started     time.Time `json:"started,omitzero"`
	LastSession string    `json:"last_session,omitempty"`
	// Held, Broken and Asleep are why the ticker leaves it down.
	Held   bool      `json:"held,omitempty"`
	Broken bool      `json:"broken,omitempty"`
	Asleep time.Time `json:"asleep,omitzero"`
	Tasks  []string  `json:"tasks,omitempty"`
	// Words says it in a line.
	Words string `json:"words"`
}

// evidenceMax is the most characters of a screen line an explanation quotes.
const evidenceMax = 120

// sessionTag is the random tag a role's lines carry ([zmina:<12 characters>]): it is what tells a line from a role's own session
// from one another session sends, so it is never quoted. A terminal wraps a long line anywhere, and a quoted row is folded with a
// space where the wrap was, so the tag is also read with one space in it, at any place; and a row that ends in the middle of one
// (sessionTagStart) ends in the start of it.
var (
	sessionTag      = regexp.MustCompile(spacedTag())
	sessionTagStart = regexp.MustCompile(`\[zmina:[A-Za-z0-9 ]{0,13}$`)
)

// spacedTag is a pattern for the tag whole, or with a single space between any two of its characters.
func spacedTag() string {
	parts := []string{`\[`, `z`, `m`, `i`, `n`, `a`, `:`}
	for range 12 {
		parts = append(parts, `[A-Za-z0-9]`)
	}

	parts = append(parts, `\]`)

	alt := []string{strings.Join(parts, "")}
	for i := 1; i < len(parts); i++ {
		alt = append(alt, strings.Join(parts[:i], "")+" "+strings.Join(parts[i:], ""))
	}

	return strings.Join(alt, "|")
}

const tagMasked = "[zmina:…]"

// maskTags shows each session tag in a text as tagMasked, a partial one at the end of it too.
func maskTags(text string) string {
	text = sessionTag.ReplaceAllString(text, tagMasked)

	return sessionTagStart.ReplaceAllStringFunc(text, func(partial string) string {
		// The start of a tag has at most the 12 characters and one space; any more is words after a bracket.
		if strings.Count(partial, " ") > 1 || len(strings.ReplaceAll(partial, " ", "")) > len("[zmina:")+12 {
			return partial
		}

		return tagMasked
	})
}

// Quote is a text as an explanation quotes it, and the one way every quotation of a screen row, a line body, a status or a
// crew's own words goes out: control characters stripped and white space folded first, so that nothing hides in them, then
// any session tag marked as there and not shown, then cut to evidenceMax characters.
func Quote(text string) string { return quoteN(text, evidenceMax) }

// quoteN is Quote cut to n characters.
func quoteN(text string, n int) string {
	return clipRunes(maskTags(oneLine(text)), n)
}

func quote(line string) string { return Quote(line) }

// age is how long before at the look was, in whole seconds, never negative.
func (r Reasoning) age(at time.Time) int {
	if at.IsZero() || at.After(r.At) {
		return 0
	}

	return int(r.At.Sub(at).Seconds())
}

// said makes a Said from a source's word with its time, in UTC: the same instant whatever zone the reader is in.
func (r Reasoning) said(src Source, kind, says, evidence string, at time.Time) Said {
	if !at.IsZero() {
		at = at.UTC()
	}

	return Said{Source: src, Kind: kind, Says: says, Evidence: evidence, At: at, AgeSeconds: r.age(at)}
}

// registrySaid is what the role's session file said, or that it said nothing.
func (r Reasoning) registrySaid(v View) Said {
	who := "its Claude Code session file"
	if r.Engine == screenrules.Codex {
		who = "its Codex bridge's status file"
	}

	sess := r.Session

	switch {
	case r.SessionErr != nil:
		says := who + " could not be read: " + quote(r.SessionErr.Error())
		if v.StatusError != "" {
			says = who + " does not read (empty, cut short or damaged), so its status is unknown"
		}

		return Said{Source: SourceRegistry, Kind: "session file", Says: says, Evidence: quote(r.SessionErr.Error())}
	case sess == nil:
		return Said{Source: SourceRegistry, Kind: "session file", Says: who + " is not there for its process yet"}
	case sess.Status == "":
		return r.said(SourceRegistry, "status", who+" gives no status", "status is empty", sess.StatusSince())
	}

	says := who + " says it is " + quote(sess.Status)

	switch waitKind(sess) {
	case WaitPermission:
		says += " on a permission prompt"
	case WaitQuestion:
		says += " on a question put to the owner"
	default:
		if sess.Status == "waiting" {
			says += " (the file does not say on what)"
		}
	}

	ev := "status=" + quote(sess.Status)
	if sess.WaitingFor != "" {
		ev += ", waitingFor=" + quote(sess.WaitingFor)
	}

	ev += fmt.Sprintf(", pid=%d", sess.PID)

	return r.said(SourceRegistry, "status", says, ev, sess.StatusSince())
}

// hookFailureSaid is what the StopFailure record said, whether or not it decided.
func (r Reasoning) hookFailureSaid(sess *registry.Session) Said {
	f := r.Failure
	if f == nil {
		return Said{Source: SourceHook, Kind: "StopFailure", Says: "nothing recorded for this session", Why: r.FailureWhy}
	}

	says := "its last turn ended in an API error: " + quote(f.Text())
	ev := "record of session " + shortID(f.SessionID) + " written by the StopFailure hook"

	s := r.said(SourceHook, "StopFailure", says, ev, f.At)

	if r.FailureCurrent {
		return s
	}

	switch {
	case sess == nil:
		s.Why = "the role has no session file to match it to"
	case sess.Status != "idle":
		s.Why = "an older turn's record: the session is " + quote(sess.Status) + " now, not idle since that turn"
	default:
		s.Why = "an older turn's record: the session went idle at " + sess.StatusSince().UTC().Format(time.RFC3339) +
			", not within " + hookSkew.String() + " of it"
	}

	return s
}

// shortID is the start of a session id.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}

	return id
}

// inputSaid is the screen rule that reads the input line, as it read this screen.
func (r Reasoning) inputSaid() Said {
	s := Said{Source: SourceScreen, Kind: screenrules.BuiltinInputLine}

	switch {
	case r.Engine == screenrules.Codex:
		s.Kind = screenrules.BuiltinBridgeMode
		s.Says = "not read: a Codex role's waiting comes from its bridge's status file"
	case !r.InputKnown:
		s.Says = "the input line could not be looked for"
	case r.InputPresent:
		s.Says = "an input line is on the screen: the role is at its prompt or in a turn, not at a dialog"
	default:
		s.Says = "no input line is on the screen: a dialog, a login or a screen being drawn"
		s.Evidence = quote(lastRow(r.Screen))
	}

	return s
}

// lastRow is the last row of a screen that has words.
func lastRow(screen string) string {
	rows := strings.Split(screen, "\n")
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.TrimSpace(rows[i]) != "" {
			return rows[i]
		}
	}

	return ""
}

// Explain says why a view is what it is: from the reasoning of the look that made it, in the order the sources are
// trusted. It reads the input line (to say whether a draft stands in it) and the hold of the queue, and writes nothing.
func (s *Shift) Explain(v View, r Reasoning) Explained {
	e := Explained{Role: v.Role, State: v.State}
	sess := r.Session

	rs := s.State.Role(v.Role)

	// What each source said of the role, in the order they are trusted.
	var hook, reg, scr []Said

	switch v.State {
	case PresenceUp:
		hook = append(hook, r.hookFailureSaid(sess))
		reg = append(reg, r.registrySaid(v))
		scr = append(scr, r.inputSaid())

		e.Decided, hook, reg, scr = r.decide(v, hook, reg, scr)
	default:
		e.Decided = s.presenceSaid(v, r)
	}

	e.Lost = append(append(hook, reg...), scr...)
	e.Screen = s.screenFacts(v, r)
	e.Queue = s.queueFacts(v, r, rs)
	e.Context = s.contextFacts(v)
	e.Restart = s.restartFacts(v, rs)

	if v.State != PresenceUp {
		e.LastKnown = s.lastKnown(v, rs)
	}

	e.Summary = summary(v, e)

	return e
}

// decide picks the source that decided the status shown — the first, in the order hook, registry, screen, that has a word
// in it — and leaves the others as the sources that lost, each with why.
func (r Reasoning) decide(v View, hook, reg, scr []Said) (Said, []Said, []Said, []Said) {
	var decided Said

	switch {
	case r.FailureCurrent:
		decided, hook = hook[0], nil
		reg[0].Why = "its word is kept (" + quote(v.Status) + "); the hook's record adds the error to it"
		scr[0].Why = "it does not change the status"

		if v.Prompt {
			scr[0].Why = "it adds the (dialog) mark ls shows, not the status word"
		}
	case r.Session != nil && r.Session.Status != "" || r.SessionErr != nil:
		decided, reg = reg[0], nil
		hook[0].Why = r.hookWhy(hook[0])
		scr[0].Why = "the session file already says it"

		if v.Prompt {
			scr[0].Why = "it adds the (dialog) mark ls shows, not the status word"
		}
	case v.Prompt:
		decided, scr = scr[0], nil
		reg[0].Why = "it gave no status"
		hook[0].Why = r.hookWhy(hook[0])
	default:
		decided = Said{Source: SourceNone, Kind: "none", Says: "nothing says: the role is up, its session file gives no status yet and its screen shows an input line"}
		hook[0].Why = r.hookWhy(hook[0])
		reg[0].Why = "it gave no status"
		scr[0].Why = "it shows an input line: nothing to add"
	}

	return decided, hook, reg, scr
}

// hookWhy is why the hook's word did not decide: its own reason when it has one, else that it said nothing.
func (r Reasoning) hookWhy(s Said) string {
	if s.Why != "" {
		return s.Why
	}

	return "it said nothing of the status"
}

// presenceSaid is what tmux said of a role that is not up.
func (s *Shift) presenceSaid(v View, r Reasoning) Said {
	switch v.State {
	case PresenceDown:
		return Said{Source: SourceTmux, Kind: "session", Says: "down: no session"}
	case PresenceDead:
		return Said{Source: SourceTmux, Kind: "session", Says: "dead: its process ended and its screen is kept until the ticker collects it"}
	case PresenceOutside:
		return Said{Source: SourceRegistry, Kind: "session file", Says: "outside: a claude with this role's session name runs, but not under zmina"}
	default:
		says := "unknown: tmux could not be asked"
		if r.ProbeErr != nil {
			says += ": " + quote(r.ProbeErr.Error())
		}

		return Said{Source: SourceTmux, Kind: "session", Says: says}
	}
}

// screenFacts are the rules' readings of the screen that are not the status.
func (s *Shift) screenFacts(v View, r Reasoning) ScreenFacts {
	var f ScreenFacts

	if v.State != PresenceUp {
		return f
	}

	f.Read = r.ScreenErr == nil
	if r.ScreenErr != nil {
		f.Error = quote(r.ScreenErr.Error())
	}

	in := r.inputSaid()
	f.Input = &in

	// What stands in the input line: read here, from the screen with its colours, never typed into.
	if r.Engine == screenrules.Claude && r.InputKnown && r.InputPresent {
		draft, suggestion, ok, err := s.Tmux.Input(v.Role)

		switch {
		case err != nil || !ok:
			f.Draft = "its input line could not be read"
		case draft != "":
			f.Draft = fmt.Sprintf("holds a draft of %d characters", len([]rune(draft)))
		case suggestion != "":
			f.Draft = "empty (a dim suggestion is shown, not typed)"
		default:
			f.Draft = "empty"
		}
	}

	if r.ScreenErr == nil {
		// The usage-limit notice: the bottom rows, as the rules of the engine say how many.
		window := s.ScreenRules().Window(r.Engine, screenrules.UsageLimit, limitLines)
		if _, hit := limitHit(s.ScreenRules(), r.Engine, screenrules.Tail(r.Screen, window)); hit.Rule.ID != "" {
			l := r.said(SourceScreen, quote(hit.Rule.ID), "a usage-limit notice is on its last rows: "+quoteN(hit.Rule.Means, 200), quote(hit.Line), time.Time{})
			l.Why = "not part of the status: ls does not show it; the ticker tells the dispatcher once per stretch"
			f.Limit = &l
		}

		if v.Now != "" {
			now := Said{Source: SourceScreen, Kind: screenrules.BuiltinNowLine, Says: "the last words it said", Evidence: quote(v.Now),
				At: v.NowAt.UTC()}
			f.Now = &now
		}
	}

	if v.Prompt && !s.isCodex(v.Role) {
		f.Dialog = r.dialogFact(v)
	}

	return f
}

// dialogFact says where the dialog shown came from.
func (r Reasoning) dialogFact(v View) *DialogFact {
	d := v.Dialog
	if d == nil {
		return nil
	}

	fact := &DialogFact{From: SourceScreen, Question: quote(d.Question), Options: len(d.Options)}
	if r.QuestionCurrent {
		fact.From = SourceHook
	} else if r.QuestionWhy != "" {
		fact.Why = "the question its hook recorded was not used: " + r.QuestionWhy
	}

	return fact
}

// queueFacts are what bears on the lines queued for the role, read from the state as saved and the records.
func (s *Shift) queueFacts(v View, r Reasoning, rs *RoleState) QueueFacts {
	q := QueueFacts{Lines: len(rs.Inbox), Failed: len(rs.Failed), Attention: rs.attention()}

	if rs.Sent != nil {
		q.InFlight = &InFlightFact{Kind: "line", Length: len([]rune(rs.Sent.Line))}

		switch {
		case IsBell(rs.Sent.Line):
			q.InFlight.Kind = "doorbell"
		case len(rs.Sent.Parts) > 0:
			q.InFlight.Kind = "lines joined into one"
		}

		q.Waits = append(q.Waits, "a line typed into it has not landed yet")
	}

	// Held for a failed turn: the test the delivery makes (failureHeld) — a record of this session, the turn it is idle since,
	// within its hold — over the session this very look read, so that what ls shows of the hold (turn_error_held_until) and
	// this can never differ.
	if f := r.Failure; r.FailureCurrent && f.holds(r.At) && !s.isCodex(v.Role) {
		c := f.Class()
		q.Held = &HoldFact{Kind: string(c.Kind), Text: quote(f.Text()), Why: HoldWords(f, s.Crew.Location()),
			Until: f.holdEnd().In(s.Crew.Location()), At: f.At}
	}

	if !rs.SlashAt.IsZero() && within(r.At, rs.SlashAt, slashSettle) {
		q.SettlingUntil = rs.SlashAt.Add(slashSettle)
		q.Waits = append(q.Waits, "a slash command typed into it is settling")
	}

	if v.State != PresenceUp {
		if q.Lines > 0 {
			q.Waits = append(q.Waits, "it is not up: the lines wait for its next start")
		}

		return q
	}

	switch v.Status {
	case "busy", "shell", "working":
		q.Waits = append(q.Waits, "it is "+quote(v.Status)+": a line is typed when its turn ends")
	case "waiting":
		q.Waits = append(q.Waits, "it waits on a dialog: nothing is typed into a dialog")
	}

	if v.Prompt && v.Status != "waiting" {
		q.Waits = append(q.Waits, "no input line is on its screen")
	}

	return q
}

// contextFacts put the size of the conversation against the thresholds of the role.
func (s *Shift) contextFacts(v View) ContextFacts {
	c := ContextFacts{Tokens: v.Context, ContextMax: v.ContextMax}

	r, err := s.Role(v.Role)
	if err != nil {
		c.Words = "the role is not in the crew file"

		return c
	}

	c.ContextAction, c.RestartAt, c.RestartDone = r.ContextDo(), r.RestartContext(), r.RestartDoneAt()

	size := "its conversation size is not known"
	if v.Context > 0 {
		size = Tokens(v.Context) + " tokens"
	}

	words := []string{size}

	words = append(words, threshold("context_max", c.ContextMax, v.Context, "the ticker types /"+c.ContextAction+" once it sits idle with nothing waiting"))
	words = append(words, threshold("restart_at", c.RestartAt, v.Context, "a role holding a task is restarted fresh with its note"))
	words = append(words, threshold("restart_done", c.RestartDone, v.Context, "a role whose task is done is restarted fresh once idle"))

	c.Words = strings.Join(words, "; ")

	return c
}

// threshold says where a size stands against one threshold.
func threshold(name string, limit, size int, then string) string {
	switch {
	case limit <= 0:
		return name + " off"
	case size <= 0:
		return name + " " + Tokens(limit)
	case size > limit:
		return name + " " + Tokens(limit) + ": over it, " + then
	default:
		return name + " " + Tokens(limit) + ": under it"
	}
}

// restartFacts are the restarts and trims that were begun and not finished.
func (s *Shift) restartFacts(v View, rs *RoleState) RestartFacts {
	f := RestartFacts{Asked: rs.RestartAsked, AskedWhy: quoteN(rs.RestartWhy, 80), Due: rs.RestartDue != "", Trim: rs.TrimPending}

	var words []string

	if p := rs.Pending; p != nil {
		f.Pending = p.Kind
		if p.Why != "" {
			f.Pending += ": " + quoteN(p.Why, 80)
		}

		words = append(words, "a "+p.Kind+" the ticker began "+p.Claim.UTC().Format(time.RFC3339)+" is waiting for the role to end")
	}

	if !rs.RestartAsked.IsZero() {
		w := "it was asked for its note before a restart at " + rs.RestartAsked.UTC().Format(time.RFC3339)
		if rs.RestartWhy != "" {
			w += " (" + quoteN(rs.RestartWhy, 80) + ")"
		}

		words = append(words, w)
	}

	if f.Due {
		words = append(words, "a restart waits for it to settle (it passed restart_at holding a task)")
	}

	if f.Trim {
		words = append(words, "a /compact or /clear typed into it has not shown yet")
	}

	if v.Rotation != nil && v.Rotation.Open {
		f.Rotation = v.Rotation.ID
		words = append(words, "rotation "+v.Rotation.ID+" is in flight")
	}

	f.Words = strings.Join(words, "; ")

	return f
}

// lastKnown is what the state keeps of a role that is not up.
func (s *Shift) lastKnown(v View, rs *RoleState) *LastKnown {
	k := &LastKnown{Started: rs.Started, LastSession: shortID(rs.LastSession), Held: rs.Held, Broken: rs.Broken, Asleep: v.Asleep,
		Tasks: v.Tasks}

	var words []string

	if !rs.Started.IsZero() {
		words = append(words, "last started "+rs.Started.UTC().Format(time.RFC3339))
	} else {
		words = append(words, "never started under this state")
	}

	if rs.LastSession != "" {
		words = append(words, "on conversation "+shortID(rs.LastSession))
	}

	switch {
	case rs.Broken:
		words = append(words, "marked broken (it died on boot; not started again until zmina up)")
	case !v.Asleep.IsZero():
		words = append(words, "asleep until "+v.Asleep.UTC().Format(time.RFC3339))
	case rs.Held:
		words = append(words, "put down by hand: the ticker leaves it down until the next up")
	}

	if len(v.Tasks) > 0 {
		words = append(words, "holds "+strings.Join(v.Tasks, ", "))
	}

	k.Words = strings.Join(words, "; ")

	return k
}

// summary is the status in plain words with the source that decided it.
func summary(v View, e Explained) string {
	switch v.State {
	case PresenceUp:
		status := v.Status
		if status == "" {
			status = "no status"
		}

		return fmt.Sprintf("up, %s: decided by the %s (%s)", quote(status), e.Decided.Source, quote(e.Decided.Kind))
	default:
		// What tmux said already starts with the state: "down: no session".
		return e.Decided.Says
	}
}
