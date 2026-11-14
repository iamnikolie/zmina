package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/shift"
)

// dispatcherNotes is everything waiting for the dispatcher in the crew's state, one note a line.
func dispatcherNotes(t *testing.T, file string) string {
	t.Helper()

	st, err := shift.Read(filepath.Dir(file))
	require.NoError(t, err)

	return strings.Join(bodies(st.Signals), "\n")
}

// A decision taken is recorded with its reason, and a record without one is refused.
func TestTodoDecidedNeedsItsReason(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "todo", "decided", "keep the old flag name")
	require.EqualError(t, err, `a decision needs its reason: --reason "<why>"`)

	_, _, err = zm(t, file, "todo", "decided", "keep the old flag name", "--reason", "   ")
	require.EqualError(t, err, `a decision needs its reason: --reason "<why>"`, "a blank reason is none")

	out, _, err := zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.Equal(t, "nothing waits on you\n", out, "nothing was recorded")

	out, _, err = zm(t, file, "todo", "decided", "keep", "the", "old", "flag", "name", "--reason", "links point at it",
		"--instead", "rename it", "--title", "old flag name", "--from", "dev", "--task", "t-3", "--project", "site")
	require.NoError(t, err)
	assert.Equal(t, "A-1\n", out)

	c := mustCrew(t, file)
	a, err := askStore(c).Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, asks.Decided, a.Kind)
	assert.Equal(t, "keep the old flag name", a.Text)
	assert.Equal(t, "links point at it", a.Reason)
	assert.Equal(t, "rename it", a.Instead)
	assert.Equal(t, "old flag name", a.Title)
	assert.Equal(t, "dev", a.Role)
	assert.Equal(t, "T-3", a.Task)
	assert.Equal(t, "site", a.Project)
	assert.Empty(t, a.Blocks, "a decided item never holds a role up")
	assert.Empty(t, a.Rec)
	assert.Empty(t, a.Options)
	assert.True(t, a.Open())

	out, _, err = zm(t, file, "log", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "decided")
	assert.Contains(t, out, "A-1: keep the old flag name")

	// The role is the one that runs it, as in todo add.
	t.Setenv("ZMINA_ROLE", "dev")
	_, _, err = zm(t, file, "todo", "decided", "use sqlite", "--reason", "one file")
	require.NoError(t, err)

	a, err = askStore(c).Get("A-2")
	require.NoError(t, err)
	assert.Equal(t, "dev", a.Role)
	assert.Equal(t, "dev", a.By)
}

// A decided item is recorded with todo decided, which asks for its reason, and is not edited.
func TestTodoDecidedIsNeitherAddedNorEdited(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "todo", "add", "--kind", "decided", "keep the old flag name")
	require.ErrorContains(t, err, "zmina todo decided")

	_, _, err = zm(t, file, "todo", "decided", "keep the old flag name", "--reason", "links point at it")
	require.NoError(t, err)

	_, _, err = zm(t, file, "todo", "edit", "A-1", "--rec", "yes", "--blocks", "dev")
	require.ErrorContains(t, err, "not edited")

	a, err := askStore(mustCrew(t, file)).Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, asks.Decided, a.Kind, "an edit never turns it into a decision")
	assert.Empty(t, a.Blocks)
}

// Decided items are the last group, newest first; never overdue, never blocking; one older than a week is not listed and
// stays on disk, open.
func TestTodoListsDecidedLast(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	c := mustCrew(t, file)
	store := askStore(c)
	now := time.Now()

	add := func(a asks.Ask, age time.Duration) string {
		got, err := store.Add(a, now.Add(-age))
		require.NoError(t, err)

		return got.ID
	}

	decidedOld := add(asks.Ask{Kind: asks.Decided, Text: "old call", Reason: "r"}, 3*24*time.Hour)
	plain := add(asks.Ask{Kind: asks.Check, Text: "sign in"}, 2*time.Hour)
	// Whatever a file says, a decided item blocks nothing and is not overdue.
	decidedNew := add(asks.Ask{Kind: asks.Decided, Text: "new call", Reason: "r", Blocks: "dev"}, time.Hour)
	late := add(asks.Ask{Kind: asks.Check, Text: "publish"}, 30*time.Hour)
	gone := add(asks.Ask{Kind: asks.Decided, Text: "stale call", Reason: "r"}, 8*24*time.Hour)
	holds := add(asks.Ask{Kind: asks.Check, Text: "log in", Blocks: "dev"}, time.Hour)
	edge := add(asks.Ask{Kind: asks.Decided, Text: "a week ago", Reason: "r"}, decidedKeep)

	items, _ := todo(c, now, nil)
	assert.Equal(t, []string{holds, late, plain, decidedNew, decidedOld, edge}, ids(items),
		"blocking, overdue, the rest, then the decided ones newest first; the week-old one is the oldest of them")

	for _, it := range items {
		if it.Kind == string(asks.Decided) {
			assert.False(t, it.Overdue, it.ID)
			assert.Empty(t, it.Blocks, it.ID)
		}
	}

	a, err := store.Get(gone)
	require.NoError(t, err)
	assert.True(t, a.Open(), "a decided item past the week stays on disk, open")

	items, _ = todo(c, now.Add(time.Hour), nil)
	assert.NotContains(t, ids(items), edge, "older than a week is not listed")
	assert.Contains(t, ids(items), decidedOld)
}

// todo ls says it all in a line, --full says why and what was not done, --json carries both.
func TestTodoLsShowsDecided(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	out, _, err := zm(t, file, "todo", "decided", "keep the old flag name", "--reason", "links point at it", "--instead",
		"rename it", "--title", "old flag name", "--from", "dev")
	require.NoError(t, err)
	assert.Equal(t, "A-1\n", out)

	out, _, err = zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out, "nothing waits on you"), "a decided item waits on nobody")
	assert.Contains(t, out, "A-1  decided")
	assert.Contains(t, out, "old flag name")
	assert.NotContains(t, out, "links point at it", "the reason is for --full")

	out, _, err = zm(t, file, "todo", "ls", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "keep the old flag name · reason: links point at it · instead: rename it")

	out, _, err = zm(t, file, "todo", "ls", "--json")
	require.NoError(t, err)

	var got struct {
		Items []todoItem `json:"items"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Items, 1)
	assert.Equal(t, "decided", got.Items[0].Kind)
	assert.Equal(t, "links point at it", got.Items[0].Reason)
	assert.Equal(t, "rename it", got.Items[0].Instead)
	assert.Contains(t, out, `"instead": "rename it"`)

	// Something that does wait is said to wait, and the decided item stays after it.
	_, _, err = zm(t, file, "todo", "add", "sign in and publish the board")
	require.NoError(t, err)

	out, _, err = zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "nothing waits on you")
	assert.Less(t, strings.Index(out, "A-2"), strings.Index(out, "A-1"), "the decided item is last")
}

// Done, yes and ignore say a decided item was read and is fine: it closes and the dispatcher hears nothing.
func TestTodoDecidedFineIsQuiet(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	for _, action := range []string{"done", "yes", "ignore"} {
		_, _, err := zm(t, file, "todo", "decided", "call for "+action, "--reason", "why")
		require.NoError(t, err)
	}

	for i, tc := range []struct {
		action string
		state  asks.State
	}{{"done", asks.Done}, {"yes", asks.Yes}, {"ignore", asks.Ignored}} {
		id := "A-" + string(rune('1'+i))

		out, _, err := zm(t, file, "todo", tc.action, id)
		require.NoError(t, err, tc.action)
		assert.Equal(t, id+" · "+tc.action+" · reviewed, nothing queued\n", out)

		a, err := askStore(mustCrew(t, file)).Get(id)
		require.NoError(t, err)
		assert.Equal(t, tc.state, a.State)
		assert.Equal(t, "owner", a.ClosedBy)
	}

	assert.Empty(t, dispatcherNotes(t, file), "nothing for the dispatcher to act on")

	out, _, err := zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.Equal(t, "nothing waits on you\n", out)

	out, _, err = zm(t, file, "log", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "answered")
	assert.Contains(t, out, "A-1 · done — decided: call for done")

	_, _, err = zm(t, file, "todo", "done", "A-1")
	require.ErrorContains(t, err, "already done", "the first answer wins")
}

// The owner's park is as quiet as his done: the parked item is made, and the dispatcher hears nothing.
func TestTodoDecidedParkIsQuiet(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "todo", "decided", "use sqlite", "--reason", "one file")
	require.NoError(t, err)

	out, _, err := zm(t, file, "todo", "park", "A-1")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · park · parked, nothing queued\n", out)
	assert.Empty(t, dispatcherNotes(t, file))

	out, _, err = zm(t, file, "later", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "use sqlite · from A-1", "the item is on the parked list all the same")

	a, err := askStore(mustCrew(t, file)).Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, asks.Parked, a.State)
}

// Only the owner reads a decided item as fine in silence. A role that closes one is recording what he said, as for any
// ask: the dispatcher is told, and the dispatcher closing one has nothing queued for itself.
func TestTodoDecidedClosedByARoleIsNotSilent(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	for _, text := range []string{"first call", "second call", "third call", "fourth call"} {
		_, _, err := zm(t, file, "todo", "decided", text, "--reason", "why", "--from", "dev")
		require.NoError(t, err)
	}

	t.Setenv("ZMINA_ROLE", "dev")

	out, _, err := zm(t, file, "todo", "done", "A-1")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · done · queued for the dispatcher\n", out)
	assert.Contains(t, dispatcherNotes(t, file),
		"dev: A-1 · done — decided: first call (the owner's answer in chat, recorded by dev)")

	a, err := askStore(mustCrew(t, file)).Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, asks.Done, a.State)
	assert.Equal(t, "role dev", a.ClosedBy, "it is a role's close, not the owner's")

	_, _, err = zm(t, file, "todo", "park", "A-2")
	require.NoError(t, err)
	assert.Contains(t, dispatcherNotes(t, file), "A-2 · later (L-1) — decided: second call")

	t.Setenv("ZMINA_ROLE", "dispatcher")

	out, _, err = zm(t, file, "todo", "done", "A-3")
	require.NoError(t, err)
	assert.Equal(t, "A-3 · done · recorded\n", out)
	assert.NotContains(t, dispatcherNotes(t, file), "third call", "the dispatcher has it already")

	// The owner at the keyboard, or through a plugin, is silent.
	t.Setenv("ZMINA_ROLE", "")

	out, _, err = zm(t, file, "todo", "done", "A-4")
	require.NoError(t, err)
	assert.Equal(t, "A-4 · done · reviewed, nothing queued\n", out)
	assert.NotContains(t, dispatcherNotes(t, file), "fourth call")
}

// An owner's answer through a plugin is his own: a decided item it reads as fine closes in silence.
func TestTodoDecidedFineThroughAPluginIsQuiet(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("plugins:\n  - {name: pager, on: [ask]}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, _, err = zm(t, file, "todo", "decided", "use sqlite", "--reason", "one file")
	require.NoError(t, err)

	t.Setenv("ZMINA_PLUGIN", "pager")

	out, _, err := zm(t, file, "todo", "done", "A-1", "--by", "pager")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · done · reviewed, nothing queued\n", out)
	assert.Empty(t, dispatcherNotes(t, file))

	a, err := askStore(mustCrew(t, file)).Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, "plugin pager", a.ClosedBy)
}

// A decided item left open past a week is closed by zmina the next time one is recorded, and the log says which; zmina show
// reads it as closed, not as waiting for review.
func TestTodoDecidedExpiresPastAWeek(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	store := askStore(mustCrew(t, file))
	now := time.Now()

	stale, err := store.Add(asks.Ask{Kind: asks.Decided, Text: "stale call", Reason: "why", Role: "dev"}, now.Add(-8*24*time.Hour))
	require.NoError(t, err)
	fresh, err := store.Add(asks.Ask{Kind: asks.Decided, Text: "recent call", Reason: "why", Role: "dev"}, now.Add(-6*24*time.Hour))
	require.NoError(t, err)
	check, err := store.Add(asks.Ask{Kind: asks.Check, Text: "an old check"}, now.Add(-20*24*time.Hour))
	require.NoError(t, err)

	_, _, err = zm(t, file, "todo", "decided", "a new call", "--reason", "why")
	require.NoError(t, err)

	got, err := store.Get(stale.ID)
	require.NoError(t, err)
	assert.Equal(t, asks.Ignored, got.State)
	assert.Empty(t, got.ClosedBy, "zmina closed it itself")

	got, err = store.Get(fresh.ID)
	require.NoError(t, err)
	assert.True(t, got.Open(), "a week not yet passed")

	got, err = store.Get(check.ID)
	require.NoError(t, err)
	assert.True(t, got.Open(), "only decided items expire")

	out, _, err := zm(t, file, "log", "--full")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out, "decided-expired"), "one event")
	assert.Contains(t, out, stale.ID)

	assert.Empty(t, dispatcherNotes(t, file), "nothing is told to anyone")

	out, _, err = zm(t, file, "show", stale.ID)
	require.NoError(t, err)
	assert.Contains(t, out, "decided by dev · closed after a week, not reviewed · ")
	assert.NotContains(t, out, "to review")
	assert.NotContains(t, out, "act: zmina todo done")

	// Nothing is expired a second time, and none is logged when none is.
	_, _, err = zm(t, file, "todo", "decided", "another call", "--reason", "why")
	require.NoError(t, err)

	out, _, err = zm(t, file, "log", "--full")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out, "decided-expired"))
}

// An answer says the owner disagrees: the dispatcher is told to redo it.
func TestTodoDecidedAnswerQueuesTheRedo(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "todo", "decided", "keep the old flag name", "--reason", "links point at it")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "decided", "use sqlite", "--reason", "one file")
	require.NoError(t, err)

	out, _, err := zm(t, file, "todo", "answer", "A-1", "rename", "it", "after", "all")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · answer · queued for the dispatcher\n", out)

	a, err := askStore(mustCrew(t, file)).Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, asks.Answered, a.State)
	assert.Equal(t, "rename it after all", a.Answer)
	assert.Contains(t, dispatcherNotes(t, file), "hand: A-1 · redo: rename it after all — decided: keep the old flag name")

	_, _, err = zm(t, file, "todo", "answer", "A-1", "again")
	require.ErrorContains(t, err, "already answered")
}

// A decided item about a task is not the task's question: the waiting task still shows for itself, the item takes its
// project, and answering the item leaves the task as it is.
func TestTodoDecidedAboutAWaitingTaskLeavesTheTask(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "task", "new", "dev", "--project", "site", "ship", "it")
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "set", "T-1", "waiting", "--note", "which region?")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "decided", "ship it to staging first", "--reason", "cheaper to undo", "--task", "t-1")
	require.NoError(t, err)

	c := mustCrew(t, file)
	items, _ := todo(c, time.Now(), nil)
	require.Len(t, items, 2)
	assert.Equal(t, []string{"T-1", "A-1"}, ids(items), "the task waits and shows; the decided item is last")
	assert.Equal(t, "waiting", items[0].Kind)
	assert.Equal(t, "which region?", items[0].Note, "the decided item took none of the task's item")
	assert.Equal(t, "site", items[1].Project, "the project is the task's")
	assert.Equal(t, "T-1", items[1].Task)

	// Neither a fine nor a redo is an answer to the task's question.
	_, _, err = zm(t, file, "todo", "done", "A-1")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "decided", "use the eu region", "--reason", "closest", "--task", "T-1")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "answer", "A-2", "use us-east")
	require.NoError(t, err)

	tk, err := taskStore(c).Get("T-1")
	require.NoError(t, err)
	assert.True(t, tk.Answered.IsZero(), "reviewing a decision is no answer to the task's question")

	items, _ = todo(c, time.Now(), nil)
	assert.Equal(t, []string{"T-1"}, ids(items), "the task still waits on the owner")
}

// No notify_on mode pushes a decided item, blocking or not.
func TestPushNeverSendsADecidedItem(t *testing.T) {
	start := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

	for _, modes := range []string{"[blocking]", "[ask]", "[reminder]", "[reminder, blocking, ask]"} {
		c := pushCrew(t, "notify_on: "+modes+"\n")
		st := shift.NewState()
		st.PushFrom = start

		// Even one that says it blocks, by hand or by a bug, and one that says it is a reminder's.
		decided := []todoItem{
			{ID: "A-1", Kind: "decided", Title: "a call", Created: start.Add(time.Minute)},
			{ID: "A-2", Kind: "decided", Title: "another", Blocks: "dev", Created: start.Add(time.Minute)},
			{ID: "A-3", Kind: "decided", Title: "a third", byAlarm: true, Created: start.Add(time.Minute)},
		}
		assert.Empty(t, ids(pushDue(c, st, decided, start.Add(2*time.Minute))), modes)
		assert.Empty(t, st.Pushed, modes)

		// Beside one that is due (when the mode sends any), that one alone goes.
		want := []string{"A-4"}
		if modes == "[reminder]" {
			want = []string{}
		}

		check := todoItem{ID: "A-4", Kind: "check", Title: "login", Blocks: "dev", Created: start.Add(time.Minute)}
		assert.Equal(t, want, ids(pushDue(c, st, append(decided, check), start.Add(3*time.Minute))), modes)
	}
}

// No plugin is told of a decided item: it is on no card, and a turn that put one on the todo still ended on its own words.
func TestPluginsAreNotToldOfDecided(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "todo", "decided", "keep the old flag name", "--reason", "links point at it", "--from", "dev")
	require.NoError(t, err)

	a := &app{crew: file, out: io.Discard, err: io.Discard}
	w, err := a.pluginWorld(mustCrew(t, file), time.Now())
	require.NoError(t, err)
	assert.Empty(t, w.items)
	assert.Empty(t, w.things)

	_, _, err = zm(t, file, "todo", "add", "sign in and publish the board", "--from", "dev")
	require.NoError(t, err)

	w, err = a.pluginWorld(mustCrew(t, file), time.Now())
	require.NoError(t, err)
	assert.Len(t, w.items, 1, "an ask is told as it was")
	assert.Contains(t, w.items, "A-2")

	now := time.Now()
	rec := &shift.TurnRecord{At: now, Prev: now.Add(-time.Hour)}
	_, _, err = zm(t, file, "todo", "ignore", "A-2")
	require.NoError(t, err)
	assert.True(t, turnAsked(mustCrew(t, file), "dev", rec), "an ask the role made counts, open or closed")

	only := testCrew(t, "  dev: {}\n")
	_, _, err = zm(t, only, "todo", "decided", "a call", "--reason", "why", "--from", "dev")
	require.NoError(t, err)
	assert.False(t, turnAsked(mustCrew(t, only), "dev", rec), "a decided item is no card, so no question was asked")
}

// zmina show says a decided item as it is: decided by its role, and the two ways to answer it.
func TestShowSaysDecided(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "todo", "decided", "keep the old flag name", "--reason", "links point at it", "--instead",
		"rename it", "--title", "old flag name", "--from", "dev", "--task", "T-4")
	require.NoError(t, err)

	out, _, err := zm(t, file, "show", "a-1")
	require.NoError(t, err)

	for _, want := range []string{
		"A-1 · old flag name\n",
		"  decided by dev · to review · 0m · about T-4\n",
		"  decided: keep the old flag name\n",
		"  reason: links point at it\n",
		"  instead: rename it\n",
		"  act: zmina todo done A-1 (fine) / zmina todo answer A-1 \"<what to redo>\"\n",
	} {
		assert.Contains(t, out, want)
	}

	assert.NotContains(t, out, "waits on the owner")

	_, _, err = zm(t, file, "todo", "done", "A-1")
	require.NoError(t, err)

	out, _, err = zm(t, file, "show", "A-1")
	require.NoError(t, err)
	assert.Contains(t, out, "decided by dev · reviewed · ")
	assert.NotContains(t, out, "act: zmina todo done")
}

// On the page a decided item is in the todo as the kind decided and holds nothing, even about a waiting task; it is not
// counted as waiting on the owner, and is not the need of the work it is about.
func TestUIDecidedHoldsNothingAndIsNotCounted(t *testing.T) {
	uiHome(t)

	ops := crewPathOf(t, "ops")

	_, _, err := zm(t, ops, "task", "new", "dev", "ship", "it")
	require.NoError(t, err)
	_, _, err = zm(t, ops, "task", "set", "T-1", "waiting", "--note", "which region?")
	require.NoError(t, err)
	_, _, err = zm(t, ops, "todo", "decided", "ship it to staging first", "--reason", "cheaper to undo", "--instead",
		"ship to prod", "--task", "T-1")
	require.NoError(t, err)

	srv, _ := uiServer(t, true, "")

	resp, err := http.Get(srv.URL + "/api/state")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	var snap uiSnapshot
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&snap))
	require.Len(t, snap.Todo, 2)

	assert.Equal(t, "T-1", snap.Todo[0].ID)
	assert.True(t, snap.Todo[0].Holds, "the waiting task holds as it did")
	assert.Equal(t, "A-1", snap.Todo[1].ID, "the decided item is last")
	assert.Equal(t, "decided", snap.Todo[1].Kind)
	assert.False(t, snap.Todo[1].Holds, "a decided item holds nothing")
	assert.Equal(t, "cheaper to undo", snap.Todo[1].Reason)
	assert.Equal(t, "ship to prod", snap.Todo[1].Instead)

	found := false

	for _, c := range snap.Crews {
		if c.Key == "ops" {
			found = true

			assert.Equal(t, 1, c.Waiting, "only the task waits on the owner")
		}
	}

	require.True(t, found, "the crew is in the snapshot")

	require.Len(t, snap.Work, 1)
	require.NotNil(t, snap.Work[0].Needs)
	assert.Equal(t, "T-1", snap.Work[0].Needs.ID, "the task's own item, not the decided one")

	// The card of a decided item offers Done, as a check does.
	d := askDecision(asks.Ask{ID: "A-1", Kind: asks.Decided})
	assert.Equal(t, "done", d.Action)

	// A decided item is never late on its card, however old; a check is.
	now := time.Now()
	old := now.Add(-3 * 24 * time.Hour)
	x := &itemIndex{now: now, asks: map[string]asks.Ask{
		"A-1": {ID: "A-1", Kind: asks.Decided, State: asks.Open, Created: old},
		"A-2": {ID: "A-2", Kind: asks.Check, State: asks.Open, Created: old},
	}}
	assert.False(t, x.card("A-1").Overdue)
	assert.True(t, x.card("A-2").Overdue)
	assert.Equal(t, "need", x.card("A-1").State)
}
