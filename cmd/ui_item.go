package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// An id in a message is a task (T-n), a parked item (L-n) or an ask (A-n) of the crew the conversation is with. Chat
// asks for every id its messages name at once, and opens one as a card (design D-15).

// itemIDsMax is how many ids one request resolves.
const itemIDsMax = 200

// chainMax bounds a chain's walk: a task parked and taken again is one piece of work, T → L → T → …
const chainMax = 40

var itemIDRe = regexp.MustCompile(`^[TLA]-[0-9]{1,7}$`)

// itemStep is one item of a chain as the card shows it.
type itemStep struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // task | later
	// Title is a task's headline (its title, else the start of what was asked) or a parked item's text.
	Title   string `json:"title"`
	State   string `json:"state"`
	Role    string `json:"role,omitempty"`
	On      string `json:"on,omitempty"`
	Project string `json:"project,omitempty"`
	// Note is a task's latest milestone, or a parked item's note; Result the summary of a task's result.
	Note    string    `json:"note,omitempty"`
	Result  string    `json:"result,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// itemAsk is an ask as the card shows it.
type itemAsk struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // decision | check
	Title   string    `json:"title"`
	Text    string    `json:"text"`
	Rec     string    `json:"rec,omitempty"`
	Role    string    `json:"role,omitempty"`
	Task    string    `json:"task,omitempty"`
	Project string    `json:"project,omitempty"`
	Open    bool      `json:"open"`
	Created time.Time `json:"created"`
}

// itemDecision is the one answer the card offers, only while the item waits on the owner: the page sends it as its
// todo answer would (POST /api/todo).
type itemDecision struct {
	// ID is the todo item answered: the ask, or a task waiting on the owner without one.
	ID     string `json:"id"`
	Action string `json:"action"` // yes | done | answer
	// Text is what an answer says; Word what the button says (the recommendation, Mark checked, Yes).
	Text string `json:"text,omitempty"`
	Word string `json:"word"`
}

// itemCard is one id resolved.
type itemCard struct {
	ID    string `json:"id"`
	Found bool   `json:"found"`
	Kind  string `json:"kind,omitempty"` // task | later | ask
	// Chain is the piece of work, its root (the owner's ask) first and its head (the newest item) last; At is the
	// id's place in it. An ask has none.
	Chain []itemStep `json:"chain,omitempty"`
	At    int        `json:"at"`
	Ask   *itemAsk   `json:"ask,omitempty"`
	// State is the head's in a word the page puts in plain words: need (waiting on the owner), busy (its role works on
	// it), with (taken, its role not working now), queued (handed, not taken yet), on (waiting on Who), parked, done,
	// dropped.
	State string `json:"state,omitempty"`
	// Who is whom it is with: the role in work, the one it waits on, the role it was parked from, an ask's kind.
	Who   string    `json:"who,omitempty"`
	Since time.Time `json:"since,omitzero"`
	// Idle is set with "with" while its role is up and idle (since Since).
	Idle bool `json:"idle,omitempty"`
	// Overdue is an item waiting on the owner for over 24 h.
	Overdue  bool          `json:"overdue,omitempty"`
	Decision *itemDecision `json:"decision,omitempty"`
	// Talk is the role a conversation about it goes to; Back the parked head «Bring back» names.
	Talk string `json:"talk,omitempty"`
	Back string `json:"back,omitempty"`
}

// itemIndex is a crew's ledger read once for many ids.
type itemIndex struct {
	tasks    map[string]task.Task
	later    map[string]later.Item
	asks     map[string]asks.Ask
	from     map[string]string // a task taken from a parked item → that item
	todo     map[string]todoItem
	roles    map[string]shift.View
	dispatch string
	stateDir string
	now      time.Time
}

func readItemIndex(s *shift.Shift) *itemIndex {
	x := &itemIndex{tasks: map[string]task.Task{}, later: map[string]later.Item{}, asks: map[string]asks.Ask{},
		from: map[string]string{}, todo: map[string]todoItem{}, roles: map[string]shift.View{},
		dispatch: s.Crew.Dispatcher, stateDir: s.Crew.StateDir(), now: s.Now()}

	tasks, _, _ := taskStore(s.Crew).Scan()
	for _, t := range tasks {
		x.tasks[t.ID] = t
	}

	items, _, _ := laterStore(s.Crew).Scan()
	for _, it := range items {
		x.later[it.ID] = it
		if it.Task != "" {
			x.from[it.Task] = it.ID
		}
	}

	all, _, _ := askStore(s.Crew).Scan()
	for _, a := range all {
		x.asks[a.ID] = a
	}

	views := s.Views()
	for _, v := range views {
		x.roles[v.Role] = v
	}

	todos, _ := todo(s.Crew, x.now, views)
	for _, it := range todos {
		// An ask about a task is that task's question; a task waiting on the owner is its own item.
		if it.Task != "" {
			if _, ok := x.todo[it.Task]; !ok || it.Kind != "waiting" {
				x.todo[it.Task] = it
			}
		}
	}

	return x
}

// prev and next are the chain's links: a task taken from a parked item and the item a task was parked as; a parked
// item's task it came from and the one it became.
func (x *itemIndex) prev(id string) string {
	if strings.HasPrefix(id, "T-") {
		return x.from[id]
	}

	return x.later[id].From
}

func (x *itemIndex) next(id string) string {
	if strings.HasPrefix(id, "T-") {
		return x.tasks[id].Later
	}

	return x.later[id].Task
}

func (x *itemIndex) known(id string) bool {
	if strings.HasPrefix(id, "T-") {
		_, ok := x.tasks[id]
		return ok
	}

	_, ok := x.later[id]

	return ok
}

// chain walks from id back to its root and on to its head; a link to an item that does not read ends the walk.
func (x *itemIndex) chain(id string) (ids []string, at int) {
	seen := map[string]bool{id: true}

	var back []string
	for p := x.prev(id); p != "" && x.known(p) && !seen[p] && len(back) < chainMax; p = x.prev(p) {
		seen[p] = true
		back = append(back, p)
	}

	for i := len(back) - 1; i >= 0; i-- {
		ids = append(ids, back[i])
	}

	at = len(ids)
	ids = append(ids, id)

	for n := x.next(id); n != "" && x.known(n) && !seen[n] && len(ids) < 2*chainMax; n = x.next(n) {
		seen[n] = true
		ids = append(ids, n)
	}

	return ids, at
}

func (x *itemIndex) step(id string) itemStep {
	if t, ok := x.tasks[id]; ok {
		st := itemStep{ID: id, Kind: "task", Title: t.Headline(), State: string(t.State), Role: t.Role, On: t.On,
			Project: t.Where(), Note: t.Note, Created: t.Created, Updated: t.Updated}
		if t.Result != nil {
			if d, err := record.Load(x.stateDir, "result", t.ID, t.Result.Revision); err == nil {
				st.Result = d.Values["summary"]
			}
		}

		return st
	}

	it := x.later[id]

	return itemStep{ID: id, Kind: "later", Title: it.Text, State: string(it.State), Role: it.Role, Note: it.Note,
		Created: it.Created, Updated: it.Created}
}

// card resolves one id.
func (x *itemIndex) card(id string) itemCard {
	id = strings.ToUpper(id)
	c := itemCard{ID: id}

	if strings.HasPrefix(id, "A-") {
		a, ok := x.asks[id]
		if !ok {
			return c
		}

		c.Found, c.Kind, c.Talk, c.Since = true, "ask", a.Role, a.Created
		c.Ask = &itemAsk{ID: a.ID, Kind: string(a.Kind), Title: a.Title, Text: a.Text, Rec: a.Rec, Role: a.Role,
			Task: a.Task, Project: a.Project, Open: a.Open(), Created: a.Created}
		if c.Ask.Project == "" && a.Task != "" {
			c.Ask.Project = x.tasks[a.Task].Where()
		}

		c.Who = "a " + string(a.Kind)

		switch {
		case a.Open():
			c.State, c.Decision = "need", askDecision(a)
			c.Overdue = x.now.Sub(a.Created) > overdueAfter
		case a.State == asks.Parked:
			c.State, c.Since = "parked", a.Updated
		default:
			c.State, c.Since = "done", a.Updated
		}

		return c
	}

	if !x.known(id) {
		return c
	}

	ids, at := x.chain(id)
	c.Found, c.At = true, at
	c.Kind = map[bool]string{true: "task", false: "later"}[strings.HasPrefix(id, "T-")]

	for _, s := range ids {
		c.Chain = append(c.Chain, x.step(s))
	}

	x.headState(&c)

	return c
}

// headState says where the chain's head stands and what the owner can do about it.
func (x *itemIndex) headState(c *itemCard) {
	head := c.Chain[len(c.Chain)-1]
	c.Talk, c.Since = head.Role, head.Updated

	if head.Kind == "later" {
		it := x.later[head.ID]
		c.Who = it.Role

		switch it.State {
		case later.Done:
			c.State = "done"
		case later.Dropped:
			c.State = "dropped"
		default:
			c.State, c.Back = "parked", head.ID
		}

		// Parked from no task: it was the dispatcher's to keep.
		if c.Talk == "" {
			c.Talk = x.dispatch
		}

		return
	}

	t := x.tasks[head.ID]
	c.Who = t.Role

	switch t.State {
	case task.Done:
		c.State = "done"
	case task.Dropped:
		c.State = "dropped"
	case task.Parked:
		// Its parked item does not read (else it would be the head).
		c.State = "parked"
	case task.Waiting:
		it, mine := x.todo[t.ID]

		switch {
		case mine:
			c.State, c.Who, c.Decision = "need", "", x.todoDecision(it)
			c.Overdue = x.now.Sub(it.Created) > overdueAfter
		case t.On != "":
			c.State, c.Who = "on", t.On
		default:
			// Answered: the turn is the dispatcher's.
			c.State, c.Who = "on", x.dispatch
		}
	default:
		if it, ok := x.todo[t.ID]; ok {
			// An open task with an ask about it waits on the owner all the same.
			c.State, c.Who, c.Decision = "need", "", x.todoDecision(it)
			c.Overdue = x.now.Sub(it.Created) > overdueAfter

			return
		}

		v := x.roles[t.Role]

		switch {
		case t.Delivery != nil && t.Delivery.AcceptedAt.IsZero():
			c.State = "queued"
		case v.State == shift.PresenceUp && v.Status == "busy":
			c.State = "busy"
		default:
			// Taken, and its role is not working on anything now: idle (since when it went idle) or down.
			c.State = "with"
			if v.State == shift.PresenceUp && v.Status == "idle" {
				c.Idle = true
				if !v.Since.IsZero() {
					c.Since = v.Since
				}
			}
		}
	}
}

func (x *itemIndex) todoDecision(it todoItem) *itemDecision {
	if a, ok := x.asks[it.ID]; ok {
		return askDecision(a)
	}

	return &itemDecision{ID: it.ID, Action: "answer", Text: "Yes", Word: "Yes"}
}

// askDecision is an open ask's one answer: its recommendation, a check's Mark checked, else Yes.
func askDecision(a asks.Ask) *itemDecision {
	switch {
	case a.Kind == asks.Check:
		return &itemDecision{ID: a.ID, Action: "done", Word: "Mark checked"}
	case a.Rec != "":
		return &itemDecision{ID: a.ID, Action: "yes", Word: a.Rec}
	}

	return &itemDecision{ID: a.ID, Action: "answer", Text: "Yes", Word: "Yes"}
}

// itemIDs reads a request's comma-separated ids: each one T-n, L-n or A-n, at most itemIDsMax, no repeats.
func itemIDs(list string) ([]string, error) {
	var ids []string

	seen := map[string]bool{}

	for f := range strings.SplitSeq(list, ",") {
		id := strings.ToUpper(strings.TrimSpace(f))
		if id == "" || seen[id] {
			continue
		}

		if !itemIDRe.MatchString(id) {
			return nil, errors.New("ids: T-n, L-n or A-n, comma-separated")
		}

		seen[id] = true
		ids = append(ids, id)
	}

	if len(ids) == 0 || len(ids) > itemIDsMax {
		return nil, errors.New("ids: 1 to 200")
	}

	return ids, nil
}

// localItems resolves ids against one of this node's crews (and a peer cockpit's item/read), as JSON
// {"items": {id: card}}.
func (a *app) localItems(key, list string) ([]byte, error) {
	ids, err := itemIDs(list)
	if err != nil {
		return nil, err
	}

	c, err := a.crewApp(key)
	if err != nil {
		return nil, err
	}

	out := map[string]itemCard{}

	err = c.readState(func(s *shift.Shift) error {
		x := readItemIndex(s)
		for _, id := range ids {
			out[id] = x.card(id)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]any{"items": out})
}

func (a *app) uiItems(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		var (
			data []byte
			err  error
		)

		if node := q.Get("node"); node != "" && !a.isSelf(node) {
			ctx, cancel := context.WithTimeout(r.Context(), peerActTimeout)
			defer cancel()

			var me *mesh.Node
			if me, err = openNode(); err == nil {
				data, err = me.ItemRead(ctx, mesh.Exec{}, node, q.Get("crew"), q.Get("ids"))
			}
		} else {
			data, err = a.localItems(q.Get("crew"), q.Get("ids"))
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
}

// isSelf is true when node names this node: Chat names a local crew with the node's own name too.
func (a *app) isSelf(node string) bool {
	me, err := openNode()

	return err == nil && me.Name == node
}

// ownCrewKey reads a crew key that names this node's own crew as <this node>/<crew> (Chat names a local crew with the
// node's name too) as the crew's key here.
func ownCrewKey(key string) string {
	node, crew, ok := strings.Cut(key, "/")
	if !ok || node == "" || crew == "" {
		return key
	}

	if me, err := openNode(); err == nil && me.Name == node {
		return crew
	}

	return key
}
