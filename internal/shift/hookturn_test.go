package shift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// What a role's last reply asks the owner, and from where the owner has to read it.
func TestTurnQuestion(t *testing.T) {
	long := strings.Repeat("The build is red on three targets and the cause is the same. ", 300)

	for _, tc := range []struct {
		name, reply string
		want        string
		asked       bool
	}{
		{name: "no question", reply: "Done. The tests pass and the branch is pushed."},
		{name: "nothing at all", reply: ""},
		{name: "only blanks", reply: " \n\n \t\n"},
		{name: "a question that ends the reply", reply: "The migration is ready.\n\nShip it to dev?",
			want: "Ship it to dev?", asked: true},
		{name: "a question first, then statements and a second question",
			reply: "I found two failures.\n\nFix it now? I also need to know whether it was dev or prod.\nThe logs say dev.\n\nThat is all.",
			want:  "Fix it now? I also need to know whether it was dev or prod.\nThe logs say dev.\n\nThat is all.", asked: true},
		{name: "the paragraph is kept from its start", reply: "Intro line.\n\nThe cache is stale. Should I clear it\nor wait for the deploy?\nIt costs nothing.",
			want: "The cache is stale. Should I clear it\nor wait for the deploy?\nIt costs nothing.", asked: true},
		{name: "the first paragraph with a question, not the last", reply: "A?\n\nplain\n\nB?", want: "A?\n\nplain\n\nB?", asked: true},
		{name: "a question in a later paragraph only", reply: "Plain one.\n\nPlain two.\n\nWhich one?\n\nPlain four.",
			want: "Which one?\n\nPlain four.", asked: true},
		{name: "a question in a url or a query string", reply: "See https://example.com/search?q=acme&page=2 and http://acme.test/a?b=1 for it."},
		{name: "a question mark that ends a url sentence", reply: "Did you see https://example.com/page? It moved.",
			want: "Did you see https://example.com/page? It moved.", asked: true},
		{name: "a question mark glued to a word", reply: "Is it a?b or x?.y here"},
		{name: "a question in a fenced code block", reply: "Here is the code:\n\n```go\nfmt.Println(\"really?\")\nx := a ? b : c\n```\n\nIt compiles."},
		{name: "a tilde fence", reply: "Run:\n~~~\nwhy?\n~~~\nthat is it."},
		{name: "a fence longer than its closing line", reply: "```\nline?\n``` not a close?\n```\n````\ndone"},
		{name: "a fence that is never closed", reply: "Output:\n```\nwhat?\nstill code?"},
		{name: "a question in an inline code span", reply: "Use `a ? b : c` or ``what?`` here."},
		{name: "an unmatched backtick is just a character", reply: "It costs ` a lot. Fine?", want: "It costs ` a lot. Fine?", asked: true},
		{name: "a question after an inline code span", reply: "Should I run `make test`?", want: "Should I run `make test`?", asked: true},
		{name: "a question in a quoted line", reply: "You wrote:\n> is it ready?\n> and tested?\n\nIt is."},
		{name: "a quote does not hide the question outside it", reply: "You wrote:\n> is it ready?\n\nShall I go on?", want: "Shall I go on?", asked: true},
		{name: "a quote inside the paragraph does not move its start", reply: "Before.\n> quoted?\nAfter, so ok?", want: "Before.\n> quoted?\nAfter, so ok?", asked: true},
		{name: "code inside the paragraph stays in the text", reply: "The diff:\n```\n- a\n+ b\n```\nApply it?", want: "The diff:\n```\n- a\n+ b\n```\nApply it?", asked: true},
		{name: "a paragraph that opens with code starts at its prose", reply: "```\nx\n```\nShip it?", want: "Ship it?", asked: true},
		{name: "emphasis closes after the mark", reply: "All good.\n\n**Fix it now?**", want: "**Fix it now?**", asked: true},
		{name: "underscore and quotes close after the mark", reply: "He said \"why?\" and left. _Really?_ (maybe?) [sure?]", want: "He said \"why?\" and left. _Really?_ (maybe?) [sure?]", asked: true},
		{name: "a guillemet closes after the mark", reply: "Он спросил «зачем?» и ушёл", want: "Он спросил «зачем?» и ушёл", asked: true},
		{name: "a mark and an exclamation", reply: "Really?! Yes.", want: "Really?! Yes.", asked: true},
		{name: "the fullwidth mark", reply: "続けますか？ はい。", want: "続けますか？ はい。", asked: true},
		{name: "a question mark before a letter is none", reply: "ключ?значение и что?то"},
		{name: "Cyrillic", reply: "Готово, ветка залита.\n\nЧинить сейчас? Ещё нужно знать, это dev или prod.\nВ логах dev.",
			want: "Чинить сейчас? Ещё нужно знать, это dev или prod.\nВ логах dev.", asked: true},
		{name: "Cyrillic without a question", reply: "Готово. Всё работает, вопросов нет."},
		{name: "windows line ends", reply: "Ready.\r\n\r\nShip it?\r\nNow.", want: "Ship it?\nNow.", asked: true},
		{name: "trailing white space is cut", reply: "Ship it?  \n\n\n", want: "Ship it?", asked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TurnQuestion(tc.reply)
			assert.Equal(t, tc.asked, ok)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("a very long text is bounded and keeps the question's start", func(t *testing.T) {
		got, ok := TurnQuestion("Short intro.\n\nIs this the one? " + long + "Last words.")
		require.True(t, ok)
		assert.LessOrEqual(t, utf8.RuneCountInString(got), turnTextMax)
		assert.True(t, strings.HasPrefix(got, "Is this the one? The build"), got[:40])
		assert.True(t, strings.HasSuffix(got, "…"), "the cut is shown")
	})

	t.Run("a very long reply is read from its end", func(t *testing.T) {
		huge := strings.Repeat("Nothing is asked in this line at all.\n", 20000) + "\nDid it work?"
		require.Greater(t, len(huge), turnReplyMax)

		got, ok := TurnQuestion(huge)
		require.True(t, ok)
		assert.Equal(t, "Did it work?", got)
	})

	t.Run("a very long reply with a question only before the part that is read has none", func(t *testing.T) {
		huge := "Early question?\n\n" + strings.Repeat("A plain line with nothing asked.\n", 20000)
		require.Greater(t, len(huge), turnReplyMax)

		_, ok := TurnQuestion(huge)
		assert.False(t, ok)
	})

	t.Run("backticks of every length do not make it slow", func(t *testing.T) {
		var b strings.Builder

		for n := 1; n < 700; n++ {
			b.WriteString(strings.Repeat("`", n) + " x ")
		}

		done := make(chan struct{})

		go func() { _, _ = TurnQuestion(b.String()); close(done) }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("slow")
		}
	})
}

// The record is the question and where it is from; a turn that asks nothing takes it away.
func TestRecordTurnWritesAndRemoves(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)

	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Done.\n\nShip it?", at))

	rec, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "sid-1", rec.SessionID)
	assert.True(t, at.Equal(rec.At))
	assert.Equal(t, "Ship it?", rec.Text)
	assert.Regexp(t, `^[0-9a-f]{16}$`, rec.ID)

	st, err := os.Stat(l.HookTurn("dev"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	// The same words in another turn are another question.
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Done.\n\nShip it?", at.Add(time.Minute)))

	again, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.NotEqual(t, rec.ID, again.ID)

	// The next turn asks nothing.
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Shipped.", at.Add(2*time.Minute)))
	assert.NoFileExists(t, l.HookTurn("dev"))

	none, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.Nil(t, none)

	// Taking away what is not there is no error.
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Shipped.", at.Add(3*time.Minute)))
}

// A role or a session that cannot name a file records nothing, and a crew directory that is not there is not made.
func TestRecordTurnRefusesWhatCannotBeKept(t *testing.T) {
	dir := t.TempDir()
	l := crew.Layout{Dir: dir}
	now := time.Now()

	for _, role := range []string{"", "..", "a/b", "../x"} {
		require.NoError(t, RecordTurn(l, role, "sid-1", "Ship it?", now), role)
	}

	require.NoError(t, RecordTurn(l, "dev", "", "Ship it?", now))
	assert.NoDirExists(t, l.Hooks())

	gone := crew.Layout{Dir: filepath.Join(dir, "nope")}
	require.Error(t, RecordTurn(gone, "dev", "sid-1", "Ship it?", now))
	assert.NoDirExists(t, gone.Dir)
}

// A record that is there and does not read is an error, one that is not there is none.
func TestReadTurnTellsAMissingRecordFromABrokenOne(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	require.NoError(t, os.MkdirAll(l.Hooks(), 0o700))

	rec, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.Nil(t, rec)

	for name, body := range map[string]string{
		"not json":         "{",
		"empty":            "",
		"no session":       `{"at":"2026-11-13T09:30:00Z","text":"Ship it?","id":"0123456789abcdef"}`,
		"no time":          `{"session":"s","text":"Ship it?","id":"0123456789abcdef"}`,
		"no text":          `{"session":"s","at":"2026-11-13T09:30:00Z","id":"0123456789abcdef"}`,
		"no id":            `{"session":"s","at":"2026-11-13T09:30:00Z","text":"Ship it?"}`,
		"a time that is 0": `{"session":"s","at":"1970-01-01T00:00:00Z","text":"Ship it?","id":"0123456789abcdef"}`,
	} {
		require.NoError(t, os.WriteFile(l.HookTurn("dev"), []byte(body), 0o600))

		rec, err = ReadTurn(l, "dev")
		require.Error(t, err, name)
		assert.Nil(t, rec, name)
	}

	// A name that is no file name is no record.
	rec, err = ReadTurn(l, "../dev")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

// A view is of the turn the record is of only while the role is idle since it, in its session.
func TestTurnRecordStands(t *testing.T) {
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	rec := &TurnRecord{SessionID: "sid-1", At: at, Text: "Ship it?", ID: "0123456789abcdef"}
	idle := View{State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: at.Add(300 * time.Millisecond)}

	assert.True(t, rec.Stands(idle))
	assert.True(t, rec.Stands(View{State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: at.Add(-turnSkew)}), "the session file may be first")
	assert.True(t, rec.Stands(View{State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: at.Add(turnSkew)}))

	for name, v := range map[string]View{
		"busy":               {State: PresenceUp, Status: "busy", SessionID: "sid-1", Since: at},
		"waiting":            {State: PresenceUp, Status: "waiting", SessionID: "sid-1", Since: at},
		"another session":    {State: PresenceUp, Status: "idle", SessionID: "sid-2", Since: at},
		"no session":         {State: PresenceUp, Status: "idle", Since: at},
		"idle since later":   {State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: at.Add(turnSkew + time.Second)},
		"idle since before":  {State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: at.Add(-turnSkew - time.Second)},
		"no moment":          {State: PresenceUp, Status: "idle", SessionID: "sid-1"},
		"a moment of 0":      {State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: time.UnixMilli(0)},
		"down":               {State: PresenceDown, Status: "idle", SessionID: "sid-1", Since: at},
		"a prompt":           {State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: at, Prompt: true},
		"a dialog":           {State: PresenceUp, Status: "idle", SessionID: "sid-1", Since: at, Dialog: &Dialog{}},
		"outside the window": {State: PresenceOutside, Status: "idle", SessionID: "sid-1", Since: at},
	} {
		assert.False(t, rec.Stands(v), name)
	}

	assert.False(t, (*TurnRecord)(nil).Stands(idle), "no record")
	assert.False(t, (&TurnRecord{At: at}).Stands(idle), "a record of no session")
}

// A role that starts again has no turn question of the session before.
func TestStartDropsTheTurnRecord(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	l := f.s.Crew.Layout()

	require.NoError(t, RecordTurn(l, "designer", "sid-designer", "Ship it?", time.Now()))
	require.FileExists(t, l.HookTurn("designer"))

	f.s.dropHookRecords("designer")
	assert.NoFileExists(t, l.HookTurn("designer"))
}
