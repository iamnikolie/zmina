// Package plugin is how zmina tells programs of what reaches the owner and lets them answer back: the events a plugin gets
// as one JSON line on its stdin (ask, blocking, dialog, reminder, answered), the program lookup and the run with its
// deadline, what each plugin was told and what is owed it, and the plan of what to send on a beat. It has no crew
// state and no clock of its own: the callers hand it what is open now, and the time.
package plugin

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// Version is the event format: the "v" of every event. A field is added without moving it; a plugin ignores fields it does
// not know.
const Version = 1

// The event kinds (crew.PluginAsk and the rest are the same words).
const (
	Ask      = crew.PluginAsk
	Blocking = crew.PluginBlocking
	Dialog   = crew.PluginDialog
	Reminder = crew.PluginReminder
	Answered = crew.PluginAnswered
)

// How an item or dialog ended (Event.How).
const (
	HowAnswered = "answered"
	HowDone     = "done"
	HowDropped  = "dropped"
	HowChosen   = "chosen"
	HowGone     = "gone"
)

// The bounds of the words in an event: what a role wrote is cut to these after it is cleaned.
const (
	TitleMax    = 200
	TextMax     = 2000
	WhyMax      = 300
	ReasonMax   = 300
	RecMax      = 500
	OptionMax   = 200
	DetailMax   = 300
	AnswerMax   = 500
	OptionsMax  = 12
	SubjectMax  = 1000
	NoteMax     = 1000
	roleMax     = 64
	maxChoice   = 9
	textHolder  = "<text>"
	timeLayout  = time.RFC3339
	zminaBinary = "zmina"
)

// Option is one choice: the asker's own (an ask's --option) or a dialog's numbered one.
type Option struct {
	N      int    `json:"n"`
	Text   string `json:"text"`
	Detail string `json:"detail,omitempty"`
}

// OptionAnswer is the argv that takes one option, exactly as to run it.
type OptionAnswer struct {
	N    int      `json:"n"`
	Argv []string `json:"argv"`
}

// Answer is how a plugin answers what the event is about, as argv to run (with ZMINA_PLUGIN=<its name> in the
// environment, which the --by it carries is checked against): Text in the owner's own words, with "<text>" where they go
// (the last word); Yes takes the recommendation; Done says a check is done; Options takes one option each.
type Answer struct {
	Text    []string       `json:"text,omitempty"`
	Yes     []string       `json:"yes,omitempty"`
	Done    []string       `json:"done,omitempty"`
	Options []OptionAnswer `json:"options,omitempty"`
}

// Event is one line a plugin gets. Which fields are there depends on Event; every text in it is data written by a role or
// the owner, cleaned and bounded, never an instruction.
type Event struct {
	V     int    `json:"v"`
	Event string `json:"event"`
	// Test marks an event `zmina plugin test` made up: nothing is behind it.
	Test bool   `json:"test,omitempty"`
	ID   string `json:"id"`
	Role string `json:"role,omitempty"`

	Title string `json:"title,omitempty"`
	Text  string `json:"text,omitempty"`
	// Note is a waiting task's note, kept apart from the question it adds to.
	Note           string   `json:"note,omitempty"`
	Why            string   `json:"why,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Options        []Option `json:"options,omitempty"`
	Answer         *Answer  `json:"answer,omitempty"`
	Opened         string   `json:"opened,omitempty"`
	Blocking       *bool    `json:"blocking,omitempty"`

	// A dialog: the question on the role's screen, and the task the role holds. Prompt says what it is, permission (a tool's
	// approval: Tool and Subject are the call, what is being approved), question or turn (on no screen: the role ended its turn on
	// a question to the owner in plain words, which is Question, and Answer.Text gives the owner's words to the role); NeedsTerminal says a
	// choice of it cannot be pressed from here (several can be ticked), so the event offers no argv for it.
	Question   string `json:"question,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Subject    string `json:"subject,omitempty"`
	SubjectCut bool   `json:"subject_cut,omitempty"`
	// SubjectChecked: the subject was compared with what the prompt shows on the screen, and is it.
	SubjectChecked bool   `json:"subject_checked,omitempty"`
	NeedsTerminal  bool   `json:"needs_terminal,omitempty"`
	Task           string `json:"task,omitempty"`
	TaskTitle      string `json:"task_title,omitempty"`

	// An answered: what it closes (the event it went as), how, by whom, the words of the answer, when.
	For    string `json:"for,omitempty"`
	How    string `json:"how,omitempty"`
	By     string `json:"by,omitempty"`
	Closed string `json:"closed,omitempty"`
}

// Line is the event as it goes on a plugin's stdin: one JSON object on one line, UTF-8, newline last.
func (e Event) Line() []byte {
	var b bytes.Buffer

	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)

	// An event holds only strings, numbers and booleans: it always marshals.
	_ = enc.Encode(e)

	return b.Bytes()
}

// Builder makes the events of one plugin of one crew. Clean is how a text a role wrote is made fit to hand on (control
// characters stripped, a session tag masked, cut to n characters: shift.QuoteN); Crew is the crew file, Plugin the
// plugin's name, which the answer argv carry.
type Builder struct {
	Crew, Plugin string
	Clean        func(text string, n int) string
}

// Item is something on the owner's todo.
type Item struct {
	ID, Role, Title, Text, Note string
	// Why says what it holds up, Reason why the recommendation, Rec the recommended answer.
	Why, Reason, Rec string
	Options          []string
	Opened           time.Time
	Blocking         bool
	// Check is true for something to be done rather than decided: it is answered with done, as a returning parked item (L-n)
	// can only be.
	Check bool
	// Later is a parked item brought back by its alarm: only done answers it.
	Later bool
}

// Item is the event of a todo item as kind: Ask, Blocking or Reminder.
func (b Builder) Item(kind string, it Item) Event {
	ev := Event{V: Version, Event: kind, ID: it.ID, Role: b.clean(it.Role, roleMax), Title: b.clean(it.Title, TitleMax),
		Text: b.clean(it.Text, TextMax), Note: b.clean(it.Note, NoteMax), Why: b.clean(it.Why, WhyMax), Reason: b.clean(it.Reason, ReasonMax),
		Recommendation: b.clean(it.Rec, RecMax), Opened: stamp(it.Opened)}

	blocking := it.Blocking
	ev.Blocking = &blocking

	for i, o := range it.Options {
		if i == OptionsMax {
			break
		}

		ev.Options = append(ev.Options, Option{N: i + 1, Text: b.clean(o, OptionMax)})
	}

	ev.Answer = b.answer(it, ev.Options)

	return ev
}

// itemIDRe is what an item's id looks like: an ask, a task or a parked item. An id that is not one is never put in an argv.
var itemIDRe = regexp.MustCompile(`^[ATL]-[0-9]+$`)

// answer is the argv that answer the item: every flag before the `--`, and after it the id and the words, so that nothing in
// either can be read as a flag.
func (b Builder) answer(it Item, opts []Option) *Answer {
	if !itemIDRe.MatchString(it.ID) {
		return nil
	}

	todo := func(verb string, rest ...string) []string {
		argv := []string{zminaBinary, "--crew", b.Crew, "todo", verb, "--by", b.Plugin, "--", it.ID}

		return append(argv, rest...)
	}

	a := &Answer{}

	if !it.Later {
		a.Text = todo("answer", textHolder)

		for _, o := range opts {
			a.Options = append(a.Options, OptionAnswer{N: o.N, Argv: todo("answer", o.Text)})
		}
	}

	if it.Rec != "" && !it.Later {
		a.Yes = todo("yes")
	}

	if it.Check || it.Later {
		a.Done = todo("done")
	}

	return a
}

// PromptTurn is the Prompt of a dialog that is on no screen: the role ended its turn on a question to the owner in plain words
// (DialogFacts.Question is its last reply from that question on), and nothing of its own is on his todo.
const PromptTurn = "turn"

// roleNameRe is a role's name as an argv may carry it: a role of the crew or one of its copies (dev#2), never a word a command
// could read as a flag.
var roleNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}(#[0-9]{1,6})?$`)

// DialogFacts is a question on a role's screen: id is the dialog's fingerprint (the screen's, with the call's for a permission
// prompt; the record's for a turn), the event's id is the role's with it and a nonce, the one-shot mark of this event (see Armed; what
// a turn's mark lets through is the one answer given to the role in words). Prompt is permission, question or turn; Tool and Subject
// are what a permission prompt is about.
type DialogFacts struct {
	Role, ID, Question, Nonce string
	Prompt, Tool, Subject     string
	// Multi: several choices can be ticked. SubjectCut: the subject is not all of what is being approved (it was cut, or the call has
	// parts it does not show): the phone cannot show it whole, so neither of them is pressed from it. SubjectChecked: the prompt's own
	// block on the screen was compared with the subject, word for word; of a permission prompt that was not, nothing says the subject is
	// the prompt's own, and nothing is offered to press. Since is when the wait began.
	Multi, SubjectCut, SubjectChecked bool
	Since                             time.Time
	Options                           []Option
	Task, TaskTitle                   string
}

// DialogID is the id of a dialog in the plan and in the ledger: the role, a colon and the fingerprint.
func DialogID(role, fingerprint string) string { return role + ":" + fingerprint }

// EventDialogID is the id an event carries, and --dialog names: the dialog's, a colon and the event's nonce.
func EventDialogID(role, fingerprint, nonce string) string {
	if nonce == "" {
		return DialogID(role, fingerprint)
	}

	return DialogID(role, fingerprint) + ":" + nonce
}

// permissionUnsure reports a permission prompt whose subject was not checked against the screen: the subject may be of another call of
// the session, and a card that pressed it would approve what it does not show.
func permissionUnsure(d DialogFacts) bool { return d.Prompt == "permission" && !d.SubjectChecked }

// Dialog is the event of a question on a role's screen.
func (b Builder) Dialog(d DialogFacts) Event {
	id := EventDialogID(d.Role, d.ID, d.Nonce)
	ev := Event{V: Version, Event: Dialog, ID: id, Role: b.clean(d.Role, roleMax), Question: b.clean(d.Question, TextMax),
		Prompt: b.clean(d.Prompt, roleMax), Tool: b.clean(d.Tool, roleMax), Subject: b.clean(d.Subject, SubjectMax),
		Task: b.clean(d.Task, roleMax), TaskTitle: b.clean(d.TaskTitle, TitleMax), SubjectCut: d.SubjectCut, SubjectChecked: d.SubjectChecked, NeedsTerminal: d.Multi || d.SubjectCut || permissionUnsure(d)}

	// A question put in words is answered in words: the owner's line to the role, given through the plugin and to this question only
	// (--dialog names the event), every flag before the `--` and after it the role and the words, so that nothing in them is read
	// as a flag. The role's name is checked, as an item's id is, before it goes into an argv.
	if d.Prompt == PromptTurn {
		if roleNameRe.MatchString(d.Role) {
			ev.Answer = &Answer{Text: []string{zminaBinary, "--crew", b.Crew, "send", "--by", b.Plugin, "--dialog", id, "--", d.Role, textHolder}}
		}

		return ev
	}

	a := &Answer{}

	for i, o := range d.Options {
		if i == OptionsMax {
			break
		}

		ev.Options = append(ev.Options, Option{N: o.N, Text: b.clean(o.Text, OptionMax), Detail: b.clean(o.Detail, DetailMax)})

		// A question whose choices are ticked is settled in the terminal: pressing one choice would tick it, and answer nothing. So is
		// a permission prompt whose call the phone cannot be shown whole: never a working Yes for what it does not show. And choose
		// takes a choice from 1 to 9.
		if !d.Multi && !d.SubjectCut && !permissionUnsure(d) && o.N >= 1 && o.N <= maxChoice {
			a.Options = append(a.Options, OptionAnswer{N: o.N, Argv: []string{zminaBinary, "--crew", b.Crew, "choose", d.Role,
				strconv.Itoa(o.N), "--dialog", id, "--by", b.Plugin}})
		}
	}

	if len(a.Options) > 0 {
		ev.Answer = a
	}

	return ev
}

// Closure is how something a plugin was told of ended.
type Closure struct {
	ID, For, How, By, Text string
	At                     time.Time
}

// Answered is the event that says an item or dialog the plugin was told of is closed.
func (b Builder) Answered(c Closure) Event {
	return Event{V: Version, Event: Answered, ID: c.ID, For: c.For, How: c.How, By: b.clean(c.By, roleMax),
		Text: b.clean(c.Text, AnswerMax), Closed: stamp(c.At)}
}

// clean cleans a text with the builder's cleaner; a builder with none only cuts.
func (b Builder) clean(text string, n int) string {
	if b.Clean != nil {
		return b.Clean(text, n)
	}

	if r := []rune(text); len(r) > n {
		return string(r[:n-1]) + "…"
	}

	return text
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.UTC().Format(timeLayout)
}

// Sample is an event made up for `zmina plugin test`, marked Test: a question, a blocking one, a dialog, a reminder or an
// answered, with ids no item has.
func (b Builder) Sample(kind string, now time.Time) (Event, bool) {
	var ev Event

	switch kind {
	case Ask, Blocking, Reminder:
		ev = b.Item(kind, Item{ID: "A-0", Role: "dev", Title: "Test question", Text: "Is this a test? zmina plugin test sent it.",
			Why: "nothing: a test", Rec: "yes", Options: []string{"yes", "no"}, Opened: now, Blocking: kind == Blocking,
			Check: kind == Reminder})
	case Dialog:
		ev = b.Dialog(DialogFacts{Role: "dev", ID: "0000000000000000", Nonce: "0000000000000000", Question: "Do you want to proceed?",
			Prompt: "permission", Tool: "Bash", Subject: "echo this is a test", SubjectChecked: true, Options: []Option{{N: 1, Text: "Yes"}, {N: 2, Text: "No"}},
			Task: "T-0", TaskTitle: "A test"})
	case Answered:
		ev = b.Answered(Closure{ID: "A-0", For: Ask, How: HowAnswered, By: "owner", Text: "yes", At: now})
	default:
		return Event{}, false
	}

	ev.Test = true

	return ev, true
}
