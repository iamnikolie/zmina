package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/shift"
)

// putBell stores the text of a doorbell for dev the way the delivery does and returns its id.
func putBell(t *testing.T, c *crew.Crew, role, body string) string {
	t.Helper()

	id := inbox.IDOf("bell " + role + "\x00" + body)
	require.NoError(t, (&inbox.Store{Dir: c.Layout().Inbox()}).Put(inbox.Entry{ID: id, From: "queue " + role, Role: role, Body: body}))

	return id
}

// zmina inbox show prints the text of a doorbell and notes the fetch in the role's own file, the role the text was stored
// for, with no wait for the state lock another command holds.
func TestInboxShowRecordsTheFetchWithoutTheStateLock(t *testing.T) {
	file := testCrew(t, "  dev: {}\n  ops: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	id := putBell(t, c, "dev", "1. look at T-1 (zmina S-3)\n2. then T-2 (zmina S-4)")

	// Another command holds the lock; the show is a look and waits for nobody.
	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	defer l.Release()

	t.Setenv("ZMINA_ROLE", "ops")

	type result struct {
		out string
		err error
	}

	done := make(chan result, 1)

	go func() {
		out, _, err := zm(t, file, "inbox", "show", id)
		done <- result{out, err}
	}()

	var got result

	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("inbox show waited for the state lock")
	}

	require.NoError(t, got.err)
	assert.Contains(t, got.out, "| 1. look at T-1 (zmina S-3)\n| 2. then T-2 (zmina S-4)\n")

	raw, err := os.ReadFile(c.Layout().DeliveryFetched("dev"))
	require.NoError(t, err, "the file of the role the text was stored for, not of whoever ran the command")

	var line struct {
		ID string    `json:"id"`
		At time.Time `json:"at"`
		By string    `json:"by"`
	}

	require.Equal(t, 1, strings.Count(string(raw), "\n"), "one line a fetch")
	require.NoError(t, json.Unmarshal(raw, &line))
	assert.Equal(t, id, line.ID)
	assert.Equal(t, "ops", line.By)
	assert.WithinDuration(t, time.Now(), line.At, time.Minute)
	assert.NoFileExists(t, c.Layout().DeliveryFetched("ops"))

	// Again: one more line, appended.
	_, _, err = zm(t, file, "inbox", "show", id)
	require.NoError(t, err)

	raw, err = os.ReadFile(c.Layout().DeliveryFetched("dev"))
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(raw), "\n"))
}

// A text that is not a doorbell's is no fetch: no file is made for it.
func TestInboxShowOfAnythingElseRecordsNothing(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	id := inbox.IDOf("note")
	require.NoError(t, (&inbox.Store{Dir: c.Layout().Inbox()}).Put(inbox.Entry{ID: id, From: "note", Body: "a long note"}))

	out, _, err := zm(t, file, "inbox", "show", id)
	require.NoError(t, err)
	assert.Contains(t, out, "| a long note")
	assert.NoDirExists(t, c.Layout().Delivery())
}

// A fetch that cannot be noted never fails the show.
func TestInboxShowNeverFailsForTheFetch(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	id := putBell(t, c, "dev", "1. look at T-1\n2. then T-2")

	// A file where the delivery directory belongs: nothing can be appended under it.
	require.NoError(t, os.WriteFile(c.Layout().Delivery(), []byte("a file where the directory belongs"), 0o600))

	out, errOut, err := zm(t, file, "inbox", "show", id)
	require.NoError(t, err)
	assert.Contains(t, out, "| 1. look at T-1\n| 2. then T-2\n")
	assert.Empty(t, errOut)
}

// A doorbell is listed as the lines it is for, one row each, said to be one bell, and each line says whether its body was
// fetched; a typed one the same.
func TestQueueLsShowsABellAsItsLines(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	const first, second, third = "[dispatcher] look at T-1", "[ops] the build is red", "[dispatcher] and then T-2"

	const text = "[zmina] 2 lines wait for you (dispatcher, ops on T-1): zmina inbox show in-0123456789ab"

	rs := l.Role("dev")
	rs.Posted = []shift.Posted{{Line: text, MsgID: "m-1", Session: "s-1", At: now.Add(-3 * time.Minute),
		Parts: []shift.Part{{Line: first}, {Line: second}}}}
	rs.Sent = &shift.Sent{Line: "[zmina] 1 line waits for you: zmina inbox show in-ba9876543210", At: now.Add(-time.Minute),
		Parts: []shift.Part{{Line: third}}}
	require.NoError(t, l.Save())

	accepted := &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedSocket, Session: "s-1"}

	writeCards(t, file, "dev",
		// An earlier bell's card for the same words, fetched long ago: not this row's.
		shift.Card{ID: shift.CardID("dev", first), Accepted: accepted, Bell: &shift.CardBell{Body: "in-aaaaaaaaaaaa", Shown: cardAt(10, 9, 0)},
			Seen: &shift.CardSeen{At: cardAt(10, 9, 1), By: shift.SeenFetch}},
		shift.Card{ID: shift.CardID("dev", first), From: "dispatcher", Task: "T-1", Written: cardAt(12, 14, 2), Accepted: accepted,
			Bell: &shift.CardBell{Body: "in-0123456789ab"}},
		// Fetched before the conversation showed the bell: its record is still posted, and the line says so.
		shift.Card{ID: shift.CardID("dev", second), From: "ops", Written: cardAt(12, 14, 2), Accepted: accepted,
			Bell: &shift.CardBell{Body: "in-0123456789ab", Shown: cardAt(12, 14, 6)},
			Seen: &shift.CardSeen{At: cardAt(12, 14, 6), By: shift.SeenFetch}},
		shift.Card{ID: shift.CardID("dev", third), From: "dispatcher", Written: cardAt(12, 14, 2),
			Accepted: &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedTyped, Session: "s-1"},
			Bell:     &shift.CardBell{Body: "in-ba9876543210"}},
	)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)

	where := "to its inbox socket " + now.Add(-3*time.Minute).Local().Format("01-02 15:04") + ", 3m ago, not yet seen"

	var rows []string

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "      ") || strings.Contains(line, "[dispatcher]") || strings.Contains(line, "[ops]") {
			rows = append(rows, line)
		}
	}

	require.Len(t, rows, 6, out)
	assert.Contains(t, rows[0], "  typed    "+third+" (typed into its prompt")
	assert.Contains(t, rows[0], "not landed yet: it may have it, 1 of 1 in one bell)")
	assert.Equal(t, "      from dispatcher 11-12 14:02 · accepted 11-12 14:03 typed · bell, not shown yet", rows[1])
	assert.Equal(t, "  posted   "+first+" ("+where+", 1 of 2 in one bell)", rows[2])
	assert.Equal(t, "      from dispatcher 11-12 14:02 · on T-1 · accepted 11-12 14:03 socket · bell, not shown yet", rows[3])
	assert.Equal(t, "  posted   "+second+" ("+where+", 2 of 2 in one bell)", rows[4])
	assert.Equal(t, "      from ops 11-12 14:02 · accepted 11-12 14:03 socket · bell, fetched 11-12 14:06", rows[5])
	assert.NotContains(t, out, "zmina inbox show", "the bell's own text is not listed as a row")
}

// zmina pending --brief lists the doorbells the role was shown and has not fetched, each with its command.
func TestPendingBriefListsTheUnfetchedBell(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	now := time.Now()
	accepted := &shift.CardAccepted{At: now.Add(-20 * time.Minute), How: shift.AcceptedSocket, Session: "s-1"}

	writeCards(t, file, "dev",
		shift.Card{ID: shift.CardID("dev", "[ops] the build is red"), From: "ops", Task: "T-1", Accepted: accepted,
			Bell: &shift.CardBell{Body: "in-0123456789ab", Shown: now.Add(-15 * time.Minute)}},
		shift.Card{ID: shift.CardID("dev", "[ops] and the deploy"), From: "ops", Task: "T-1", Accepted: accepted,
			Bell: &shift.CardBell{Body: "in-0123456789ab", Shown: now.Add(-15 * time.Minute)}},
		shift.Card{ID: shift.CardID("dev", "[ops] read earlier"), From: "ops", Accepted: accepted,
			Bell: &shift.CardBell{Body: "in-ba9876543210", Shown: now.Add(-15 * time.Minute)},
			Seen: &shift.CardSeen{At: now.Add(-10 * time.Minute), By: shift.SeenFetch}},
	)

	out, _, err := zm(t, file, "pending", "--role", "dev", "--brief")
	require.NoError(t, err)
	assert.Contains(t, out, "zmina inbox show in-0123456789ab (2 lines from ops on T-1, shown ")
	assert.NotContains(t, out, "in-ba9876543210", "fetched")

	// A fetch the cards do not hold yet is as good: the brief reads the role's fetches too.
	require.NoError(t, shift.RecordFetch(c.Layout(), "dev", "in-0123456789ab", "dev", now))

	out, _, err = zm(t, file, "pending", "--role", "dev", "--brief")
	require.NoError(t, err)
	assert.NotContains(t, out, "inbox show")
}

// A fetch says who ran it: the role, hand for the owner's shell, and nothing for a tool of an agent's session whose role is
// not named, which the delivery takes as the role's own.
func TestInboxShowSaysWhoRanIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		by   string
	}{
		{"the role", map[string]string{"ZMINA_ROLE": "dev"}, "dev"},
		{"another role", map[string]string{"ZMINA_ROLE": "ops"}, "ops"},
		{"the owner's shell", map[string]string{}, "hand"},
		{"a Claude tool with no role", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-1"}, ""},
		{"a shared Codex tool with no role", map[string]string{"CODEX_THREAD_ID": "thread-1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := testCrew(t, "  dev: {}\n  ops: {}\n")

			c, err := crew.Load(file)
			require.NoError(t, err)

			id := putBell(t, c, "dev", "1. one\n2. two")

			t.Setenv("CLAUDE_CODE_SESSION_ID", "")
			t.Setenv("CODEX_THREAD_ID", "")

			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, _, err = zm(t, file, "inbox", "show", id)
			require.NoError(t, err)

			raw, err := os.ReadFile(c.Layout().DeliveryFetched("dev"))
			require.NoError(t, err)

			var line struct {
				By string `json:"by"`
			}

			require.NoError(t, json.Unmarshal(raw, &line))
			assert.Equal(t, tc.by, line.By)
		})
	}
}

// The text of a bell whose lines went back to the queue says so in one line, and reading it is no fetch.
func TestInboxShowOfASupersededBellSaysSoAndRecordsNothing(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	id := inbox.IDOf("bell dev\x00gone")
	require.NoError(t, (&inbox.Store{Dir: c.Layout().Inbox()}).Put(inbox.Entry{ID: id, From: "queue dev", Body: shift.BellSupersededText}))

	out, _, err := zm(t, file, "inbox", "show", id)
	require.NoError(t, err)
	assert.Contains(t, out, "| These lines were queued again and will come with a new bell")
	assert.Equal(t, 2, strings.Count(out, "\n"), "the header and the one line")
	assert.NoDirExists(t, c.Layout().Delivery())
}

// The lines of a doorbell that was shown and not fetched are listed under the role: the bell is no record in flight any
// more, and they wait. One the role fetched is not; one a different role fetched still is.
func TestQueueLsListsTheLinesOfABellShownAndNotFetched(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	now := time.Now()
	accepted := &shift.CardAccepted{At: now.Add(-20 * time.Minute), How: shift.AcceptedSocket, Session: "s-1"}
	shown := now.Add(-15 * time.Minute)

	const open, read, other = "[ops] the build is red", "[ops] read already", "[ops] read by someone else"

	writeCards(t, file, "dev",
		shift.Card{ID: shift.CardID("dev", open), Head: open, From: "ops", Task: "T-1", Accepted: accepted, Bell: &shift.CardBell{Body: "in-0123456789ab", Shown: shown}},
		shift.Card{ID: shift.CardID("dev", read), Head: read, From: "ops", Accepted: accepted, Bell: &shift.CardBell{Body: "in-ba9876543210", Shown: shown}},
		shift.Card{ID: shift.CardID("dev", other), Head: other, From: "ops", Accepted: accepted, Bell: &shift.CardBell{Body: "in-aaaaaaaaaaaa", Shown: shown}},
	)

	require.NoError(t, shift.RecordFetch(c.Layout(), "dev", "in-ba9876543210", "dev", now.Add(-10*time.Minute)))
	require.NoError(t, shift.RecordFetch(c.Layout(), "dev", "in-aaaaaaaaaaaa", "ops", now.Add(-10*time.Minute)))

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)

	assert.Contains(t, out, "dev:\n")
	assert.Contains(t, out, "  bell     "+open+" (a bell shown "+shown.Local().Format("01-02 15:04")+", 15m ago, its text not fetched by dev)")
	assert.Contains(t, out, "bell shown "+shown.Local().Format("01-02 15:04")+", not fetched", "its facts")
	assert.Contains(t, out, "  bell     "+other)
	assert.NotContains(t, out, read)
	assert.NotContains(t, out, "inbox show", "the command that prints the lines is the role's")

	js, _, err := zm(t, file, "queue", "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, js, `"unfetched"`)
}
