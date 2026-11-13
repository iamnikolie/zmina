// Package screenrules is the table of what zmina looks for on a role's screen: the words and shapes Claude Code and the
// Codex bridge draw that mean something — a usage limit, a tool call, a trust question, a dialog's footer. One table per
// engine is embedded (rules/<engine>.yaml); every row says what it sets, how it matches and where it reads. The code that
// reads a screen is a small matcher over these rows; what is procedural (the input line's colours, a dialog's geometry)
// stays in Go under a row of kind builtin that names it.
//
// A crew may put a screen-rules.yaml next to its crew file to add rows, or to disable or replace an embedded one by id
// (LoadOverrides): a CLI release that changes a wording is mended there before zmina ships a build. The package reads no
// crew and no state, and touches nothing but the file it is given.
package screenrules

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// The engines a table is written for: the CLI whose screen its rows describe.
const (
	Claude = "claude"
	Codex  = "codex"
)

// The kinds of a row. A match row is evaluated by the matcher; a builtin row names logic that stays in Go and has no
// matcher: it is listed so that every way zmina reads a screen has an id, and it cannot be disabled or replaced.
const (
	KindMatch   = "match"
	KindBuiltin = "builtin"
)

// Scopes: a row tests one line of its region at a time, or the region's text as a whole.
const (
	ScopeLine = "line"
	ScopeText = "text"
)

//go:embed rules/*.yaml
var embedded embed.FS

// The bounds on a row. A crew's file is read by every command and every beat of the ticker, over every pane, so what a row
// may cost is bounded where it is read: a regexp's source and the program it unrolls to, the number of rows in the file, how
// far up the screen a row reads. The embedded rows are far inside them (TestEmbeddedRowsAreWellInsideTheBounds).
const (
	// maxRegexpSource is the longest regexp or literal of a row, in bytes.
	maxRegexpSource = 512
	// maxProgram is the most instructions a regexp may compile to, repeats unrolled.
	maxProgram = 1000
	// maxOverrideRows is the most rows an override file lists.
	maxOverrideRows = 64
	// MaxLastLines is the most rows from the bottom a row may read: the capture both the ticker's watch and zmina explain take
	// of a screen. A row that asked for more would be read differently by the two.
	MaxLastLines = 80

	maxID, maxMeans, maxNote, maxVersion, maxSkipPrefix = 64, 512, 1024, 128, 64
)

// idForm is what an id looks like: it is printed in notes, doctor and zmina explain, and named by overrides.
var idForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Rule is one row of a table, as the file writes it.
type Rule struct {
	// ID names the row: engine.topic.what, unique over all tables. An override file refers to a row by it.
	ID string `yaml:"id" json:"id"`
	// Engine is the CLI whose screen the row describes: claude or codex.
	Engine string `yaml:"engine" json:"engine"`
	// Kind is match (the default) or builtin.
	Kind string `yaml:"kind,omitempty" json:"kind"`
	// Sets is the flag the row raises when it matches (Effects names them); empty for a builtin.
	Sets string `yaml:"sets,omitempty" json:"sets,omitempty"`
	// Means says in words what a match shows.
	Means string `yaml:"means" json:"means"`
	// Match is how the row matches.
	Match Match `yaml:"match,omitempty" json:"match"`
	// Region is where on the screen it reads; the whole pane when empty.
	Region Region `yaml:"region,omitempty" json:"region"`
	// Scope is line (the default) or text.
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty"`
	// Priority orders the rows that set one flag: the higher is tried first, rows of one priority in file order.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Since and Until say, as free text, from which CLI release the screen reads this way and until which; empty is
	// no known bound.
	Since string `yaml:"since,omitempty" json:"since,omitempty"`
	Until string `yaml:"until,omitempty" json:"until,omitempty"`
	// Note is anything else worth knowing about the row.
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
	// Disabled, in an override file, takes the embedded row of that id away.
	Disabled bool `yaml:"disabled,omitempty" json:"-"`
}

// Match says how a row tests a text: exactly one of Regexp, Contains and Prefix.
type Match struct {
	// Regexp is a Go regular expression, tried on the text; its groups are returned to the reader.
	Regexp string `yaml:"regexp,omitempty" json:"regexp,omitempty"`
	// Contains is a literal substring.
	Contains string `yaml:"contains,omitempty" json:"contains,omitempty"`
	// Prefix is a literal start.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	// Trim takes the white space off both ends of the text before it is tested.
	Trim bool `yaml:"trim,omitempty" json:"trim,omitempty"`
	// Lower lower-cases the text first; a literal is lower-cased with it.
	Lower bool `yaml:"lower,omitempty" json:"lower,omitempty"`
	// SkipPrefix makes a text that starts with it (after Trim) never match.
	SkipPrefix string `yaml:"skip_prefix,omitempty" json:"skip_prefix,omitempty"`
}

// Region is the part of the screen a row reads. LastLines is how many rows from the bottom (the reader captures that
// many); InputBox is the rows of Claude Code's input box (from the ❯ row under a rule to the next rule). Both empty:
// every row the reader gave.
type Region struct {
	LastLines int  `yaml:"last_lines,omitempty" json:"last_lines,omitempty"`
	InputBox  bool `yaml:"input_box,omitempty" json:"input_box,omitempty"`
}

// compiled is a row ready to match: the literals as they are compared (lower-cased with the text when the row says so).
type compiled struct {
	Rule
	re               *regexp.Regexp
	contains, prefix string
}

// Hit is a row that matched.
type Hit struct {
	Rule Rule
	// Line is the row of the screen that matched, as it stands there (a text-scope row: the text it matched);
	// N is its index among the screen's rows, -1 for a text-scope row.
	Line string
	N    int
	// Sub are the regexp's match and groups (Sub[0] the whole match); nil for a literal.
	Sub []string
	// Rest is what follows a prefix, in the text as the row saw it (after Trim and Lower).
	Rest string
}

// Table is a set of rows, from the embedded files and perhaps an override. It is read only.
type Table struct {
	rules []*compiled
	byID  map[string]*compiled
	// by is the match rows of an engine and effect, in the order they are tried.
	by map[string][]*compiled
}

type effectInfo struct {
	doc string
	// groups is the fewest groups a regexp row of the effect must have: the reader indexes them.
	groups int
	// codex is that the Codex readers read the effect too; every other effect is read only for a Claude screen.
	codex bool
	// text is that the reader tests the effect on the region's text as a whole, not a row at a time: the only scope a row of
	// it may name.
	text bool
	// lastLines and inputBox are the regions the reader honours: how much of the bottom it captures, and the input box. A
	// row names no region its reader ignores.
	lastLines, inputBox bool
}

// Effect names: what a row sets. Each is read by one piece of code, named in its doc.
const (
	UsageLimit    = "usage_limit"
	SayMark       = "say_mark"
	ToolCall      = "tool_call"
	SayCont       = "say_cont"
	SayStop       = "say_stop"
	TrustQuestion = "trust_question"
	TrustOption   = "trust_option"
	TrustSelected = "trust_selected"
	TrustYes      = "trust_yes"
	ExitDialog    = "exit_dialog"
	DialogOption  = "dialog_option"
	DialogFooter  = "dialog_footer"
	DialogFree    = "dialog_free_text"
	DialogTitle   = "dialog_title"
	FooterHint    = "footer_hint"
	FooterOption  = "footer_option"
	FooterModel   = "footer_model"
	FooterPath    = "footer_path"
	ChatRow       = "chat_dialog_row"
	ChatFrame     = "chat_dialog_frame"
)

var effects = map[string]effectInfo{
	UsageLimit: {doc: "a line of the screen's bottom that tells a usage limit stops the role (the ticker's usage-limit note)",
		codex: true, lastLines: true, inputBox: true},
	SayMark:  {doc: "the start of a line the role says in words (NowLine); Rest is what follows"},
	ToolCall: {doc: "the words after a say mark that are a tool call, not words (NowLine skips such a line)"},
	SayCont:  {doc: "a row wrapped under a say line (NowLine joins it), tested as it stands on the screen"},
	SayStop:  {doc: "a row that ends the rows wrapped under a say line (NowLine)"},
	TrustQuestion: {doc: "the screen asks whether to trust the folder (the answer to it is pressed for a worktree zmina made)",
		text: true, lastLines: true},
	TrustOption:   {doc: "an answer of the trust question"},
	TrustSelected: {doc: "the answer the cursor stands on"},
	TrustYes:      {doc: "the answer that says yes (tested lower-cased)"},
	ExitDialog: {doc: "the exit question's answer that stops the background work; group 1 is its key", groups: 1, text: true,
		lastLines: true},
	DialogOption: {doc: "a numbered choice of a dialog; groups: 1 the cursor, 2 the number, 3 the words", groups: 3},
	DialogFooter: {doc: "the footer line a live dialog ends with"},
	DialogFree:   {doc: "a choice that asks for words, not a pick (the terminal answers it)"},
	DialogTitle:  {doc: "the title mark of a dialog's question"},
	FooterHint:   {doc: "a hint Claude Code draws under its input box (peek cuts the footer)"},
	FooterOption: {doc: "a line that reads as a dialog's option (peek keeps a dialog whole)"},
	FooterModel:  {doc: "a status line's model name in brackets"},
	FooterPath:   {doc: "the start of a status line's path"},
	ChatRow: {doc: "a numbered row of the question dialog, for answering it from the browser chat; groups: 1 cursor, 2 number, 3 tick, 4 words",
		groups: 4},
	ChatFrame: {doc: "the frame a dialog's question is drawn in; removed before its words are read"},
}

// The ids of the rows of kind builtin: the logic that stays in Go, each named where it runs.
const (
	// BuiltinInputLine is internal/tmux's reading of Claude Code's input line (a ❯ row under a rule, the draft and the
	// dim suggestion told by the colours of a capture).
	BuiltinInputLine = "claude.input.line"
	// BuiltinBridgeMode is the Codex bridge's own mode marker, which internal/tmux reads as the input line's presence.
	BuiltinBridgeMode = "codex.bridge.mode"
	// BuiltinNowLine is shift.NowLine: the last words the role said, with the rows wrapped under them.
	BuiltinNowLine = "claude.now.line"
	// BuiltinDialogParse is shift.ParseDialog: the last numbered list and the words above it, geometry and all.
	BuiltinDialogParse = "claude.dialog.parse"
	// BuiltinTrustAnswer is the keys that move the cursor to the "yes" answer of the trust question.
	BuiltinTrustAnswer = "claude.trust.answer"
	// BuiltinPeekFooter is peek's cut of the footer under the input box (footerAt), read from the bottom up.
	BuiltinPeekFooter = "claude.peek.footer"
	// BuiltinChatDialog is the browser chat's reading of the active question dialog (activeDialog).
	BuiltinChatDialog = "claude.chat.active-dialog"
	// BuiltinExcerpt is the few lines a "no input line" note quotes, frames and rules left out.
	BuiltinExcerpt = "claude.excerpt"
)

// Effects lists the flags a row may set.
func Effects() []string {
	out := make([]string, 0, len(effects))
	for name := range effects {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}

// EffectDoc says what a flag is read for; empty for a name that is none.
func EffectDoc(effect string) string { return effects[effect].doc }

// fileDoc is one table file: the rows under rules.
type fileDoc struct {
	Rules []Rule `yaml:"rules"`
}

// decode reads a table file strictly: a field it does not know is an error, so that a misspelled one never passes for
// a row that did what it was meant to.
func decode(name string, data []byte) ([]Rule, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var doc fileDoc
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}

		return nil, fmt.Errorf("%s: %w", name, err)
	}

	return doc.Rules, nil
}

// compile checks a row and makes it ready to match; the error names the row's id.
func compile(r Rule) (*compiled, error) {
	fail := func(format string, args ...any) (*compiled, error) {
		return nil, fmt.Errorf("rule %q: %s", r.ID, fmt.Sprintf(format, args...))
	}

	if r.ID == "" {
		return fail("no id")
	}

	if !idForm.MatchString(r.ID) {
		return fail("an id is letters, digits, dots, dashes and underscores, at most %d characters, starting with a letter or digit", maxID)
	}

	switch r.Engine {
	case Claude, Codex:
	default:
		return fail("engine is %s or %s, not %q", Claude, Codex, r.Engine)
	}

	if r.Means == "" {
		return fail("no means: say in words what it shows")
	}

	for _, f := range []struct {
		name, text string
		most       int
	}{{"means", r.Means, maxMeans}, {"note", r.Note, maxNote}, {"since", r.Since, maxVersion}, {"until", r.Until, maxVersion}} {
		if len(f.text) > f.most {
			return fail("%s is longer than %d bytes", f.name, f.most)
		}
	}

	if r.Kind == "" {
		r.Kind = KindMatch
	}

	switch r.Kind {
	case KindBuiltin:
		if r.Sets != "" || r.Match != (Match{}) || r.Region != (Region{}) || r.Scope != "" {
			return fail("a builtin row names logic that stays in Go: it has no sets, match, region or scope")
		}

		return &compiled{Rule: r}, nil
	case KindMatch:
	default:
		return fail("kind is %s or %s, not %q", KindMatch, KindBuiltin, r.Kind)
	}

	info, ok := effects[r.Sets]
	if !ok {
		return fail("sets %q is no flag zmina reads (%s)", r.Sets, strings.Join(Effects(), ", "))
	}

	if r.Engine == Codex && !info.codex {
		return fail("%s is read only for a Claude screen: a row of the codex table that sets it would never be evaluated", r.Sets)
	}

	// The scope a reader tests an effect at is the effect's: a row may say it, never choose another.
	want := ScopeLine
	if info.text {
		want = ScopeText
	}

	if r.Scope == "" {
		r.Scope = want
	}

	if r.Scope != want {
		return fail("scope %q: a row that sets %s is tested %s at a time, no reader tests it otherwise", r.Scope, r.Sets,
			map[string]string{ScopeLine: "a line", ScopeText: "as a whole text"}[want])
	}

	if r.Region.LastLines < 0 {
		return fail("region.last_lines is negative")
	}

	if r.Region.LastLines > MaxLastLines {
		return fail("region.last_lines %d is more than the %d rows the ticker and zmina explain both capture", r.Region.LastLines,
			MaxLastLines)
	}

	if r.Region.InputBox && r.Region.LastLines > 0 {
		return fail("region has last_lines and input_box: pick one")
	}

	if r.Region.LastLines > 0 && !info.lastLines {
		return fail("region.last_lines: no reader of %s takes a count of rows; it would be ignored", r.Sets)
	}

	if r.Region.InputBox && !info.inputBox {
		return fail("region.input_box: no reader of %s reads the input box; it would be ignored", r.Sets)
	}

	m := r.Match

	set := 0
	for _, s := range []string{m.Regexp, m.Contains, m.Prefix} {
		if s != "" {
			set++
		}
	}

	if set != 1 {
		return fail("match has exactly one of regexp, contains and prefix")
	}

	for _, lit := range []struct{ name, text string }{{"contains", m.Contains}, {"prefix", m.Prefix}, {"regexp", m.Regexp}} {
		if len(lit.text) > maxRegexpSource {
			return fail("match.%s is longer than %d bytes", lit.name, maxRegexpSource)
		}
	}

	if len(m.SkipPrefix) > maxSkipPrefix {
		return fail("match.skip_prefix is longer than %d bytes", maxSkipPrefix)
	}

	c := &compiled{Rule: r, contains: m.Contains, prefix: m.Prefix}

	if m.Lower {
		c.contains, c.prefix = strings.ToLower(m.Contains), strings.ToLower(m.Prefix)
	}

	if m.Regexp != "" {
		// The program it unrolls to is measured before it is compiled: the matcher runs it over every row of every pane at every
		// beat, and a pattern such as (?:a?){1000} is a program of thousands of instructions out of a dozen bytes.
		n, err := programSize(m.Regexp)
		if err != nil {
			return fail("bad regexp: %v", err)
		}

		if n > maxProgram {
			return fail("the regexp unrolls to a program of %d instructions, more than the %d a row may have", n, maxProgram)
		}

		re, err := regexp.Compile(m.Regexp)
		if err != nil {
			return fail("bad regexp: %v", err)
		}

		if re.NumSubexp() < info.groups {
			return fail("the regexp has %d groups; a row that sets %s needs at least %d (%s)", re.NumSubexp(), r.Sets,
				info.groups, info.doc)
		}

		c.re = re
	} else if info.groups > 0 {
		return fail("a row that sets %s is a regexp with at least %d groups (%s)", r.Sets, info.groups, info.doc)
	}

	return c, nil
}

// build makes a table of rows in the order given, which breaks ties of priority.
func build(rows []Rule) (*Table, error) {
	t := &Table{byID: map[string]*compiled{}, by: map[string][]*compiled{}}

	for _, r := range rows {
		c, err := compile(r)
		if err != nil {
			return nil, err
		}

		if _, dup := t.byID[c.ID]; dup {
			return nil, fmt.Errorf("rule %q: the id is used twice", c.ID)
		}

		t.byID[c.ID] = c
		t.rules = append(t.rules, c)
	}

	for _, c := range t.rules {
		if c.Kind != KindMatch {
			continue
		}

		key := c.Engine + "/" + c.Sets
		t.by[key] = append(t.by[key], c)
	}

	for _, rows := range t.by {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Priority > rows[j].Priority })
	}

	return t, nil
}

// Load reads the embedded tables. Any row that does not read, or a file that does not, is an error that names it.
func Load() (*Table, error) {
	rows, err := embeddedRows()
	if err != nil {
		return nil, err
	}

	return build(rows)
}

func embeddedRows() ([]Rule, error) {
	files, err := embedded.ReadDir("rules")
	if err != nil {
		return nil, err
	}

	var rows []Rule

	for _, f := range files {
		data, err := embedded.ReadFile("rules/" + f.Name())
		if err != nil {
			return nil, err
		}

		rs, err := decode("rules/"+f.Name(), data)
		if err != nil {
			return nil, err
		}

		want := strings.TrimSuffix(f.Name(), ".yaml")
		for _, r := range rs {
			if r.Disabled {
				return nil, fmt.Errorf("rule %q: disabled belongs in an override file", r.ID)
			}

			if r.Engine != want {
				return nil, fmt.Errorf("rule %q is in rules/%s but is for engine %q", r.ID, f.Name(), r.Engine)
			}
		}

		rows = append(rows, rs...)
	}

	return rows, nil
}

var (
	defaultOnce  sync.Once
	defaultTable *Table
)

// Default is the embedded table, read once and shared: it is never changed. A crew's own table (LoadOverrides) is a new table
// that each shift carries, so that one process serving several crews never reads a pane by another crew's rows. An embedded
// table that does not read is a bug of the build, caught by this package's tests, and stops the program with the row's id
// rather than reading screens by a half table.
func Default() *Table {
	defaultOnce.Do(func() {
		t, err := Load()
		if err != nil {
			panic("zmina: the embedded screen rules do not read: " + err.Error())
		}

		defaultTable = t
	})

	return defaultTable
}

// Rules lists every row, builtins too, in the order of the files.
func (t *Table) Rules() []Rule {
	out := make([]Rule, 0, len(t.rules))
	for _, c := range t.rules {
		out = append(out, c.Rule)
	}

	return out
}

// Rule is the row of an id.
func (t *Table) Rule(id string) (Rule, bool) {
	c, ok := t.byID[id]
	if !ok {
		return Rule{}, false
	}

	return c.Rule, true
}

// Builtins lists the rows that name logic kept in Go.
func (t *Table) Builtins() []Rule {
	var out []Rule

	for _, c := range t.rules {
		if c.Kind == KindBuiltin {
			out = append(out, c.Rule)
		}
	}

	return out
}

// rows are the match rows of an engine that set an effect, in the order they are tried.
func (t *Table) rows(engine, effect string) []*compiled { return t.by[engine+"/"+effect] }

// Has reports whether a row of the engine sets the effect.
func (t *Table) Has(engine, effect string) bool { return len(t.rows(engine, effect)) > 0 }

// Window is how many rows from the bottom the readers of an effect capture: the most any of its rows reads; def when
// none names a count (the rows read what the reader captured).
func (t *Table) Window(engine, effect string, def int) int {
	most := 0
	for _, c := range t.rows(engine, effect) {
		most = max(most, c.Region.LastLines)
	}

	if most == 0 {
		return def
	}

	return most
}

// test tries the row on a text; the hit keeps the text as it was given.
func (c *compiled) test(raw string) (Hit, bool) {
	m, text := c.Match, raw

	if m.Trim {
		text = strings.TrimSpace(text)
	}

	if m.Lower {
		text = strings.ToLower(text)
	}

	if m.SkipPrefix != "" && strings.HasPrefix(text, m.SkipPrefix) {
		return Hit{}, false
	}

	switch {
	case c.re != nil:
		sub := c.re.FindStringSubmatch(text)
		if sub == nil {
			return Hit{}, false
		}

		return Hit{Rule: c.Rule, Line: raw, N: -1, Sub: sub}, true
	case c.contains != "":
		if !strings.Contains(text, c.contains) {
			return Hit{}, false
		}

		return Hit{Rule: c.Rule, Line: raw, N: -1}, true
	default:
		rest, ok := strings.CutPrefix(text, c.prefix)
		if !ok {
			return Hit{}, false
		}

		return Hit{Rule: c.Rule, Line: raw, N: -1, Rest: rest}, true
	}
}

// Test tries the rows of an effect, in priority order, on one text the reader cut for them, and returns the first that
// matches. The row's region and scope are the reader's business here: the text is tested as it is given.
func (t *Table) Test(engine, effect, text string) (Hit, bool) {
	for _, c := range t.rows(engine, effect) {
		if h, ok := c.test(text); ok {
			return h, true
		}
	}

	return Hit{}, false
}

// Strip removes from a text what each regexp row of the effect matches, in priority order: the frame a dialog is drawn in.
func (t *Table) Strip(engine, effect, text string) string {
	for _, c := range t.rows(engine, effect) {
		if c.re != nil {
			text = c.re.ReplaceAllString(text, "")
		}
	}

	return text
}

// Tail is the last n rows of a screen; the screen as it is when n is not positive or it has no more.
func Tail(screen string, n int) string {
	if n <= 0 {
		return screen
	}

	lines := strings.Split(screen, "\n")
	if len(lines) <= n {
		return screen
	}

	return strings.Join(lines[len(lines)-n:], "\n")
}

// Scan reads a whole screen with the rows of an effect, each in its own region and scope, and returns what matched in
// the order of the screen: a line-scope row once for each row it matches (the first row of the effect that matches it),
// a text-scope row once, last.
func (t *Table) Scan(engine, effect, screen string) []Hit {
	lines := strings.Split(screen, "\n")

	var (
		hits  []Hit
		texts []Hit
		seen  = map[int]bool{}
	)

	type span struct{ from, to int }

	region := func(c *compiled) []span {
		switch {
		case c.Region.InputBox:
			if from, to, ok := inputBox(lines); ok {
				return []span{{from, to}}
			}

			return nil
		case c.Region.LastLines > 0:
			return []span{{max(0, len(lines)-c.Region.LastLines), len(lines)}}
		default:
			return []span{{0, len(lines)}}
		}
	}

	for _, c := range t.rows(engine, effect) {
		for _, sp := range region(c) {
			if c.Scope == ScopeText {
				if h, ok := c.test(strings.Join(lines[sp.from:sp.to], "\n")); ok {
					h.Line = h.matched()
					texts = append(texts, h)
				}

				continue
			}

			for i := sp.from; i < sp.to; i++ {
				if seen[i] {
					continue
				}

				if h, ok := c.test(lines[i]); ok {
					h.Line, h.N = lines[i], i
					hits = append(hits, h)
					seen[i] = true
				}
			}
		}
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].N < hits[j].N })

	return append(hits, texts...)
}

// matched is what a text-scope row matched: the regexp's match, else the row's literal.
func (h Hit) matched() string {
	switch {
	case len(h.Sub) > 0:
		return h.Sub[0]
	case h.Rule.Match.Contains != "":
		return h.Rule.Match.Contains
	default:
		return h.Rule.Match.Prefix
	}
}

// The input box is Claude Code's: a ❯ row right under a rule, and what follows up to the next rule. The same shape
// internal/tmux reads colours from; here it is only a region.
const (
	promptGlyph = "❯"
	rulePrefix  = "───"
)

// inputBox finds the last input box of a screen: the index of its ❯ row and the one past its last row.
func inputBox(lines []string) (from, to int, ok bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), promptGlyph) || !underRule(lines[:i]) {
			continue
		}

		to = len(lines)

		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), rulePrefix) {
				to = j

				break
			}
		}

		return i, to, true
	}

	return 0, 0, false
}

func underRule(above []string) bool {
	for i := len(above) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(above[i]); l != "" {
			return strings.HasPrefix(l, rulePrefix)
		}
	}

	return false
}

// IDs lists the ids of the rows, sorted.
func (t *Table) IDs() []string {
	ids := make([]string, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	return ids
}
