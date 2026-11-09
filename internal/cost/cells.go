package cost

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
)

// Cell is the replies that share everything a view groups by, summed: the smallest piece of a usage table, and
// what a peer sends for its crews.
type Cell struct {
	Node string `json:"node,omitempty"`
	Crew string `json:"crew,omitempty"`
	Role string `json:"role,omitempty"`
	// Task is an Attribution's key; How how it was decided.
	Task    string `json:"task,omitempty"`
	How     string `json:"how,omitempty"`
	Model   string `json:"model,omitempty"`
	Day     string `json:"day,omitempty"`
	Trigger string `json:"trigger,omitempty"`

	Turns int `json:"turns"`
	// Sub is how many of the turns were a subagent's.
	Sub     int `json:"sub,omitempty"`
	Input   int `json:"input"`
	Write   int `json:"write,omitempty"`
	Write5m int `json:"write_5m"`
	Write1h int `json:"write_1h"`
	Read    int `json:"read"`
	Output  int `json:"output"`
	// Cost is the priced turns' dollars; Unpriced how many turns had no price.
	Cost     float64 `json:"cost"`
	Unpriced int     `json:"unpriced,omitempty"`
	// Waits are the turns of polling chains past their first wait and look (registry.Message.Wait): foreground waits
	// with at most three calls between them, the turns a background wait would not take. WaitCost is their dollars.
	Waits    int     `json:"waits,omitempty"`
	WaitCost float64 `json:"wait_cost,omitempty"`
}

func (c *Cell) add(m registry.Message, usd float64, priced bool) {
	c.Turns++
	c.Input += m.Input
	c.Write += m.Write
	c.Write5m += m.Write5m
	c.Write1h += m.Write1h
	c.Read += m.Read
	c.Output += m.Output

	if m.Sub {
		c.Sub++
	}

	if m.Wait {
		c.Waits++
	}

	if priced {
		c.Cost += usd
		if m.Wait {
			c.WaitCost += usd
		}
	} else {
		c.Unpriced++
	}
}

func (c *Cell) sum(o Cell) {
	c.Turns += o.Turns
	c.Sub += o.Sub
	c.Input += o.Input
	c.Write += o.Write
	c.Write5m += o.Write5m
	c.Write1h += o.Write1h
	c.Read += o.Read
	c.Output += o.Output
	c.Cost += o.Cost
	c.Unpriced += o.Unpriced
	c.Waits += o.Waits
	c.WaitCost += o.WaitCost
}

// Tokens is every token of the cell.
func (c Cell) Tokens() int { return c.Input + c.Write + c.Write5m + c.Write1h + c.Read + c.Output }

// Session is one role session's replies to price and give to tasks.
type Session struct {
	Node, Crew string
	// Role is the role or instance (dev#2) the session runs; Group the row it is counted under (dev for dev#2).
	Role, Group string
	// Dispatcher is the crew's dispatcher session: a peer message from it is a hand-off, not a peer's line.
	Dispatcher string
	Messages   []registry.Message
}

// TriggerDispatcher is a message from the crew's own dispatcher session.
const TriggerDispatcher = "dispatcher"

// Cells prices the sessions' replies and gives each to a task.
func Cells(sessions []Session, ledgers map[string]Ledger, prices Table, loc *time.Location) []Cell {
	by := map[Cell]*Cell{}

	for _, s := range sessions {
		l := ledgers[s.Crew]

		for _, m := range s.Messages {
			a := l.Of(s.Role, m.Cwd, m.At)

			trigger := m.Trigger
			if trigger == registry.TriggerPeer && m.From != "" && m.From == s.Dispatcher {
				trigger = TriggerDispatcher
			}

			key := Cell{Node: s.Node, Crew: s.Crew, Role: s.Group, Task: a.Key(), How: a.How, Model: m.Model,
				Day: m.At.In(loc).Format(time.DateOnly), Trigger: trigger}

			c := by[key]
			if c == nil {
				c = &key
				by[key] = c
			}

			usd, ok := prices.Cost(m)
			c.add(m, usd, ok)
		}
	}

	out := make([]Cell, 0, len(by))
	for _, c := range by {
		out = append(out, *c)
	}

	return out
}

// Views are what a usage table groups by.
var Views = []string{"role", "task", "model", "day", "crew", "trigger", "how"}

// Row is a table row: the cells that share the view's key, with their model mix and how their tasks were decided.
type Row struct {
	Key string `json:"key"`
	// Node and Crew are set where the key belongs to one crew (a role, a task).
	Node string `json:"node,omitempty"`
	Crew string `json:"crew,omitempty"`
	Cell
	// Models is dollars by model, Hows turns by how their task was decided.
	Models   map[string]float64 `json:"models"`
	ModelsN  map[string]int     `json:"model_turns"`
	Hows     map[string]int     `json:"hows,omitempty"`
	Unknowns []string           `json:"unpriced_models,omitempty"`
}

// keyOf is a cell's row key in a view; crewScoped views keep one crew's rows apart from another's.
func keyOf(c Cell, view string) (key string, crewScoped bool) {
	switch view {
	case "role":
		return c.Role, true
	case "task":
		return c.Task, c.Task != Untasked
	case "model":
		return c.Model, false
	case "day":
		return c.Day, false
	case "crew":
		return crewName(c.Node, c.Crew), false
	case "trigger":
		return c.Trigger, false
	case "how":
		return c.How, false
	}

	panic(fmt.Sprintf("cost: no view %q", view))
}

func crewName(node, crew string) string {
	if node == "" {
		return crew
	}

	return node + "/" + crew
}

// Group sums the cells into the view's rows, the costliest first.
func Group(cells []Cell, view string) []Row {
	type id struct{ key, node, crew string }

	by := map[id]*Row{}

	for _, c := range cells {
		k, scoped := keyOf(c, view)

		i := id{key: k}
		if scoped {
			i.node, i.crew = c.Node, c.Crew
		}

		r := by[i]
		if r == nil {
			r = &Row{Key: k, Node: i.node, Crew: i.crew, Models: map[string]float64{}, ModelsN: map[string]int{},
				Hows: map[string]int{}}
			by[i] = r
		}

		r.sum(c)
		r.Models[c.Model] += c.Cost
		r.ModelsN[c.Model] += c.Turns
		r.Hows[c.How] += c.Turns

		if c.Unpriced > 0 && !slices.Contains(r.Unknowns, c.Model) {
			r.Unknowns = append(r.Unknowns, c.Model)
		}
	}

	out := make([]Row, 0, len(by))
	for _, r := range by {
		out = append(out, *r)
	}

	sort.Slice(out, func(i, j int) bool {
		if view == "day" {
			return out[i].Key > out[j].Key
		}

		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}

		return out[i].Turns > out[j].Turns
	})

	return out
}

// Total sums every cell into one row.
func Total(cells []Cell) Row {
	r := Row{Key: "all", Models: map[string]float64{}, ModelsN: map[string]int{}, Hows: map[string]int{}}

	for _, c := range cells {
		r.sum(c)
		r.Models[c.Model] += c.Cost
		r.ModelsN[c.Model] += c.Turns
		r.Hows[c.How] += c.Turns

		if c.Unpriced > 0 && !slices.Contains(r.Unknowns, c.Model) {
			r.Unknowns = append(r.Unknowns, c.Model)
		}
	}

	return r
}

// Mix is the row's models by their share of its dollars ("opus-5.5 94% · haiku-5.5 6%"); shares under 1% are
// left out, a model without a price is named as unpriced.
func (r Row) Mix() string {
	type part struct {
		name  string
		share float64
	}

	var (
		parts    []part
		unpriced []string
	)

	for _, m := range slices.Sorted(maps.Keys(r.ModelsN)) {
		if slices.Contains(r.Unknowns, m) {
			unpriced = append(unpriced, ShortModel(m)+" unpriced")

			continue
		}

		share := 0.0
		if r.Cost > 0 {
			share = r.Models[m] / r.Cost
		}

		parts = append(parts, part{ShortModel(m), share})
	}

	sort.SliceStable(parts, func(i, j int) bool { return parts[i].share > parts[j].share })

	var out []string

	for _, p := range parts {
		if p.share >= 0.01 || len(parts) == 1 {
			out = append(out, fmt.Sprintf("%s %.0f%%", p.name, 100*p.share))
		}
	}

	return strings.Join(append(out, unpriced...), " · ")
}

// HowMix is how the row's turns were given to tasks: "cwd 80% · window 20%", or "cwd" when all one way.
func (r Row) HowMix() string {
	if len(r.Hows) == 1 {
		for h := range r.Hows {
			return h
		}
	}

	var out []string

	for _, h := range []string{ByCwd, ByWindow, Shared, Untasked} {
		n := r.Hows[h]

		switch share := 100 * float64(n) / float64(r.Turns); {
		case n == 0:
		case share < 0.5:
			out = append(out, h+" <1%")
		default:
			out = append(out, fmt.Sprintf("%s %.0f%%", h, share))
		}
	}

	return strings.Join(out, " · ")
}

// ShortModel is a model id as a table shows it: claude-opus-5-5 is opus-5.5, a dated snapshot loses its date.
func ShortModel(id string) string {
	s := strings.TrimPrefix(id, "claude-")

	parts := strings.Split(s, "-")
	if n := len(parts); n > 1 && len(parts[n-1]) == 8 && isDigits(parts[n-1]) {
		parts = parts[:n-1]
	}

	if n := len(parts); n >= 3 && isDigits(parts[n-1]) && isDigits(parts[n-2]) {
		parts = append(parts[:n-2], parts[n-2]+"."+parts[n-1])
	}

	return strings.Join(parts, "-")
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
