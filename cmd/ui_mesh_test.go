package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/mesh"
)

// fakeMesh is a mesh of this node (node-a) and peers whose reads the test decides.
type fakeMesh struct {
	mu    sync.Mutex
	now   time.Time
	why   string
	reads map[string]int
	fetch func(node, kind string) ([]byte, error)
}

func (f *fakeMesh) cache() *uiPeerCache {
	c := newUIPeerCache()
	c.now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	c.peers = func() (string, []mesh.Peer, string) {
		return "node-a", []mesh.Peer{{Name: "node-b"}, {Name: "node-c", OK: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}, f.why
	}
	c.fetch = func(ctx context.Context, node, kind string) ([]byte, error) {
		f.mu.Lock()
		f.reads[node+"/"+kind]++
		f.mu.Unlock()

		return f.fetch(node, kind)
	}

	return c
}

func cockpitServer(t *testing.T, peers *uiPeerCache) *httptest.Server {
	t.Helper()

	a := &app{out: io.Discard, err: io.Discard, cockpitPeers: peers}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = a.uiHandler(srv.Listener.Addr().String(), true, "")
	srv.Start()
	t.Cleanup(srv.Close)

	return srv
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()

	resp, err := http.Get(url)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.NoError(t, json.NewDecoder(resp.Body).Decode(v))
}

func snapKeys(s uiSnapshot) []string {
	var keys []string
	for _, c := range s.Crews {
		keys = append(keys, c.Key)
	}

	return keys
}

func TestCockpitMergesPeersAndNeverWaitsForOne(t *testing.T) {
	uiHome(t)

	_, _, err := zm(t, crewPathOf(t, "ops"), "todo", "add", "--blocks", "dev", "rotate the key")
	require.NoError(t, err)

	// The peer's own answer to crew/status: its local crews, here the same two, live.
	a := &app{out: io.Discard, err: io.Discard}
	var peer uiSnapshot
	raw, err := a.localStatus("")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &peer))
	require.Len(t, peer.Crews, 2)
	for i := range peer.Crews {
		peer.Crews[i].LastTick = time.Now()
	}
	raw, _ = json.Marshal(peer)

	_, err = a.localStatus("anything")
	require.Error(t, err)

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	f := &fakeMesh{now: time.Now(), reads: map[string]int{}, fetch: func(node, kind string) ([]byte, error) {
		switch {
		case node == "node-c":
			<-block // a peer that never answers

			return nil, errors.New("ssh: connect timed out")
		case kind == "cost":
			return json.Marshal(uiCost{Ready: true, Today: 1.5, Week: 4, Crews: map[string]uiSpend{"ops": {Today: 1.5, Week: 4}},
				Roles: map[string]float64{"ops/dev": 1.5}, Tasks: map[string]float64{}})
		}

		return raw, nil
	}}
	srv := cockpitServer(t, f.cache())

	var snap uiSnapshot

	start := time.Now()
	getJSON(t, srv.URL+"/api/state", &snap)
	assert.Less(t, time.Since(start), 3*time.Second, "a peer that never answers does not hold the page")

	require.Eventually(t, func() bool {
		snap = uiSnapshot{}
		getJSON(t, srv.URL+"/api/state", &snap)

		return len(snap.Crews) == 4
	}, 5*time.Second, 50*time.Millisecond)

	assert.Equal(t, "node-a", snap.Self)
	assert.ElementsMatch(t, []string{"node-b/default", "node-b/ops", "default", "ops"}, snapKeys(snap))

	for _, c := range snap.Crews {
		if c.Node == "node-b" {
			assert.True(t, c.Remote)
			assert.False(t, c.Dormant, "its ticker beats")
			assert.False(t, c.Stale)
		} else {
			assert.Equal(t, "node-a", c.Node)
			assert.False(t, c.Remote)
			assert.False(t, c.Dormant, "not stopped on purpose: stays")
			assert.True(t, c.Silent, "no ticker beat here")
		}
	}

	var crews []string
	for _, item := range snap.Todo {
		crews = append(crews, item.Crew)
	}
	assert.ElementsMatch(t, []string{"ops", "node-b/ops"}, crews, "each item labelled with its node/crew")

	require.Len(t, snap.Nodes, 3)
	assert.Equal(t, uiNode{Name: "node-a", Self: true, State: "self", Seen: snap.Nodes[0].Seen, Crews: []string{"default", "ops"}}, snap.Nodes[0])
	assert.Equal(t, "node-b", snap.Nodes[1].Name)
	assert.Equal(t, "fresh", snap.Nodes[1].State)
	assert.ElementsMatch(t, []string{"node-b/default", "node-b/ops"}, snap.Nodes[1].Crews)
	assert.Equal(t, "node-c", snap.Nodes[2].Name)
	assert.Equal(t, "offline", snap.Nodes[2].State)
	assert.Equal(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), snap.Nodes[2].Seen.UTC(), "last mesh session")

	var spend uiCost
	require.Eventually(t, func() bool {
		spend = uiCost{}
		getJSON(t, srv.URL+"/api/cost", &spend)

		return spend.Crews["node-b/ops"].Week == 4
	}, 5*time.Second, 50*time.Millisecond)
	assert.InDelta(t, 1.5, spend.Roles["node-b/ops/dev"], 1e-9)

	// Two minutes on with the peer unread: its crews stay, marked stale.
	f.mu.Lock()
	f.now = f.now.Add(uiPeerStale + time.Second)
	f.fetch = func(string, string) ([]byte, error) { return nil, errors.New("no route") }
	f.mu.Unlock()

	require.Eventually(t, func() bool {
		snap = uiSnapshot{}
		getJSON(t, srv.URL+"/api/state", &snap)

		return len(snap.Nodes) == 3 && snap.Nodes[1].State == "stale" && snap.Nodes[1].Error == "no route"
	}, 5*time.Second, 50*time.Millisecond)

	for _, c := range snap.Crews {
		if c.Node == "node-b" {
			assert.True(t, c.Stale)
		}
	}
}

func TestCockpitOffTheOwnersNodeReadsNoPeer(t *testing.T) {
	uiHome(t)

	f := &fakeMesh{now: time.Now(), why: "read from the owner's node", reads: map[string]int{},
		fetch: func(string, string) ([]byte, error) { return nil, errors.New("must not be called") }}
	srv := cockpitServer(t, f.cache())

	var snap uiSnapshot
	getJSON(t, srv.URL+"/api/state", &snap)
	time.Sleep(100 * time.Millisecond)

	assert.Empty(t, f.reads)
	require.Len(t, snap.Nodes, 3)
	assert.Equal(t, "offline", snap.Nodes[1].State)
	assert.Equal(t, "read from the owner's node", snap.Nodes[1].Error)
	assert.Len(t, snap.Crews, 2)
}

func TestCockpitWithoutMeshIsThisNodeOnly(t *testing.T) {
	uiHome(t)

	srv, _ := uiServer(t, true, "")

	var snap uiSnapshot
	getJSON(t, srv.URL+"/api/state", &snap)

	assert.Empty(t, snap.Self)
	assert.Empty(t, snap.Nodes)
	assert.Len(t, snap.Crews, 2)
}

func TestOnlyACrewStoppedOnPurposeLeavesAll(t *testing.T) {
	now := time.Now()
	snap := uiSnapshot{Now: now, Crews: []uiCrewView{
		{Key: "stopped", Stopped: now.Add(-time.Hour), LastTick: now.Add(-time.Hour)},
		{Key: "live", LastTick: now.Add(-time.Minute)},
		{Key: "silent", LastTick: now.Add(-uiDormant - time.Minute)},
		{Key: "stopped-but-asks", Stopped: now.Add(-time.Hour), Waiting: 1},
	}}

	markDormant(&snap)

	got := map[string][2]bool{}
	for _, c := range snap.Crews {
		got[c.Key] = [2]bool{c.Dormant, c.Silent}
	}

	assert.Equal(t, map[string][2]bool{"stopped": {true, false}, "live": {false, false}, "silent": {false, true},
		"stopped-but-asks": {false, false}}, got)
	assert.Equal(t, "stopped", snap.Crews[len(snap.Crews)-1].Key, "folded crews last")
}
