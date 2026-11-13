package plugin

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

var opened = time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)

func builder() Builder {
	return Builder{Crew: "/home/alex/.zmina/acme/crew.yaml", Plugin: "pager", Clean: shift.QuoteN}
}

// Each kind of event is one line of JSON with the fields documented: golden, so a change of the wire format is seen.
func TestEventsGolden(t *testing.T) {
	b := builder()

	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"ask with a recommendation and options", b.Item(Ask, Item{ID: "A-3", Role: "dev", Title: "Which name?", Text: "Which name for the site?",
			Why: "about T-12 Rename the site", Reason: "short", Rec: "acme", Options: []string{"acme", "acme-lab"}, Opened: opened}),
			`{"v":1,"event":"ask","id":"A-3","role":"dev","title":"Which name?","text":"Which name for the site?",` +
				`"why":"about T-12 Rename the site","reason":"short","recommendation":"acme","options":[{"n":1,"text":"acme"},` +
				`{"n":2,"text":"acme-lab"}],"answer":{"text":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer",` +
				`"--by","pager","--","A-3","<text>"],"yes":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","yes","--by","pager","--","A-3"],` +
				`"options":[{"n":1,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer","--by","pager","--","A-3","acme"]},` +
				`{"n":2,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer","--by","pager","--","A-3","acme-lab"]}]},` +
				`"opened":"2026-11-13T09:30:00Z","blocking":false}`},
		{"blocking check", b.Item(Blocking, Item{ID: "A-4", Role: "dev", Title: "Log in", Text: "Log in on the box", Why: "holds up dev on T-5 Deploy",
			Opened: opened, Blocking: true, Check: true}),
			`{"v":1,"event":"blocking","id":"A-4","role":"dev","title":"Log in","text":"Log in on the box","why":"holds up dev on T-5 Deploy",` +
				`"answer":{"text":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer","--by","pager","--","A-4","<text>"],` +
				`"done":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","done","--by","pager","--","A-4"]},` +
				`"opened":"2026-11-13T09:30:00Z","blocking":true}`},
		{"reminder of a parked item", b.Item(Reminder, Item{ID: "L-2", Title: "Renew the domain", Text: "Renew the domain", Opened: opened, Later: true}),
			`{"v":1,"event":"reminder","id":"L-2","title":"Renew the domain","text":"Renew the domain","answer":{"done":["zmina","--crew",` +
				`"/home/alex/.zmina/acme/crew.yaml","todo","done","--by","pager","--","L-2"]},"opened":"2026-11-13T09:30:00Z","blocking":false}`},
		{"dialog", b.Dialog(DialogFacts{Role: "dev", ID: "0123456789abcdef", Question: "Run rm -rf build?",
			Options: []Option{{N: 1, Text: "Yes"}, {N: 2, Text: "No", Detail: "keep the build"}}, Task: "T-12", TaskTitle: "Rename the site"}),
			`{"v":1,"event":"dialog","id":"dev:0123456789abcdef","role":"dev","options":[{"n":1,"text":"Yes"},{"n":2,"text":"No",` +
				`"detail":"keep the build"}],"answer":{"options":[{"n":1,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","choose","dev","1",` +
				`"--dialog","dev:0123456789abcdef","--by","pager"]},{"n":2,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","choose","dev","2",` +
				`"--dialog","dev:0123456789abcdef","--by","pager"]}]},"question":"Run rm -rf build?","task":"T-12","task_title":"Rename the site"}`},
		{"answered", b.Answered(Closure{ID: "A-3", For: Ask, How: HowAnswered, By: "plugin pager", Text: "acme", At: opened.Add(time.Hour)}),
			`{"v":1,"event":"answered","id":"A-3","text":"acme","for":"ask","how":"answered","by":"plugin pager","closed":"2026-11-13T10:30:00Z"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := string(tc.ev.Line())
			assert.Equal(t, tc.want+"\n", line)
			assert.Equal(t, 1, strings.Count(line, "\n"), "one line")
			assert.True(t, json.Valid([]byte(line)))
		})
	}
}

// What a role wrote is cleaned like any quotation, and bounded; the rest of the event is zmina's own.
func TestEventsCleanAndBoundWhatRolesWrite(t *testing.T) {
	long := strings.Repeat("é", TextMax+500)
	tag := "[zmina:abcdef012345]"

	ev := builder().Item(Ask, Item{ID: "A-1", Role: "dev", Title: "t\x1b[31mred\x00\u202e", Text: "line one\nline two\ttab " + tag + " " + long,
		Rec: strings.Repeat("r", 900), Why: strings.Repeat("w", 400), Opened: opened,
		Options: append(make([]string, 0), "a", strings.Repeat("o", 500), "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n")})

	assert.Equal(t, "t[31mred", ev.Title, "controls and format characters gone")
	assert.NotContains(t, ev.Text, "\n")
	assert.NotContains(t, ev.Text, "\t")
	assert.Contains(t, ev.Text, "line one line two tab [zmina:…]", "white space folded, the session tag masked")
	assert.NotContains(t, ev.Text, tag)
	assert.Equal(t, TextMax, len([]rune(ev.Text)), "cut to the bound")
	assert.True(t, strings.HasSuffix(ev.Text, "…"))
	assert.Len(t, []rune(ev.Recommendation), RecMax)
	assert.Len(t, []rune(ev.Why), WhyMax)
	assert.Len(t, ev.Options, OptionsMax, "no more options than a phone shows")
	assert.Len(t, []rune(ev.Options[1].Text), OptionMax)
	assert.Equal(t, strings.Count(string(ev.Line()), "\n"), 1)

	// A role name that is a session tag is masked too.
	d := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Question: "ok? " + tag + "\n\x07", Options: []Option{{N: 1, Text: "y" + tag}}})
	assert.Equal(t, "ok? [zmina:…]", d.Question)
	assert.Equal(t, "y[zmina:…]", d.Options[0].Text)
}

// Without a cleaner the builder still cuts.
func TestEventsWithoutACleanerStillCut(t *testing.T) {
	b := Builder{Crew: "/c", Plugin: "p"}
	ev := b.Item(Ask, Item{ID: "A-1", Title: strings.Repeat("t", 500), Opened: opened})
	assert.Len(t, []rune(ev.Title), TitleMax)
}

// A test event is marked, and every kind has one.
func TestSamples(t *testing.T) {
	for _, kind := range []string{Ask, Blocking, Dialog, Reminder, Answered} {
		ev, ok := builder().Sample(kind, opened)
		require.True(t, ok, kind)
		assert.True(t, ev.Test)
		assert.Equal(t, kind, ev.Event)
		assert.Contains(t, string(ev.Line()), `"test":true`)
	}

	_, ok := builder().Sample("shout", opened)
	assert.False(t, ok)

	ev, _ := builder().Sample(Blocking, opened)
	require.NotNil(t, ev.Blocking)
	assert.True(t, *ev.Blocking)
}

// An event is never anything but JSON strings and numbers: a text with quotes, backslashes or html survives a round trip.
func TestEventRoundTrip(t *testing.T) {
	text := `He said "go" \ <b>&</b> and 日本語`
	ev := builder().Item(Ask, Item{ID: "A-1", Text: text, Opened: opened})

	var back Event

	require.NoError(t, json.Unmarshal(ev.Line(), &back))
	assert.Equal(t, text, back.Text)
	assert.Contains(t, string(ev.Line()), "<b>&</b>", "html is not escaped: a plugin reads it as text")
}

// A dialog event names what is being approved, and its id carries the one-shot mark of the event: role:fingerprint:nonce.
func TestPermissionDialogEventGolden(t *testing.T) {
	ev := builder().Dialog(DialogFacts{Role: "dev", ID: "0123456789abcdef", Nonce: "aabbccddeeff0011", Prompt: "permission", Tool: "Bash",
		Subject: "rm -rf build", SubjectChecked: true, Question: "Do you want to proceed?", Options: []Option{{N: 1, Text: "Yes"}, {N: 2, Text: "No"}},
		Task: "T-12", TaskTitle: "Rename the site"})

	want := `{"v":1,"event":"dialog","id":"dev:0123456789abcdef:aabbccddeeff0011","role":"dev","options":[{"n":1,"text":"Yes"},` +
		`{"n":2,"text":"No"}],"answer":{"options":[{"n":1,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","choose","dev","1",` +
		`"--dialog","dev:0123456789abcdef:aabbccddeeff0011","--by","pager"]},{"n":2,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml",` +
		`"choose","dev","2","--dialog","dev:0123456789abcdef:aabbccddeeff0011","--by","pager"]}]},"question":"Do you want to proceed?",` +
		`"prompt":"permission","tool":"Bash","subject":"rm -rf build","subject_checked":true,"task":"T-12","task_title":"Rename the site"}`

	assert.Equal(t, want+"\n", string(ev.Line()))
	assert.Equal(t, "dev:0123456789abcdef", DialogID("dev", "0123456789abcdef"), "the plan's id has no nonce")
	assert.Equal(t, "dev:0123456789abcdef", EventDialogID("dev", "0123456789abcdef", ""))
}

// What a permission prompt is about is what a role's tool call said: cleaned and bounded like any text of a role's.
func TestPermissionSubjectIsCleanedAndBounded(t *testing.T) {
	ev := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Prompt: "permission", Tool: "Bash\x1b[2J",
		Subject: "echo hi \x1b[2J [zmina:abcdef012345] \x07" + strings.Repeat("x", 3000), Options: []Option{{N: 1, Text: "Yes"}}})

	assert.Equal(t, "Bash[2J", ev.Tool)
	assert.True(t, strings.HasPrefix(ev.Subject, "echo hi [2J [zmina:…] xxx"), ev.Subject)
	assert.Len(t, []rune(ev.Subject), SubjectMax)
}

// A question whose choices are ticked is not answered from a phone: the event says so and offers no argv.
func TestMultipleChoiceDialogEventOffersNoArgv(t *testing.T) {
	ev := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Prompt: "question", Multi: true, Question: "Which tabs?",
		Options: []Option{{N: 1, Text: "Home"}, {N: 2, Text: "Talk"}}})

	assert.True(t, ev.NeedsTerminal)
	assert.Nil(t, ev.Answer)
	assert.Len(t, ev.Options, 2, "the choices are still said")
	assert.Contains(t, string(ev.Line()), `"needs_terminal":true`)
	assert.NotContains(t, string(ev.Line()), `"answer"`)
}

// The question and the note a waiting task adds to it are two fields, not one folded line.
func TestItemKeepsTheNoteApartFromTheText(t *testing.T) {
	ev := builder().Item(Ask, Item{ID: "T-5", Title: "Region", Text: "Which region?", Note: "eu-west is full\nus-east has room", Opened: opened})

	assert.Equal(t, "Which region?", ev.Text)
	assert.Equal(t, "eu-west is full us-east has room", ev.Note)
	assert.Contains(t, string(ev.Line()), `"note":"eu-west is full us-east has room"`)

	long := builder().Item(Ask, Item{ID: "T-5", Note: strings.Repeat("n", 3000), Opened: opened})
	assert.Len(t, []rune(long.Note), NoteMax)
}

// An id that is not an ask, a task or a parked item is never put in an argv: it could be read as a flag.
func TestAnItemIdThatIsNoIdGetsNoArgv(t *testing.T) {
	for _, id := range []string{"--from", "A-", "A-1x", "x-1", "A-1 --by", "", "-A-1"} {
		ev := builder().Item(Ask, Item{ID: id, Text: "q", Opened: opened, Options: []string{"a"}, Rec: "a", Check: true})
		assert.Nil(t, ev.Answer, id)
	}

	for _, id := range []string{"A-1", "T-62", "L-3"} {
		assert.NotNil(t, builder().Item(Ask, Item{ID: id, Text: "q", Opened: opened}).Answer, id)
	}

	// Every flag stands before the --: what follows it is the id and the words, whatever they say.
	ev := builder().Item(Ask, Item{ID: "A-7", Text: "q", Opened: opened, Options: []string{"--by ghost"}})
	argv := ev.Answer.Options[0].Argv
	assert.Equal(t, []string{"zmina", "--crew", "/home/alex/.zmina/acme/crew.yaml", "todo", "answer", "--by", "pager", "--", "A-7", "--by ghost"}, argv)
}

// choose takes a choice from 1 to 9: no argv is offered for a tenth, though the choice is said.
func TestNoArgvForAChoiceAboveNine(t *testing.T) {
	var opts []Option
	for n := 1; n <= 11; n++ {
		opts = append(opts, Option{N: n, Text: "choice " + strconv.Itoa(n)})
	}

	ev := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Prompt: "question", Options: opts})

	assert.Len(t, ev.Options, 11, "every choice is said")
	require.NotNil(t, ev.Answer)
	assert.Len(t, ev.Answer.Options, 9, "only 1 to 9 can be pressed")
	assert.Equal(t, 9, ev.Answer.Options[8].N)
}

// A subject that is not all of what is approved is said so, and offers nothing to press.
func TestACutSubjectOffersNoArgv(t *testing.T) {
	ev := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Nonce: "0011223344556677", Prompt: "permission", Tool: "Bash", Subject: "make deploy",
		SubjectCut: true, Options: []Option{{N: 1, Text: "Yes"}, {N: 2, Text: "No"}}})

	assert.True(t, ev.SubjectCut)
	assert.True(t, ev.NeedsTerminal)
	assert.Nil(t, ev.Answer)
	assert.Contains(t, string(ev.Line()), `"subject":"make deploy","subject_cut":true,"needs_terminal":true`)
	assert.Len(t, ev.Options, 2)
}

// A permission prompt whose subject was not compared with the prompt's own block on the screen (any tool but a shell call: no golden
// shows one) is told with its subject and offers nothing to press.
func TestAnUncheckedPermissionSubjectOffersNoArgv(t *testing.T) {
	ev := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Nonce: "0011223344556677", Prompt: "permission", Tool: "WebFetch",
		Subject: "https://example.com/x", Options: []Option{{N: 1, Text: "Yes"}, {N: 2, Text: "No"}}})

	assert.False(t, ev.SubjectChecked)
	assert.False(t, ev.SubjectCut)
	assert.True(t, ev.NeedsTerminal)
	assert.Nil(t, ev.Answer)
	assert.Equal(t, "https://example.com/x", ev.Subject)
	assert.Len(t, ev.Options, 2)

	// A question is not a permission prompt: the check is of a subject, and it has none.
	q := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Prompt: "question", Options: []Option{{N: 1, Text: "Yes"}}})
	assert.False(t, q.NeedsTerminal)
	assert.NotNil(t, q.Answer)

	// And one checked, and whole, is pressed.
	ok := builder().Dialog(DialogFacts{Role: "dev", ID: "ab", Prompt: "permission", Tool: "Bash", Subject: "ls", SubjectChecked: true,
		Options: []Option{{N: 1, Text: "Yes"}}})
	assert.False(t, ok.NeedsTerminal)
	assert.NotNil(t, ok.Answer)
}
