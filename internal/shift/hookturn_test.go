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

		// Where a question counts: in the last three paragraphs of prose, and in prose only.
		{name: "a question above the last three paragraphs is of a report that went on", reply: "Old?\n\nB.\n\nC.\n\nD."},
		{name: "a question in the third paragraph from the end", reply: "Old.\n\nAsk?\n\nB.\n\nC.", want: "Ask?\n\nB.\n\nC.", asked: true},
		{name: "a paragraph with no prose is not counted", reply: "Q?\n\n## Heading\n\nB.\n\nC.", want: "Q?\n\n## Heading\n\nB.\n\nC.", asked: true},
		{name: "a table is no paragraph either", reply: "Q?\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nB.\n\nC.", want: "Q?\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nB.\n\nC.", asked: true},
		{name: "a heading is not asked", reply: "# What next?\n\nDone."},
		{name: "a heading of any depth", reply: "###### Why?\nDone."},
		{name: "an issue number is not a heading", reply: "#123 is broken, fix it?", want: "#123 is broken, fix it?", asked: true},
		{name: "a table row is not asked", reply: "| name | ready? |\n|---|---|\n| dev | yes |\n"},
		{name: "indented code is not asked", reply: "Result:\n\n    why not?\n    really?\n\nDone."},
		{name: "tab-indented code is not asked", reply: "Result:\n\n\twhy not?\n\nDone."},
		{name: "an indented line inside a paragraph goes on with it", reply: "- an item\n    goes on, ok?", want: "- an item\n    goes on, ok?", asked: true},
		{name: "an html comment is not asked", reply: "<!-- is this ok? -->\nDone."},
		{name: "a comment of several lines", reply: "<!--\nask?\n-->\nDone."},
		{name: "a comment inside a line", reply: "Done <!-- why? --> now."},
		{name: "text around a comment is asked", reply: "Done <!-- why --> now, ok?", want: "Done <!-- why --> now, ok?", asked: true},

		// The items of a list are one paragraph, whether or not blank lines stand between them.
		{name: "a loose list does not push the question out of the last three", reply: "Should I deploy?\n\n1. a\n\n2. b\n\n3. c",
			want: "Should I deploy?\n\n1. a\n\n2. b\n\n3. c", asked: true},
		{name: "a bulleted loose list", reply: "Should I deploy?\n\n- a\n\n- b\n\n* c\n\n+ d", want: "Should I deploy?\n\n- a\n\n- b\n\n* c\n\n+ d", asked: true},
		{name: "numbers with a bracket", reply: "Which one?\n\n1) a\n\n2) b\n\n3) c\n\n4) d", want: "Which one?\n\n1) a\n\n2) b\n\n3) c\n\n4) d", asked: true},
		{name: "a list with its question inside", reply: "Plan:\n\n- a\n\n- b?\n\n- c", want: "- a\n\n- b?\n\n- c", asked: true},
		{name: "a list does not go on through a paragraph of words", reply: "Ask?\n\n- a\n\nWords.\n\n- b\n\n- c"},
		{name: "a list does not go on through a heading", reply: "Ask?\n\n- a\n\n## Heading\n\nWords.\n\n- b\n\n- c"},
		{name: "a heading between lists splits them", reply: "Ask?\n\n- a\n\n## Heading\n\n- b\n\n- c", want: "Ask?\n\n- a\n\n## Heading\n\n- b\n\n- c", asked: true},
		{name: "bold is no list", reply: "Ask?\n\n**a**\n\n**b**\n\n**c**"},
		{name: "a number that begins a sentence is no list", reply: "Ask?\n\n2026 was long.\n\n2027 will be.\n\n2028 too."},

		// What may follow the mark.
		{name: "a colon after the mark", reply: "Fix it now?: yes or no", want: "Fix it now?: yes or no", asked: true},
		{name: "a full stop after the mark", reply: "Really?.", want: "Really?.", asked: true},
		{name: "an ellipsis after the mark", reply: "Really?…", want: "Really?…", asked: true},
		{name: "a quote and a full stop after the mark", reply: "He asked \"why?\".", want: "He asked \"why?\".", asked: true},
		{name: "a bracket and a full stop after the mark", reply: "Is it done (really?).", want: "Is it done (really?).", asked: true},
		{name: "a zero-width space after the mark", reply: "Fix it?" + string(rune(0x200b)) + "Now.", want: "Fix it?" + string(rune(0x200b)) + "Now.", asked: true},
		{name: "the Arabic mark", reply: "هل تريد المتابعة؟ نعم", want: "هل تريد المتابعة؟ نعم", asked: true},
		{name: "the Arabic mark at the end", reply: "Done.\n\nهل نكمل؟", want: "هل نكمل؟", asked: true},
		{name: "optional chaining is not a question", reply: "It reads a?.b and c?:d here."},
		{name: "a full stop that goes on is no end", reply: "It reads a?.b"},
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

// A reply read from its end, cut inside a fenced block, does not read the rest of that block as prose.
func TestTurnQuestionOfACutReplyIsNotReadInsideACodeBlock(t *testing.T) {
	code := strings.Repeat("a line of code that asks why?\n", 12000)
	require.Greater(t, len(code), turnReplyMax)

	// The block opens before what is read, and is closed within it: what is read starts inside it.
	_, ok := TurnQuestion("Start.\n```\n" + code + "```\nAll good.\n")
	assert.False(t, ok, "the end of a code block is code")

	got, ok := TurnQuestion("Start.\n```\n" + code + "```\nAll good, or not?\n")
	require.True(t, ok)
	assert.Equal(t, "All good, or not?", got, "the prose after it is read as ever")

	// A reply cut in prose is read from the cut, a block inside what is read being whole.
	_, ok = TurnQuestion(strings.Repeat("Plain words with nothing asked.\n", 12000) + "\n```\ncode?\n```\nDone.\n")
	assert.False(t, ok)

	got, ok = TurnQuestion(strings.Repeat("Plain words with nothing asked.\n", 12000) + "\n```\ncode?\n```\nDone, ok?\n")
	require.True(t, ok)
	assert.Equal(t, "Done, ok?", got)
}

// The scan for the mark that ends a sentence is one pass: a long line of marks is no quadratic walk, which a hook with a 5 second
// timeout cannot afford.
func TestTurnQuestionOfLongRunsOfMarksIsLinear(t *testing.T) {
	for name, text := range map[string]string{
		"question marks then a letter":  strings.Repeat("?", 200000) + "x",
		"question marks and quotes":     strings.Repeat("?\"", 100000) + "x",
		"marks of every kind":           strings.Repeat("?!.:…*_)", 30000) + "x",
		"question marks inside a quote": "> " + strings.Repeat("?", 200000),
		"a question at the end":         strings.Repeat("?", 200000) + "x\n\nDone?",
	} {
		done := make(chan struct{})

		go func() { _, _ = TurnQuestion(text); close(done) }()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: slow", name)
		}
	}

	start := time.Now()
	assert.False(t, endsSentence(strings.Repeat("?", 200000)+"x"))
	assert.True(t, endsSentence(strings.Repeat("?", 200000)+" x"))
	assert.Less(t, time.Since(start), time.Second)

	// What counts is as it was, the run skipped whole.
	assert.False(t, endsSentence("a?.b ?:c ??x"))
	assert.True(t, endsSentence("a?.b ??x ?!"))
}

// A record is taken away by whoever ends the question it was of, and no record is no error.
func TestDropTurn(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}

	require.NoError(t, DropTurn(l, "dev"), "none")
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Ship it?", time.Now()))
	require.NoError(t, RecordTurn(l, "ops", "sid-1", "Ship it?", time.Now()))
	require.NoError(t, DropTurn(l, "dev"))
	assert.NoFileExists(t, l.HookTurn("dev"))
	assert.FileExists(t, l.HookTurn("ops"), "no other role's")
	require.NoError(t, DropTurn(l, "../ops"), "a name that is no file name is no record")
	assert.FileExists(t, l.HookTurn("ops"))
}

// A record whose id is not the fingerprint of its session, moment and words was written by no hook: it does not read.
func TestReadTurnTakesOnlyARecordWhoseIdIsItsFingerprint(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 11, 13, 9, 30, 0, 123456789, time.UTC)

	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Ship it?", at))

	raw, err := os.ReadFile(l.HookTurn("dev"))
	require.NoError(t, err)

	rec, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, turnID("sid-1", at, "Ship it?"), rec.ID, "the fingerprint of what the hook saw")
	assert.Regexp(t, `^[0-9a-f]{16}$`, rec.ID)

	for name, tamper := range map[string]func(string) string{
		"other words":         func(s string) string { return strings.Replace(s, "Ship it?", "Drop it?", 1) },
		"another session":     func(s string) string { return strings.Replace(s, "sid-1", "sid-2", 1) },
		"another moment":      func(s string) string { return strings.Replace(s, "09:30:00.123456789Z", "09:30:01Z", 1) },
		"an id of upper case": func(s string) string { return strings.Replace(s, rec.ID, strings.ToUpper(rec.ID), 1) },
		"a short id":          func(s string) string { return strings.Replace(s, rec.ID, rec.ID[:8], 1) },
		"a long id":           func(s string) string { return strings.Replace(s, rec.ID, rec.ID+"00", 1) },
		"an id that is not hex": func(s string) string {
			return strings.Replace(s, rec.ID, strings.Repeat("g", 16), 1)
		},
	} {
		changed := tamper(string(raw))
		require.NotEqual(t, string(raw), changed, name)
		require.NoError(t, os.WriteFile(l.HookTurn("dev"), []byte(changed), 0o600))

		got, err := ReadTurn(l, "dev")
		require.Error(t, err, name)
		assert.Nil(t, got, name)
	}
}

// The record is the turn's: the question and where it is from, and when the turn before it ended. A turn that asks nothing leaves a
// record with no text, and a turn that ends after it knows when that one ended.
func TestRecordTurnWritesTheTurnAndWhenTheOneBeforeEnded(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)

	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Done.\n\nShip it?", at))

	rec, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "sid-1", rec.SessionID)
	assert.True(t, at.Equal(rec.At))
	assert.True(t, rec.Prev.IsZero(), "no turn before it is known")
	assert.Equal(t, "Ship it?", rec.Text)
	assert.Regexp(t, `^[0-9a-f]{16}$`, rec.ID)

	st, err := os.Stat(l.HookTurn("dev"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	// The same words in another turn are another question, and the record it replaces is the turn before it.
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Done.\n\nShip it?", at.Add(time.Minute)))

	again, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.NotEqual(t, rec.ID, again.ID)
	assert.True(t, at.Equal(again.Prev))

	// The next turn asks nothing: a record with no text says so, and when it ended.
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Shipped.", at.Add(2*time.Minute)))

	none, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	require.NotNil(t, none)
	assert.Empty(t, none.Text)
	assert.True(t, at.Add(2*time.Minute).Equal(none.At))
	assert.True(t, at.Add(time.Minute).Equal(none.Prev))
	assert.Regexp(t, `^[0-9a-f]{16}$`, none.ID, "it is a record like any: its id is its fingerprint")

	// The turn after it is of the same session and knows when that one ended.
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Shall I go on?", at.Add(3*time.Minute)))

	next, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.Equal(t, "Shall I go on?", next.Text)
	assert.True(t, at.Add(2*time.Minute).Equal(next.Prev))

	// A turn of another session does not know of one of a conversation that is over; nor does one that is not after it.
	require.NoError(t, RecordTurn(l, "dev", "sid-2", "Shall I?", at.Add(4*time.Minute)))

	other, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.True(t, other.Prev.IsZero())

	require.NoError(t, RecordTurn(l, "dev", "sid-2", "Shall I?", at.Add(time.Minute)))

	earlier, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	assert.True(t, earlier.Prev.IsZero(), "a record from the future is no turn before")
}

// The end of a question keeps the turn: its text is cleared and nothing else, so the turn after it still knows when it ended. A record
// that is not there, or does not read, has nothing to clear.
func TestClearTurnKeepsWhenTheTurnEnded(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)

	require.NoError(t, ClearTurn(l, "dev"), "none")
	assert.NoFileExists(t, l.HookTurn("dev"))

	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Done.", at))
	require.NoError(t, RecordTurn(l, "dev", "sid-1", "Ship it?", at.Add(time.Minute)))
	require.NoError(t, ClearTurn(l, "dev"))

	rec, err := ReadTurn(l, "dev")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Empty(t, rec.Text)
	assert.True(t, at.Add(time.Minute).Equal(rec.At))
	assert.True(t, at.Equal(rec.Prev))
	assert.Equal(t, "sid-1", rec.SessionID)

	require.NoError(t, os.WriteFile(l.HookTurn("dev"), []byte("{"), 0o600))
	require.Error(t, ClearTurn(l, "dev"), "what does not read is dropped, not cleared")
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
