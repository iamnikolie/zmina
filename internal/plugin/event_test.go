package plugin

import (
	"encoding/json"
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
				`{"n":2,"text":"acme-lab"}],"answer":{"text":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer","A-3",` +
				`"--by","pager","--","<text>"],"yes":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","yes","A-3","--by","pager"],` +
				`"options":[{"n":1,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer","A-3","--by","pager","--","acme"]},` +
				`{"n":2,"argv":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer","A-3","--by","pager","--","acme-lab"]}]},` +
				`"opened":"2026-11-13T09:30:00Z","blocking":false}`},
		{"blocking check", b.Item(Blocking, Item{ID: "A-4", Role: "dev", Title: "Log in", Text: "Log in on the box", Why: "holds up dev on T-5 Deploy",
			Opened: opened, Blocking: true, Check: true}),
			`{"v":1,"event":"blocking","id":"A-4","role":"dev","title":"Log in","text":"Log in on the box","why":"holds up dev on T-5 Deploy",` +
				`"answer":{"text":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","answer","A-4","--by","pager","--","<text>"],` +
				`"done":["zmina","--crew","/home/alex/.zmina/acme/crew.yaml","todo","done","A-4","--by","pager"]},` +
				`"opened":"2026-11-13T09:30:00Z","blocking":true}`},
		{"reminder of a parked item", b.Item(Reminder, Item{ID: "L-2", Title: "Renew the domain", Text: "Renew the domain", Opened: opened, Later: true}),
			`{"v":1,"event":"reminder","id":"L-2","title":"Renew the domain","text":"Renew the domain","answer":{"done":["zmina","--crew",` +
				`"/home/alex/.zmina/acme/crew.yaml","todo","done","L-2","--by","pager"]},"opened":"2026-11-13T09:30:00Z","blocking":false}`},
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
