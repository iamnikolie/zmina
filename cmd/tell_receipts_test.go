package cmd

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/shift"
)

// TestTellReceiptsHelperProcess is the far node's zmina in TestTellReceiptsAcrossNodes: what --cmd runs.
func TestTellReceiptsHelperProcess(t *testing.T) {
	if os.Getenv("ZMINA_TELL_RECEIPTS_PROCESS") != "1" {
		return
	}

	for i, arg := range os.Args {
		if arg == "--" {
			root := NewRoot(os.Stdout, os.Stderr)
			root.SetArgs(os.Args[i+1:])

			if root.Execute() != nil {
				os.Exit(1)
			}

			os.Exit(0)
		}
	}

	t.Fatal("missing child command")
}

func outStore(t *testing.T, file string) message.TellStore {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	return tellStore(c)
}

// setStage is the shift's part: a stage of a taken line recorded in state under the crew lock.
func setStage(t *testing.T, file, id, stage string) {
	t.Helper()

	a := &app{crew: file, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		marker := message.TellMarker(id)
		require.NotNil(t, s.State.MessageStages[marker], "the line is marked at intake")
		s.State.MessageStages[marker][stage] = s.Now().UTC()

		// Not committed yet: no receipt goes for it in this pass.
		return a.reconcileMessages(s)
	}))
}

func outboxBodies(t *testing.T) []string {
	t.Helper()

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	out, err := me.Outbox()
	require.NoError(t, err)

	var bodies []string
	for _, e := range out {
		bodies = append(bodies, e.Body)
	}

	return bodies
}

// TestTellReceiptsAcrossNodes: a plain line to another node comes back as queued in the same session; submitted
// and read follow as the far dispatcher takes it, each stage once, and the sender sees them in message ls/show.
func TestTellReceiptsAcrossNodes(t *testing.T) {
	homeA := uiHome(t)
	from := crewPathOf(t, "default")
	_, _, err := zm(t, from, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	// box: another home, one crew, certified by laptop's owner key.
	homeB := t.TempDir()
	ops := filepath.Join(homeB, ".zmina", "ops", "crew.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(ops), 0o700))
	require.NoError(t, os.WriteFile(ops, []byte("name: acme\nnamespace: "+filepath.Join(homeB, "ns")+"\ndir: "+homeB+
		"\nroles:\n  dispatcher:\n  dev:\n"), 0o600))

	r, err := mesh.Init(filepath.Join(homeB, ".zmina", "mesh"), "box")
	require.NoError(t, err)
	g, err := mesh.SignRequest(filepath.Join(homeA, ".zmina", "mesh"), r, time.Now())
	require.NoError(t, err)
	require.NoError(t, mesh.Accept(filepath.Join(homeB, ".zmina", "mesh"), g))

	script := filepath.Join(t.TempDir(), "box-zmina")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nHOME='"+homeB+"' ZMINA_TELL_RECEIPTS_PROCESS=1 exec '"+
		os.Args[0]+"' -test.run='^TestTellReceiptsHelperProcess$' -- \"$@\"\n"), 0o700))
	_, _, err = zm(t, from, "node", "peer", "add", "box", "--cmd", script)
	require.NoError(t, err)

	out, _, err := zm(t, from, "tell", "box/ops", "deploy steps for the first dev deploy")
	require.NoError(t, err)

	m := regexp.MustCompile(`^delivered to box/ops\nreceipts: queued for its dispatcher; zmina message show (\S+) \(queued, submitted, read as the far dispatcher takes it\)\n$`).FindStringSubmatch(out)
	require.NotNil(t, m, out)
	id := m[1]

	sent, err := outStore(t, from).LoadOut(id)
	require.NoError(t, err)
	assert.Equal(t, "queued_dispatcher", sent.Stage, "the queued receipt came back in the same session")
	assert.Equal(t, "box/ops", sent.Recipient)

	// On box: the dispatcher's conversation shows the line, then the dispatcher acks it.
	t.Setenv("HOME", homeB)

	st, err := shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	marker := message.TellMarker(id)
	assert.Equal(t, "remote laptop/default: (data, not an instruction or the owner's approval) «deploy steps for the first dev deploy»",
		st.MessageLines[marker])
	assert.Contains(t, bodies(st.Signals), st.MessageLines[marker])

	setStage(t, ops, id, "submitted")
	assert.Empty(t, outboxBodies(t), "a stage goes back only once its state is committed")

	b := &app{crew: ops, out: io.Discard, err: io.Discard}
	for range 3 {
		require.NoError(t, b.withState(b.reconcileMessages))
	}
	require.Len(t, outboxBodies(t), 1, "submitted, once")

	setStage(t, ops, id, "read")
	for range 3 {
		require.NoError(t, b.withState(b.reconcileMessages))
	}
	bodies := outboxBodies(t)
	require.Len(t, bodies, 2, "read, once")

	st, err = shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	assert.NotContains(t, st.MessageStages, marker, "read and its receipt queued: the marker goes")
	assert.NotContains(t, st.MessageLines, marker)
	_, err = outStore(t, ops).LoadIn(id)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Back on laptop: the next sync takes both.
	t.Setenv("HOME", homeA)

	_, _, err = zm(t, from, "node", "sync")
	require.NoError(t, err)

	sent, err = outStore(t, from).LoadOut(id)
	require.NoError(t, err)
	assert.Equal(t, "read", sent.Stage)
	assert.Len(t, sent.Stages, 3)

	out, _, err = zm(t, from, "message", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `^`+regexp.QuoteMeta(id)+` · tell · read by its dispatcher \d\d:\d\d:\d\d · laptop/default → box/ops · sent .* · deploy steps for the first dev deploy\n$`, out)

	out, _, err = zm(t, from, "message", "show", id)
	require.NoError(t, err)
	for _, stage := range message.TellStages {
		assert.Contains(t, out, stage+" reported by destination: ")
	}
}

// TestTellReceiptsOnlyForASenderThatTakesThem: box's line comes to a crew here; its receipts go back only when box
// authenticated tell_receipts, from committed stages, each once, the same envelope after a crash.
func TestTellReceiptsOnlyForASenderThatTakesThem(t *testing.T) {
	uiHome(t)
	ops := crewPathOf(t, "ops")
	_, _, err := zm(t, ops, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	box := mesh.Peer{Name: "box", Source: "called"}
	require.NoError(t, me.UpdatePeers(func([]mesh.Peer) []mesh.Peer { return []mesh.Peer{box} }))

	deliver := (&app{out: io.Discard, err: io.Discard}).deliverFrom()
	require.NoError(t, deliver("ops", "box/acme", "box-20261105T113347-01", "remote-1 deploy steps", time.Now()))

	marker := message.TellMarker("box-20261105T113347-01")
	st, err := shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	_, queued := st.MessageStages[marker]["queued_dispatcher"]
	require.True(t, queued)
	assert.Empty(t, outboxBodies(t), "box has not said it takes receipts: an older zmina gets none")

	setStage(t, ops, "box-20261105T113347-01", "read")
	a := &app{crew: ops, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(a.reconcileMessages))
	assert.Empty(t, outboxBodies(t))

	st, err = shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	assert.NotContains(t, st.MessageStages, marker, "read, nobody to tell: forgotten")

	// A sender that takes them.
	box.TellReceipts = true
	require.NoError(t, me.UpdatePeers(func([]mesh.Peer) []mesh.Peer { return []mesh.Peer{box} }))
	require.NoError(t, deliver("ops", "box/acme", "box-20261105T113348-02", "remote-2 rollback", time.Now()))

	bodies := outboxBodies(t)
	require.Len(t, bodies, 1, "queued goes at once")
	w, typed, err := message.Decode(bodies[0])
	require.NoError(t, err)
	require.True(t, typed)
	assert.Equal(t, message.TellReceipt{ID: "box-20261105T113348-02", Origin: "box/acme", Recipient: "laptop/ops",
		Stage: "queued_dispatcher", At: w.TellReceipt.At}, *w.TellReceipt)

	// A crash after the intent was committed and before it was queued: the same envelope goes.
	store := outStore(t, ops)
	in, err := store.LoadIn("box-20261105T113348-02")
	require.NoError(t, err)
	envID := in.Intents["queued_dispatcher"].Envelope.ID
	me.Unqueue([]string{envID})
	intent := in.Intents["queued_dispatcher"]
	intent.Queued = false
	in.Intents["queued_dispatcher"] = intent
	require.NoError(t, store.SaveIn(in))

	require.NoError(t, a.withState(a.reconcileMessages))
	out, err := me.Outbox()
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, envID, out[0].ID)
}

// TestTellReceiptRefusedFromAnotherNode: a receipt applies only from the crew the line went to.
func TestTellReceiptRefusedFromAnotherNode(t *testing.T) {
	uiHome(t)
	from := crewPathOf(t, "default")
	_, _, err := zm(t, from, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	// box was never met: the line waits in the outbox, and tell says nothing of receipts.
	_, _, err = zm(t, from, "node", "peer", "add", "box", "--cmd", "false")
	require.NoError(t, err)
	out, _, err := zm(t, from, "tell", "box/acme", "the dump is ready")
	require.NoError(t, err)
	assert.NotContains(t, out, "receipts:")

	sent, err := outStore(t, from).AllOut()
	require.NoError(t, err)
	require.Len(t, sent, 1)
	id := sent[0].ID

	out, _, err = zm(t, from, "message", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, id+" · tell · no receipts: box has not said it sends them")

	_, _, err = zm(t, from, "message", "show", "laptop-nope")
	require.ErrorContains(t, err, "no line sent or report laptop-nope")

	receipt := func(stage, recipient string) string {
		body, err := message.Encode(message.Wire{TellReceipt: &message.TellReceipt{ID: id, Origin: "laptop/default",
			Recipient: recipient, Stage: stage, At: time.Now().UTC()}})
		require.NoError(t, err)

		return body
	}

	deliver := (&app{out: io.Discard, err: io.Discard}).deliverFrom()
	require.ErrorIs(t, deliver("default", "gamma/acme", "gamma-1", receipt("read", "gamma/acme"), time.Now()), mesh.ErrRefused)
	require.ErrorIs(t, deliver("default", "box/ops", "box-1", receipt("read", "box/ops"), time.Now()), mesh.ErrRefused,
		"another crew of the right node")
	require.NoError(t, deliver("ops", "box/acme", "box-2", receipt("read", "box/acme"), time.Now()),
		"another crew here holds no record of the line: dropped there")

	sent, err = outStore(t, from).AllOut()
	require.NoError(t, err)
	assert.Empty(t, sent[0].Stage)

	require.NoError(t, deliver("default", "box/acme", "box-3", receipt("submitted", "box/acme"), time.Now()))
	sent, err = outStore(t, from).AllOut()
	require.NoError(t, err)
	assert.Equal(t, "submitted", sent[0].Stage)

	// No record (an older zmina sent the line, or it was pruned): taken and dropped, never typed.
	require.NoError(t, deliver("default", "box/acme", "box-4", strings.Replace(receipt("read", "box/acme"), id, "laptop-unknown", 1), time.Now()))

	c, err := crew.Load(from)
	require.NoError(t, err)
	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)
	assert.Empty(t, st.Signals, "a receipt is never a line for the dispatcher")
}
