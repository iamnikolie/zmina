package cost

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

func TestCost(t *testing.T) {
	p := Builtin()

	// A million of each at opus-5.5: 4 in, 5 for 5m writes, 8 for 1h writes, 0.20 read, 20 out, and two searches.
	m := registry.Message{Model: "claude-opus-5-5", Input: 1e6, Write5m: 1e6, Write1h: 1e6, Read: 1e6, Output: 1e6,
		WebSearch: 2}
	usd, ok := p.Cost(m)
	require.True(t, ok)
	assert.InDelta(t, 4+5+8+0.20+20+0.02, usd, 1e-9)

	parts, _ := p.Parts(m)
	assert.Equal(t, Parts{Input: 4.02, Write: 13, Read: 0.20, Output: 20}, roundParts(parts), "by part, searches with input")

	m.Speed = "fast"
	usd, _ = p.Cost(m)
	assert.InDelta(t, 2*(4+5+8+0.20+20)+0.02, usd, 1e-9, "fast mode doubles the token rates")

	_, ok = p.Cost(registry.Message{Model: "claude-sonnet-5-5", Speed: "fast", Output: 1})
	assert.False(t, ok, "fast mode without a price is unpriced, not guessed")

	usd, ok = p.Cost(registry.Message{Model: "claude-haiku-4-5-20251001", Output: 1e6})
	require.True(t, ok, "a dated snapshot is priced as its model")
	assert.InDelta(t, 5, usd, 1e-9)

	small, _ := p.Cost(registry.Message{Model: "claude-haiku-5-5", Input: 100_000})
	big, _ := p.Cost(registry.Message{Model: "claude-haiku-5-5", Input: 100_001})
	assert.InDelta(t, 0.01, small, 1e-9)
	assert.InDelta(t, 0.0500005, big, 1e-9, "a prompt over 100k is the second tier")

	_, ok = p.Cost(registry.Message{Model: "claude-opus-9", Output: 1})
	assert.False(t, ok, "an unknown model is unpriced")

	for _, id := range []string{"claude-opus-5-6", "claude-opus-5-55", "claude-opus-5-5-fast"} {
		_, ok = p.Find(id)
		assert.False(t, ok, "%s is not a model the table has", id)
	}
}

func roundParts(p Parts) Parts {
	r := func(v float64) float64 { return math.Round(v*1e6) / 1e6 }

	return Parts{Input: r(p.Input), Write: r(p.Write), Read: r(p.Read), Output: r(p.Output)}
}

func TestBands(t *testing.T) {
	assert.Equal(t, "0-100k", BandOf(0))
	assert.Equal(t, "100-200k", BandOf(100_000))
	assert.Equal(t, "350-500k", BandOf(499_999))
	assert.Equal(t, "700k+", BandOf(2_000_000))

	at := func(read int) registry.Message {
		return registry.Message{At: h(1), Model: "claude-opus-5-5", Read: read, Write5m: 1000, Output: 1000}
	}
	cells := Cells([]Session{{Crew: "acme", Role: "dev", Group: "dev", Messages: []registry.Message{
		at(400_000), at(50_000), at(450_000), at(150_000)}}}, nil, Builtin(), time.UTC)
	cells = append(cells, Cell{Crew: "old", Role: "dev", Turns: 1, Cost: 1}) // a peer older than bands

	rows := Group(cells, "ctx")
	keys := []string{}
	for _, r := range rows {
		keys = append(keys, r.Key)
	}

	assert.Equal(t, []string{"0-100k", "100-200k", "350-500k", "?"}, keys, "bands in order, not by cost")
	assert.Equal(t, 2, rows[2].Turns)
	assert.InDelta(t, 0.17, rows[2].ReadCost, 1e-9, "850k cache reads at $0.20 a million")
	assert.InDelta(t, rows[2].Cost, rows[2].ReadCost+rows[2].WriteCost+rows[2].OutputCost, 1e-9, "no uncached input")

	m := func(at time.Time, compacted bool) registry.Message {
		return registry.Message{At: at, Model: "claude-opus-5-5", Output: 1, Compacted: compacted}
	}
	cells = Cells([]Session{
		{Crew: "acme", Role: "dev", Group: "dev", Messages: []registry.Message{m(h(1), false),
			m(h(1).Add(time.Minute), true), m(h(1).Add(10*time.Minute), false)}},
		{Crew: "acme", Role: "dev", Group: "dev", Messages: []registry.Message{m(h(1).Add(2*time.Minute), false)}},
		{Crew: "acme", Role: "dev#2", Group: "dev", Messages: []registry.Message{m(h(1), false)}},
	}, nil, Builtin(), time.UTC)
	total := Total(cells)
	assert.Equal(t, 1, total.Compacts)
	assert.Equal(t, 3, total.Active, "two stretches of dev (its sessions together), one of its copy")
	assert.Len(t, Stretches([]Session{{Messages: []registry.Message{m(h(1), false), m(h(1).Add(time.Minute), false),
		m(h(2), false)}}}), 2)
}

// TestBuiltinRates pins the built-in table to the public pricing page as of builtinDate: input, 5m and 1h cache
// writes, cache reads, output, dollars per million tokens.
func TestBuiltinRates(t *testing.T) {
	p := Builtin()

	for model, want := range map[string][5]float64{
		"claude-fable-5-1":  {10, 12.50, 20, 0.25, 50},
		"claude-opus-5-5":   {4, 5, 8, 0.20, 20},
		"claude-opus-5":     {5, 6.25, 10, 0.50, 25},
		"claude-sonnet-5-5": {2, 2.50, 4, 0.10, 10},
		"claude-sonnet-5":   {2, 2.50, 4, 0.20, 10},
		"claude-sonnet-4-6": {3, 3.75, 6, 0.30, 15},
		"claude-haiku-5-5":  {0.10, 0.125, 0.20, 0.01, 0.50},
		"claude-haiku-4-5":  {1, 1.25, 2, 0.10, 5},
	} {
		r, ok := p.Find(model)
		require.True(t, ok, model)
		assert.InDeltaSlice(t, want[:], []float64{r.Input, r.write5m(), r.write1h(), r.Read, r.Output}, 1e-9, model)
	}

	over := p.Models["claude-haiku-5-5"].Over
	require.NotNil(t, over)
	assert.InDeltaSlice(t, []float64{0.50, 0.625, 1, 0.05, 2.50},
		[]float64{over.Input, over.write5m(), over.write1h(), over.Read, over.Output}, 1e-9)
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")

	p, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "built in", p.From)

	require.NoError(t, os.WriteFile(path, []byte(`{"date":"2026-12-01","models":{"claude-opus-9":{"input":1,"output":2,`+
		`"read":0.1},"claude-opus-5-5":{"input":3,"output":15,"read":0.3}}}`), 0o600))

	p, err = Load(path)
	require.NoError(t, err)
	assert.Equal(t, "2026-12-01", p.Date)
	assert.InDelta(t, 3, p.Models["claude-opus-5-5"].Input, 0, "the file replaces a model")
	assert.InDelta(t, 2, p.Models["claude-sonnet-5-5"].Input, 0, "and keeps the rest")
	assert.InDelta(t, 0.01, p.WebSearch, 0)

	usd, ok := p.Cost(registry.Message{Model: "claude-opus-9", Write1h: 1e6})
	require.True(t, ok)
	assert.InDelta(t, 2, usd, 1e-9, "a write left out is the input rate's multiple")

	assert.Equal(t, 31*24*time.Hour, Builtin().Age(time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)))
	assert.InDelta(t, 4, Builtin().Models["claude-opus-5-5"].Input, 0, "the built-in table is left as it is")

	require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
	_, err = Load(path)
	assert.Error(t, err)
}

var day = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func h(n int) time.Time { return day.Add(time.Duration(n) * time.Hour) }

func TestLedger(t *testing.T) {
	tasks := []task.Task{
		{ID: "T-1", Role: "dev2", State: task.Done, Worktree: "/w/acme-T-1", Created: h(1)},
		{ID: "T-2", Role: "dev", State: task.Open, Created: h(2)},
		{ID: "T-3", Role: "dev", State: task.Waiting, Created: h(3), Worktree: "/w/acme-T-1/sub"},
		// No history left: its file says when.
		{ID: "T-4", Role: "ops", State: task.Done, Created: h(1), Updated: h(5)},
	}
	evs := []events.Event{
		{TS: h(1), Role: "dev", Event: "task", Task: "T-1", Detail: "new: login"},
		{TS: h(2), Role: "dev", Event: "task", Task: "T-1", Detail: "waiting-alex, note: ask, role x"},
		{TS: h(3), Role: "dev2", Event: "task", Task: "T-1", Detail: "role dev2"},
		{TS: h(6), Role: "dev2", Event: "task", Task: "T-1", Detail: "done"},
		{TS: h(7), Role: "dev2", Event: "task", Task: "T-1", Detail: "note: after"},
		{TS: h(2), Role: "dev", Event: "task", Task: "T-2", Detail: "new: docs"},
		{TS: h(4), Role: "dev", Event: "task", Task: "T-2", Detail: "parked L-3"},
		{TS: h(8), Role: "dev", Event: "task", Task: "T-2", Detail: "open"},
		{TS: h(3), Role: "dev", Event: "task", Task: "T-3", Detail: "new: api"},
		{TS: h(2), Role: "dev", Event: "up"},
	}

	l := NewLedger(tasks, evs)
	assert.Equal(t, []Hold{
		{Task: "T-1", Role: "dev", From: h(1), To: h(3)},
		{Task: "T-1", Role: "dev2", From: h(3), To: h(6)},
		{Task: "T-2", Role: "dev", From: h(2), To: h(4)},
		{Task: "T-2", Role: "dev", From: h(8)},
		{Task: "T-3", Role: "dev", From: h(3)},
		{Task: "T-4", Role: "ops", From: h(1), To: h(5)},
	}, l.Holds)

	for _, c := range []struct {
		role, cwd string
		at        time.Time
		want      Attribution
	}{
		{"dev", "/w/acme-T-1/sub/x", h(9), Attribution{Tasks: []string{"T-3"}, How: ByCwd}},
		{"ops", "/w/acme-T-1", h(9), Attribution{Tasks: []string{"T-1"}, How: ByCwd}},
		{"ops", "/w/acme-T-10", h(9), Attribution{How: Untasked}},
		{"dev", "/work", h(2), Attribution{Tasks: []string{"T-1", "T-2"}, How: Shared}},
		{"dev", "/work", h(3), Attribution{Tasks: []string{"T-2", "T-3"}, How: Shared}},
		{"dev", "/work", h(5), Attribution{Tasks: []string{"T-3"}, How: ByWindow}},
		{"dev2", "", h(6), Attribution{How: Untasked}},
		{"dev2", "", h(5), Attribution{Tasks: []string{"T-1"}, How: ByWindow}},
	} {
		assert.Equal(t, c.want, l.Of(c.role, c.cwd, c.at), "%s %s %s", c.role, c.cwd, c.at)
	}

	assert.Equal(t, "shared T-2+T-10", Attribution{Tasks: []string{"T-2", "T-10"}, How: Shared}.Key())
	assert.Equal(t, Untasked, Attribution{How: Untasked}.Key())
}

func TestCells(t *testing.T) {
	l := NewLedger([]task.Task{{ID: "T-1", Role: "dev", State: task.Open, Created: h(0), Worktree: "/w/T-1"}}, nil)
	msg := func(at time.Time, model, cwd, trigger, from string, sub bool) registry.Message {
		return registry.Message{At: at, Model: model, Output: 1e6, Cwd: cwd, Trigger: trigger, From: from, Sub: sub}
	}

	cells := Cells([]Session{
		{Crew: "acme", Role: "dev#2", Group: "dev", Dispatcher: "zm-dispatcher", Messages: []registry.Message{
			msg(h(1), "claude-opus-5-5", "/w/T-1", registry.TriggerPeer, "zm-dispatcher", false),
			msg(h(1), "claude-opus-5-5", "/w/T-1", registry.TriggerPeer, "zm-dispatcher", false),
			msg(h(2), "claude-haiku-5-5", "/w/T-1", registry.TriggerPeer, "zm-dispatcher", true),
			msg(h(3), "claude-opus-9", "/work", registry.TriggerPeer, "zm-other", false),
		}},
		{Crew: "acme", Role: "dev", Group: "dev", Messages: []registry.Message{
			msg(h(25), "claude-opus-5-5", "/work", registry.TriggerOwner, "", false),
		}},
	}, map[string]Ledger{"acme": l}, Builtin(), time.UTC)

	rows := Group(cells, "task")
	require.Len(t, rows, 2)
	assert.Equal(t, "T-1", rows[0].Key)
	assert.Equal(t, 4, rows[0].Turns, "a copy's turns in the worktree, and its role's while it holds the task")
	assert.Equal(t, 1, rows[0].Sub)
	assert.InDelta(t, 60.5, rows[0].Cost, 1e-9)
	assert.Equal(t, "opus-5.5 99%", rows[0].Mix(), "under 1% is left out")
	assert.Equal(t, "cwd 75% · window 25%", rows[0].HowMix())
	assert.Equal(t, Untasked, rows[1].Key, "the copy holds no task: outside the worktree its turn is no task's")
	assert.Equal(t, 1, rows[1].Unpriced)
	assert.Equal(t, "opus-9 unpriced", rows[1].Mix())
	assert.Equal(t, []string{"claude-opus-9"}, rows[1].Unknowns)

	byTrigger := Group(cells, "trigger")
	assert.Equal(t, "dispatcher", byTrigger[0].Key, "a message from the crew's dispatcher is a hand-off")
	assert.Equal(t, 3, byTrigger[0].Turns)

	days := Group(cells, "day")
	assert.Equal(t, []string{"2026-10-27", "2026-10-26"}, []string{days[0].Key, days[1].Key}, "the newest day first")

	total := Total(cells)
	assert.Equal(t, 5, total.Turns)
	assert.InDelta(t, 60.5, total.Cost, 1e-9)
	assert.Equal(t, "dev", Group(cells, "role")[0].Key)
	assert.Equal(t, "acme", Group(cells, "crew")[0].Key)
}

func TestShortModel(t *testing.T) {
	for id, want := range map[string]string{
		"claude-opus-5-5":           "opus-5.5",
		"claude-opus-5":             "opus-5",
		"claude-haiku-4-5-20251001": "haiku-4.5",
		"claude-fable-5-1":          "fable-5.1",
		"<synthetic>":               "<synthetic>",
	} {
		assert.Equal(t, want, ShortModel(id))
	}
}

func TestCodexRates(t *testing.T) {
	p := Builtin()
	for model, rate := range map[string][3]float64{"gpt-6.1-sol": {2, 10, 0.10}, "gpt-6.1-terra": {1, 5, 0.05}, "gpt-6-luna": {0.10, 0.50, 0.01}, "gpt-6-astra": {10, 50, 1}} {
		m := registry.Message{Model: model, Input: 100_000, Read: 100_000, Output: 100_000}
		usd, ok := p.Cost(m)
		require.True(t, ok)
		assert.InDelta(t, (rate[0]+rate[1]+rate[2])/10, usd, 1e-9)
		m.Input = 200_000
		usd, ok = p.Cost(m)
		require.True(t, ok)
		assert.InDelta(t, (4*rate[0]+2*rate[2]+1.5*rate[1])/10, usd, 1e-9)
		m.Speed = "fast"
		fast, ok := p.Cost(m)
		require.True(t, ok)
		assert.InDelta(t, 2*usd, fast, 1e-9)
	}
	m := registry.Message{Model: "gpt-6.1-sol", Write: 10}
	_, ok := p.Cost(m)
	assert.False(t, ok, "unknown cache lifetime cannot silently use an invented rate")
	explicit := float64(5)
	price := p.Models[m.Model]
	price.Write = &explicit
	p.Models[m.Model] = price
	usd, ok := p.Cost(m)
	require.True(t, ok)
	assert.InDelta(t, .00005, usd, 1e-9)
	m.UnpricedReason = "incomplete snapshot"
	_, ok = p.Cost(m)
	assert.False(t, ok)
}
