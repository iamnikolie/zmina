package cmd

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

// The owner answers another node's question from this node's cockpit: the answer is done there, by that crew's own
// rules, and reaches that crew's dispatcher as the page there would send it.
func TestCockpitAnswersAPeersItem(t *testing.T) {
	homeA := uiHome(t)
	from := crewPathOf(t, "default")
	_, _, err := zm(t, from, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)
	far := farBox(t, homeA, from)

	out, _, err := zm(t, far, "todo", "add", "Ship it?", "--rec", "yes", "--reason", "tests are green")
	require.NoError(t, err)
	id := strings.TrimSpace(out)

	node, crew, ok := peerCrew("box/ops")
	require.True(t, ok)
	assert.Equal(t, []string{"box", "ops"}, []string{node, crew})
	_, _, ok = peerCrew("laptop/ops")
	assert.False(t, ok, "this node's own crew is not a peer's")
	_, _, ok = peerCrew("ops")
	assert.False(t, ok)

	a := &app{out: io.Discard, err: io.Discard}
	line, err := a.peerAct(context.Background(), "box", "todo/act", mesh.TerminalRequest{Crew: "ops", Action: "yes", ID: id})
	require.NoError(t, err)
	assert.Contains(t, line, id+" · yes")

	st, err := shift.Read(filepath.Dir(far))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(bodies(st.Signals), "\n"), id+" · yes: yes")

	_, err = a.peerAct(context.Background(), "box", "todo/act", mesh.TerminalRequest{Crew: "ops", Action: "rm -rf", ID: id})
	require.ErrorContains(t, err, "action:")
	_, err = a.peerAct(context.Background(), "box", "todo/act", mesh.TerminalRequest{Crew: "nope", Action: "yes", ID: id})
	require.ErrorContains(t, err, "no crew")
}
