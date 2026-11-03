package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/mesh"
)

// The cockpit is one page for every crew of the mesh: this node's crews, read as they are, and every peer's,
// read over the mesh in the background and cached, so a peer that is slow or down never holds the page.
const (
	// uiPeerEvery is how often a peer's crews are read; uiPeerCostEvery its spend (a transcript scan on the peer).
	uiPeerEvery     = 20 * time.Second
	uiPeerCostEvery = 3 * time.Minute
	// uiPeerStale is how old a peer's last good read may be before the page marks its crews stale.
	uiPeerStale = 2 * time.Minute
	// uiPeerTimeout bounds one read of a peer's crews; uiPeerCostTimeout one read of its spend.
	uiPeerTimeout     = 45 * time.Second
	uiPeerCostTimeout = 2 * time.Minute
	// uiDormant is how long a crew's ticker may be silent before the page marks the crew silent.
	uiDormant = time.Hour
)

// uiNode is one node of the mesh as the page shows it: this one, or a peer with when it was last read.
type uiNode struct {
	Name string `json:"name"`
	Self bool   `json:"self,omitempty"`
	// State: self, fresh (read within uiPeerStale), stale (an older read is shown), offline (never read).
	State string `json:"state"`
	// Seen is the last good read of its crews (for a peer, the later of that and the last mesh session).
	Seen  time.Time `json:"seen,omitzero"`
	Error string    `json:"error,omitempty"`
	Crews []string  `json:"crews"`
}

// uiPeerState is what the cache holds of one peer.
type uiPeerState struct {
	snap    *uiSnapshot
	cost    *uiCost
	read    time.Time // last good read of its crews
	costAt  time.Time // last good read of its spend
	tried   time.Time
	triedC  time.Time
	err     string
	busy    bool
	busyC   bool
	session time.Time // the mesh's last good session with it (peers.json)
}

// uiPeerCache reads the peers in the background. peers lists them (self is this node's name); fetch reads one
// peer's crews (kind "") or spend (kind "cost") as that node's JSON.
type uiPeerCache struct {
	mu    sync.Mutex
	state map[string]*uiPeerState
	self  string
	why   string // why no peer is read (no mesh, not the owner's node), shown on each peer
	names []string

	peers func() (self string, peers []mesh.Peer, why string)
	fetch func(ctx context.Context, node, kind string) ([]byte, error)
	now   func() time.Time
}

func newUIPeerCache() *uiPeerCache {
	return &uiPeerCache{state: map[string]*uiPeerState{}, peers: meshPeers, fetch: meshFetch, now: time.Now}
}

// meshPeers is this node's name and its address book; why says when the peers cannot be read from here.
func meshPeers() (string, []mesh.Peer, string) {
	n, err := mesh.Open(meshDir())
	if err != nil {
		return "", nil, ""
	}

	peers, _ := n.Peers()
	if !n.HoldsOwnerKey() {
		return n.Name, peers, "read from the owner's node (it holds owner.key)"
	}

	return n.Name, peers, ""
}

func meshFetch(ctx context.Context, node, kind string) ([]byte, error) {
	n, err := mesh.Open(meshDir())
	if err != nil {
		return nil, err
	}

	return n.CrewStatus(ctx, mesh.Exec{}, node, kind)
}

// refresh starts a read of every peer that is due and not already being read; it never waits for one.
func (c *uiPeerCache) refresh() {
	self, peers, why := c.peers()
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.self, c.why, c.names = self, why, nil

	for _, p := range peers {
		if p.Name == "" || p.Name == self {
			continue
		}

		c.names = append(c.names, p.Name)

		st := c.state[p.Name]
		if st == nil {
			st = &uiPeerState{}
			c.state[p.Name] = st
		}

		st.session = p.OK

		if why != "" {
			continue
		}

		if !st.busy && now.Sub(st.tried) >= uiPeerEvery {
			st.busy, st.tried = true, now
			go c.read(p.Name, "")
		}

		if !st.busyC && now.Sub(st.triedC) >= uiPeerCostEvery {
			st.busyC, st.triedC = true, now
			go c.read(p.Name, "cost")
		}
	}

	sort.Strings(c.names)
}

func (c *uiPeerCache) read(node, kind string) {
	timeout := uiPeerTimeout
	if kind == "cost" {
		timeout = uiPeerCostTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	raw, err := c.fetch(ctx, node, kind)

	var (
		snap uiSnapshot
		cost uiCost
	)

	if err == nil {
		if kind == "cost" {
			err = json.Unmarshal(raw, &cost)
		} else {
			err = json.Unmarshal(raw, &snap)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.state[node]
	if st == nil {
		return
	}

	if kind == "cost" {
		st.busyC = false
		if err == nil {
			st.cost, st.costAt = &cost, c.now()
		}

		return
	}

	st.busy = false

	if err != nil {
		st.err = err.Error()

		return
	}

	st.snap, st.read, st.err = &snap, c.now(), ""
}

// nodes is the mesh as the page shows it, and a copy of each peer's last snapshot and spend.
func (c *uiPeerCache) nodes() (string, []uiNode, map[string]*uiSnapshot, map[string]*uiCost) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	snaps, costs := map[string]*uiSnapshot{}, map[string]*uiCost{}

	var out []uiNode

	for _, name := range c.names {
		st := c.state[name]
		n := uiNode{Name: name, Crews: []string{}, Error: st.err}

		if c.why != "" {
			n.Error = c.why
		}

		n.Seen = st.read
		if st.session.After(n.Seen) && !st.read.IsZero() {
			n.Seen = st.session
		}

		switch {
		case st.snap == nil:
			n.State = "offline"
			n.Seen = st.session
		case now.Sub(st.read) > uiPeerStale:
			n.State = "stale"
		default:
			n.State = "fresh"
		}

		if st.snap != nil {
			snaps[name] = st.snap
		}

		if st.cost != nil {
			costs[name] = st.cost
		}

		out = append(out, n)
	}

	return c.self, out, snaps, costs
}

// cockpitSnapshot is this node's crews and every peer's, each peer's keys named node/crew.
func (a *app) cockpitSnapshot(peers *uiPeerCache) uiSnapshot {
	snap := a.uiSnapshot()
	if peers == nil {
		markDormant(&snap)

		return snap
	}

	peers.refresh()
	self, nodes, snaps, _ := peers.nodes()

	snap.Self = self
	if self != "" {
		local := uiNode{Name: self, Self: true, State: "self", Seen: snap.Now, Crews: []string{}}
		for i := range snap.Crews {
			snap.Crews[i].Node = self
			local.Crews = append(local.Crews, snap.Crews[i].Key)
		}

		sort.Strings(local.Crews)
		snap.Nodes = append(snap.Nodes, local)
	}

	for _, n := range nodes {
		if p := snaps[n.Name]; p != nil {
			stale := n.State != "fresh"
			for _, cv := range p.Crews {
				n.Crews = append(n.Crews, n.Name+"/"+cv.Key)
			}

			mergePeer(&snap, *p, n.Name, stale)
			sort.Strings(n.Crews)
		}

		snap.Nodes = append(snap.Nodes, n)
	}

	snap.sortAll()
	markDormant(&snap)

	return snap
}

// mergePeer adds a peer's snapshot to snap, every crew key as node/crew.
func mergePeer(snap *uiSnapshot, p uiSnapshot, node string, stale bool) {
	key := func(k string) string { return node + "/" + k }

	for _, cv := range p.Crews {
		cv.Key, cv.Node, cv.Remote, cv.Stale = key(cv.Key), node, true, stale
		snap.Crews = append(snap.Crews, cv)
	}

	for _, w := range p.Work {
		w.Crew = key(w.Crew)
		if w.Needs != nil {
			n := *w.Needs
			n.Crew = key(n.Crew)
			w.Needs = &n
		}

		snap.Work = append(snap.Work, w)
	}

	for _, t := range p.Todo {
		t.Crew = key(t.Crew)
		snap.Todo = append(snap.Todo, t)
	}

	for _, l := range p.Later {
		l.Crew = key(l.Crew)
		snap.Later = append(snap.Later, l)
	}

	for _, al := range p.Alarms {
		al.Crew = key(al.Crew)
		snap.Alarms = append(snap.Alarms, al)
	}

	for _, t := range p.Tasks {
		t.Crew = key(t.Crew)
		snap.Tasks = append(snap.Tasks, t)
	}

	for _, e := range p.Log {
		e.Crew = key(e.Crew)
		snap.Log = append(snap.Log, e)
	}

	for project, tint := range p.Tints {
		if _, ok := snap.Tints[project]; !ok {
			snap.Tints[project] = tint
		}
	}
}

// markDormant folds away a crew stopped on purpose that holds nothing waiting on the owner; a crew not stopped
// whose ticker has been silent past uiDormant stays, marked silent.
func markDormant(snap *uiSnapshot) {
	for i, c := range snap.Crews {
		silent := c.LastTick.IsZero() || snap.Now.Sub(c.LastTick) > uiDormant
		snap.Crews[i].Dormant = !c.Stopped.IsZero() && c.Waiting == 0
		snap.Crews[i].Silent = c.Stopped.IsZero() && silent
	}

	sort.SliceStable(snap.Crews, func(i, j int) bool { return !snap.Crews[i].Dormant && snap.Crews[j].Dormant })
}

// cockpitCost is this node's spend and every peer's last read, each peer's keys named node/crew.
func (a *app) cockpitCost(local uiCost, peers *uiPeerCache) uiCost {
	if peers == nil {
		return local
	}

	_, _, _, costs := peers.nodes()

	out := local
	out.Crews, out.Roles, out.Tasks = map[string]uiSpend{}, map[string]float64{}, map[string]float64{}

	for k, v := range local.Crews {
		out.Crews[k] = v
	}

	for k, v := range local.Roles {
		out.Roles[k] = v
	}

	for k, v := range local.Tasks {
		out.Tasks[k] = v
	}

	for node, c := range costs {
		out.Today += c.Today
		out.Week += c.Week

		for k, v := range c.Crews {
			out.Crews[node+"/"+k] = v
		}

		for k, v := range c.Roles {
			out.Roles[node+"/"+k] = v
		}

		for k, v := range c.Tasks {
			out.Tasks[node+"/"+k] = v
		}
	}

	return out
}

// localStatus serves crew/status to the owner's cockpit: this node's crews only, never its peers'.
func (a *app) localStatus(kind string) ([]byte, error) {
	switch kind {
	case "":
		snap := a.uiSnapshot()

		return json.Marshal(snap)
	case "cost":
		return json.Marshal(a.readUICost())
	}

	return nil, errors.New("crew status: kind is empty or cost")
}
