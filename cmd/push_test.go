package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

func pushCrew(t *testing.T, body string) *crew.Crew {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte("dir: "+dir+"\ntimezone: Europe/Warsaw\nnotify: [push]\n"+body+
		"roles:\n  dispatcher:\n"), 0o600))

	c, err := crew.Load(path)
	require.NoError(t, err)

	return c
}

func ids(items []todoItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.ID)
	}

	return out
}

// A blocking item goes once, and again after notify_again while it still blocks; a new ask goes once, one already
// open when pushing began never; nothing goes in quiet hours, and what was due then goes after them.
func TestPushDue(t *testing.T) {
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	require.NoError(t, err)

	start := time.Date(2026, 11, 9, 10, 0, 0, 0, warsaw)
	c := pushCrew(t, "notify_on: [blocking, ask]\nnotify_again: 6h\nquiet_hours: 23:00-08:00\n")
	st := shift.NewState()

	old := todoItem{ID: "A-1", Kind: "decision", Title: "old one", Created: start.Add(-time.Hour)}
	assert.Empty(t, ids(pushDue(c, st, []todoItem{old}, start)), "open before pushing began")
	assert.True(t, st.PushFrom.Equal(start))

	block := todoItem{ID: "A-2", Kind: "check", Title: "login", Blocks: "dev", Created: start.Add(time.Minute)}
	fresh := todoItem{ID: "A-3", Kind: "decision", Title: "name", Created: start.Add(2 * time.Minute)}
	items := []todoItem{old, block, fresh}

	assert.Equal(t, []string{"A-2", "A-3"}, ids(pushDue(c, st, items, start.Add(3*time.Minute))))
	assert.Empty(t, ids(pushDue(c, st, items, start.Add(4*time.Minute))), "once")
	assert.Empty(t, ids(pushDue(c, st, items, start.Add(5*time.Hour))))
	assert.Equal(t, []string{"A-2"}, ids(pushDue(c, st, items, start.Add(6*time.Hour+3*time.Minute))),
		"still blocking after notify_again")

	// A reminder's own ask went with the reminder.
	byAlarm := todoItem{ID: "A-4", Kind: "check", Title: "standup", Created: start.Add(7 * time.Hour), byAlarm: true}
	items = append(items, byAlarm)
	assert.Equal(t, []string{"A-4"}, ids(pushDue(c, st, items, start.Add(7*time.Hour))),
		"reminders not pushed: its ask is a new ask like any")

	// Answered: it leaves the record; a later item of the same id would be new.
	items = []todoItem{old, fresh}
	assert.Empty(t, ids(pushDue(c, st, items, start.Add(8*time.Hour))))
	assert.NotContains(t, st.Pushed, "A-2")

	// Quiet hours hold it; the first beat after them sends it.
	night := time.Date(2026, 11, 9, 23, 30, 0, 0, warsaw)
	late := todoItem{ID: "T-5", Kind: "waiting", Title: "merge it?", Blocks: "dev", Created: night}
	items = append(items, late)
	assert.Empty(t, ids(pushDue(c, st, items, night)))
	assert.Empty(t, ids(pushDue(c, st, items, night.Add(8*time.Hour+29*time.Minute))))
	assert.Equal(t, []string{"T-5"}, ids(pushDue(c, st, items, night.Add(8*time.Hour+30*time.Minute))))

	// With reminders on, an ask a reminder made is not pushed again.
	c = pushCrew(t, "notify_on: [reminder, ask]\n")
	st = shift.NewState()
	st.PushFrom = start
	assert.Empty(t, ids(pushDue(c, st, []todoItem{byAlarm}, start.Add(8*time.Hour))))
	waiting := todoItem{ID: "T-6", Kind: "waiting", Title: "review", Blocks: "dev", Created: start.Add(time.Hour)}
	assert.Empty(t, ids(pushDue(c, st, []todoItem{waiting}, start.Add(8*time.Hour))), "blocking not on")
}

func TestPushText(t *testing.T) {
	one := todoItem{ID: "A-2", Kind: "check", Title: "log in on the box", Blocks: "dev"}
	assert.Equal(t, "waits on you: A-2 log in on the box (blocks dev)", pushText([]todoItem{one}))

	two := todoItem{ID: "A-3", Kind: "decision", Text: "Which name?\nLong text", Rec: "acme"}
	assert.Equal(t, "2 wait on you: A-2 log in on the box (blocks dev); A-3 Which name? Long text (recommended: acme)",
		pushText([]todoItem{one, two}))
}

// The ticker pushes a blocking ask through the notify command once, with its event; todo ls says when it went.
func TestTickPushesBlockingAsk(t *testing.T) {
	dir := t.TempDir()
	pushed := filepath.Join(dir, "pushed")
	script := filepath.Join(dir, "push")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s|%s\\n' \"$*\" \"$ZMINA_EVENT\" "+
		"\"$ZMINA_ALARM\" >> "+pushed+"\n"), 0o700))

	crewFile := testCrew(t, "notify: ["+script+", --title, zmina]\nnotify_on: [blocking, ask]\n")

	_, _, err := zm(t, crewFile, "tick")
	require.NoError(t, err)
	_, err = os.Stat(pushed)
	require.ErrorIs(t, err, os.ErrNotExist, "nothing waits: no push")

	_, _, err = zm(t, crewFile, "todo", "add", "Log in to the build box", "--title", "log in", "--blocks", "dev")
	require.NoError(t, err)

	out, _, err := zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "push: A-1")
	assert.Equal(t, "--title zmina waits on you: A-1 log in (blocks dev)|blocking|A-1\n", string(mustRead(t, pushed)))

	_, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(mustRead(t, pushed)), "\n"), "once")

	out, _, err = zm(t, crewFile, "todo", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `A-1\s+check\s+\S+\s+\S+ · blocks dev · pushed \w{3} \d\d:\d\d\s+log in`, out)

	_, _, err = zm(t, crewFile, "todo", "add", "Which name for the site?", "--rec", "acme")
	require.NoError(t, err)
	_, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, string(mustRead(t, pushed)),
		"--title zmina waits on you: A-2 Which name for the site? (recommended: acme)|ask|A-2\n")

	out, _, err = zm(t, crewFile, "log")
	require.NoError(t, err)
	assert.Contains(t, out, "waits on you: A-1 log in (blocks dev)")
}
