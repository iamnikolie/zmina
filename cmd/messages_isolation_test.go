package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/message"
)

// addLocalReport stores a report this crew sent whose envelope is not made yet (a crash before it): the next
// reconcile makes it and queues it.
func addLocalReport(t *testing.T, store message.Store, key string) message.Message {
	t.Helper()

	r := message.Report{Version: 1, Key: key, From: "local/default", To: "local/ops", Body: "body " + key, Sequence: 1, At: time.Now().UTC()}
	r.ID = message.Identity(r.From, r.To, r.Key)
	r.Revision = message.Content(r)

	m, _, err := store.Add(r)
	require.NoError(t, err)

	return m
}

func skippedEvents(t *testing.T, file string) []events.Event {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	evs, err := eventLog(c).Read(events.Filter{})
	require.NoError(t, err)

	var out []events.Event

	for _, e := range evs {
		if e.Event == "message-skipped" {
			out = append(out, e)
		}
	}

	return out
}

// One message file that does not read stops nothing else: the other messages still get their envelopes made and
// queued, the bad file is said once (an event and a warning, not one per beat), and what did not change is not
// written again.
func TestOneBadMessageFileDoesNotStopReconcileAndIsSaidOnce(t *testing.T) {
	from, _ := reportHome(t)
	store := msgStore(t, from)

	a, b, c := addLocalReport(t, store, "a"), addLocalReport(t, store, "b"), addLocalReport(t, store, "c")
	badFile := filepath.Join(store.Dir, b.ID+".json")
	good, err := os.ReadFile(badFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(badFile, []byte("{this is not a message"), 0o600))

	var warned bytes.Buffer

	origin := &app{crew: from, out: &bytes.Buffer{}, err: &warned}

	require.NoError(t, origin.withState(origin.reconcileMessages), "a bad file is not an error of the reconcile")

	for _, id := range []string{a.ID, c.ID} {
		m, err := store.Load(id)
		require.NoError(t, err)
		in := m.Revisions[m.Latest].Intents["report"]
		assert.NotEmpty(t, in.Envelope.ID, "%s got its envelope although another message is bad", id)
		assert.True(t, in.Queued, "%s was queued", id)
	}

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	out, err := me.Outbox()
	require.NoError(t, err)
	assert.Len(t, out, 2)

	assert.Contains(t, warned.String(), b.ID+".json", "the warning names the file")
	require.Len(t, skippedEvents(t, from), 1)
	assert.Contains(t, skippedEvents(t, from)[0].Detail, b.ID+".json")

	// The next beats find the same file and say nothing new; the good files are not rewritten.
	statA, err := os.Stat(filepath.Join(store.Dir, a.ID+".json"))
	require.NoError(t, err)

	warned.Reset()

	require.NoError(t, origin.withState(origin.reconcileMessages))
	require.NoError(t, origin.withState(origin.reconcileMessages))
	assert.Empty(t, warned.String(), "said once, not at every beat")
	assert.Len(t, skippedEvents(t, from), 1)

	afterA, err := os.Stat(filepath.Join(store.Dir, a.ID+".json"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(statA, afterA), "a message that did not change is not saved again")

	_, bad, err := store.Scan()
	require.NoError(t, err)
	require.Len(t, bad, 1, "the bad file is left where it is for the owner to look at")

	// Mended, it is a message again; broken again, it is said again.
	require.NoError(t, os.WriteFile(badFile, good, 0o600))
	require.NoError(t, origin.withState(origin.reconcileMessages))
	assert.Empty(t, store.Reported())

	require.NoError(t, os.WriteFile(badFile, []byte("broken once more"), 0o600))
	require.NoError(t, origin.withState(origin.reconcileMessages))
	assert.Len(t, skippedEvents(t, from), 2)

	// message ls lists the readable ones and warns of the file.
	var listed, lsWarned bytes.Buffer

	ctl := NewRoot(&listed, &lsWarned)
	ctl.SetArgs([]string{"--crew", from, "message", "ls"})
	require.NoError(t, ctl.Execute())
	assert.Contains(t, listed.String(), a.ID)
	assert.Contains(t, listed.String(), c.ID)
	assert.NotContains(t, listed.String(), b.ID)
	assert.Contains(t, lsWarned.String(), b.ID+".json")
}

// A message whose own work fails (here: an envelope id that names no file, so its transport state cannot be read) is
// skipped like a bad file: the others go on, and it is said once with the file's name.
func TestAFailingMessageIsSkippedAndSaidOnce(t *testing.T) {
	from, _ := reportHome(t)
	store := msgStore(t, from)

	good := addLocalReport(t, store, "fine")
	poison := addLocalReport(t, store, "poison")

	pm, err := store.Load(poison.ID)
	require.NoError(t, err)

	pm.Revisions[pm.Latest].Intents["report"] = message.Intent{Envelope: mesh.Envelope{ID: "not/an/id"}}
	require.NoError(t, store.Save(pm))

	var warned bytes.Buffer

	origin := &app{crew: from, out: &bytes.Buffer{}, err: &warned}
	require.NoError(t, origin.withState(origin.reconcileMessages))

	m, err := store.Load(good.ID)
	require.NoError(t, err)
	assert.True(t, m.Revisions[m.Latest].Intents["report"].Queued, "the other message was queued")

	assert.Contains(t, warned.String(), poison.ID+".json")
	require.Len(t, skippedEvents(t, from), 1)
	assert.Contains(t, skippedEvents(t, from)[0].Detail, "invalid envelope ID")

	warned.Reset()
	require.NoError(t, origin.withState(origin.reconcileMessages))
	assert.Empty(t, warned.String(), "said once")
	assert.Len(t, skippedEvents(t, from), 1)
}
