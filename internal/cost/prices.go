// Package cost prices what the crews' sessions spent at API rates and gives every reply to a task: by the
// directory it ran in, by the task its role held at the time, or to none — never by a guess.
package cost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
)

// Rates are dollars per million tokens. A cache write left at 0 is the API's multiple of the input rate: 1.25×
// for five minutes, 2× for an hour.
type Rates struct {
	Input   float64 `json:"input"`
	Output  float64 `json:"output"`
	Read    float64 `json:"read"`
	Write5m float64 `json:"write_5m,omitempty"`
	Write1h float64 `json:"write_1h,omitempty"`
}

func (r Rates) write5m() float64 { return or(r.Write5m, 1.25*r.Input) }
func (r Rates) write1h() float64 { return or(r.Write1h, 2*r.Input) }

// Price is one model's rates.
type Price struct {
	Rates
	// Above and Over are a second tier: a reply whose prompt is over Above tokens is priced at Over.
	Above int    `json:"above,omitempty"`
	Over  *Rates `json:"over,omitempty"`
	// Fast multiplies the rates of a fast-mode reply; 0 leaves fast mode unpriced.
	Fast float64 `json:"fast,omitempty"`
}

// Table is the prices zmina knows, as of a date.
type Table struct {
	Date string `json:"date"`
	// WebSearch is dollars per web search the server ran.
	WebSearch float64          `json:"web_search,omitempty"`
	Models    map[string]Price `json:"models"`
	// From is where the table came from: built in, or the override file over it.
	From string `json:"-"`
}

// builtinDate is when the built-in prices were checked against the API's public pricing page. A model they lack is unpriced
// until it is added here or in the override file.
const builtinDate = "2026-11-01"

// Builtin is the table compiled into zmina.
func Builtin() Table {
	r := func(in, out, read float64) Price { return Price{Rates: Rates{Input: in, Output: out, Read: read}} }

	opus55 := r(4, 20, 0.20)
	opus55.Fast = 2

	haiku55 := r(0.10, 0.50, 0.01)
	haiku55.Above, haiku55.Over = 100_000, &Rates{Input: 0.50, Output: 2.50, Read: 0.05}

	return Table{
		Date:      builtinDate,
		WebSearch: 0.01,
		From:      "built in",
		Models: map[string]Price{
			"claude-fable-5-1":  r(10, 50, 0.25),
			"claude-mythos-5-1": r(10, 50, 0.25),
			"claude-fable-5":    r(10, 50, 1),
			"claude-mythos-5":   r(10, 50, 1),
			"claude-opus-5-5":   opus55,
			"claude-opus-5":     r(5, 25, 0.50),
			"claude-opus-4-8":   r(5, 25, 0.50),
			"claude-opus-4-7":   r(5, 25, 0.50),
			"claude-opus-4-6":   r(5, 25, 0.50),
			"claude-opus-4-5":   r(5, 25, 0.50),
			"claude-sonnet-5-5": r(2, 10, 0.10),
			"claude-sonnet-5":   r(2, 10, 0.20),
			"claude-sonnet-4-6": r(3, 15, 0.30),
			"claude-sonnet-4-5": r(3, 15, 0.30),
			"claude-haiku-5-5":  haiku55,
			"claude-haiku-4-5":  r(1, 5, 0.10),
		},
	}
}

// Load is the built-in table with the override file over it, model by model; a missing file is no override.
func Load(path string) (Table, error) {
	t := Builtin()

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}

	if err != nil {
		return t, err
	}

	var over Table
	if err := json.Unmarshal(raw, &over); err != nil {
		return t, fmt.Errorf("%s: %w", path, err)
	}

	t.Models = maps.Clone(t.Models)
	maps.Copy(t.Models, over.Models)

	if over.Date != "" {
		t.Date = over.Date
	}

	if over.WebSearch != 0 {
		t.WebSearch = over.WebSearch
	}

	t.From = "built in, " + path + " over it"

	return t, nil
}

// Age is how old the table's prices are, 0 when its date does not parse.
func (t Table) Age(now time.Time) time.Duration {
	d, err := time.Parse(time.DateOnly, t.Date)
	if err != nil {
		return 0
	}

	return now.Sub(d)
}

// Find is the price of a model id: the table's entry of that id, or of the model a dated snapshot is of
// (claude-haiku-4-5-20251001). Nothing else is matched: claude-opus-5-6 is not claude-opus-5.
func (t Table) Find(model string) (Price, bool) {
	if p, ok := t.Models[model]; ok {
		return p, true
	}

	i := strings.LastIndexByte(model, '-')
	if i < 0 || len(model)-i-1 != 8 || strings.Trim(model[i+1:], "0123456789") != "" {
		return Price{}, false
	}

	p, ok := t.Models[model[:i]]

	return p, ok
}

// Cost is a reply's price in dollars; ok is false when the table has no price for its model or its speed.
func (t Table) Cost(m registry.Message) (float64, bool) {
	p, ok := t.Find(m.Model)
	if !ok {
		return 0, false
	}

	r := p.Rates
	if p.Over != nil && m.Prompt() > p.Above {
		r = *p.Over
	}

	mult := 1.0

	if m.Speed != "" {
		if m.Speed != "fast" || p.Fast == 0 {
			return 0, false
		}

		mult = p.Fast
	}

	usd := (float64(m.Input)*r.Input + float64(m.Write5m)*r.write5m() + float64(m.Write1h)*r.write1h() +
		float64(m.Read)*r.Read + float64(m.Output)*r.Output) / 1e6

	return usd*mult + float64(m.WebSearch)*t.WebSearch, true
}

func or(v, def float64) float64 {
	if v != 0 {
		return v
	}

	return def
}
