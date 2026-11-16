package cmd

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/mesh"
)

// An edit of a seat of a crew over nodes changes the file on this node only: said, naming what shows it and what flags it.
func TestSeatEditOnASpreadCrewWarnsThatItDiffers(t *testing.T) {
	file := spreadCrew(t, "alpha")

	_, errOut, err := zm(t, file, "seat", "set", "dev", "about=builds")
	require.NoError(t, err)
	assert.Contains(t, errOut, "this crew declares nodes:")
	assert.Contains(t, errOut, "this edit changed it here only")
	assert.Contains(t, errOut, "zmina crew push <node>")

	_, errOut, err = zm(t, file, "seat", "add", "qa", "--about", "tests")
	require.NoError(t, err)
	assert.Contains(t, errOut, "declares nodes:")

	_, errOut, err = zm(t, file, "seat", "rm", "qa")
	require.NoError(t, err)
	assert.Contains(t, errOut, "declares nodes:")

	// A crew that declares none says nothing.
	plain := testCrew(t, "  dev: {}\n")
	_, errOut, err = zm(t, plain, "seat", "set", "dev", "about=builds")
	require.NoError(t, err)
	assert.NotContains(t, errOut, "nodes:")
}

// The address book says which nodes lately told a role up: only the crew's own, only the lately told, and a peer that told
// no held roles tells nothing.
func TestPeersHeldReadsTheBook(t *testing.T) {
	w := newPushWorld(t)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	now := time.Now()
	cert := w.cert

	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		return append(ps[:0],
			mesh.Peer{Name: "beta", Cert: &cert, CrewsAt: now.Add(-time.Minute), Crews: []mesh.CrewHealth{
				{Crew: "ops", Held: []string{"dev", "docs"}}, {Crew: "other", Held: []string{"zzz"}}}},
			mesh.Peer{Name: "gamma", Cert: &cert, CrewsAt: now.Add(-time.Hour), Crews: []mesh.CrewHealth{{Crew: "ops", Held: []string{"dev"}}}},
			mesh.Peer{Name: "delta", Cert: &cert, CrewsAt: now.Add(-time.Minute), Crews: []mesh.CrewHealth{{Crew: "ops"}}},
			mesh.Peer{Name: "eps", Cert: &cert, Crews: []mesh.CrewHealth{{Crew: "ops", Held: []string{"dev"}}}})
	}))

	got := peersHeld("ops", now)
	assert.Equal(t, map[string][]string{"dev": {"beta"}, "docs": {"beta"}}, got, "a stale word and a peer that never told are no evidence")

	assert.Empty(t, peersHeld("nonesuch", now))
}

// The ticker's shift of a crew over nodes reads that book; one of a crew that is not spread never does.
func TestShiftOfASpreadCrewAsksTheBook(t *testing.T) {
	spread := spreadCrew(t, "alpha")
	a := &app{out: io.Discard, err: io.Discard, crew: spread}

	s, err := a.newShift()
	require.NoError(t, err)
	assert.NotNil(t, s.PeerHeld)

	plain := testCrew(t, "  dev: {}\n")
	b := &app{out: io.Discard, err: io.Discard, crew: plain}

	s, err = b.newShift()
	require.NoError(t, err)
	assert.Nil(t, s.PeerHeld)
}
