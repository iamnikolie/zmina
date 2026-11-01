package cmd

import (
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/cost"
)

// uiCostEvery is how long the page's cost figures are kept before they are read again: reading a week of
// transcripts takes a second or two, the page asks every minute.
const uiCostEvery = 2 * time.Minute

// uiCostWeek is how far back the page's cost figures go.
const uiCostWeek = 7 * 24 * time.Hour

// uiCost is what the page shows of the cost: API-equivalent dollars today and over the week, for all crews, each
// crew, each role today (crew/role) and each task over the week (crew/T-n).
type uiCost struct {
	At     time.Time          `json:"at"`
	Prices string             `json:"prices"`
	Today  float64            `json:"today"`
	Week   float64            `json:"week"`
	Crews  map[string]uiSpend `json:"crews"`
	Roles  map[string]float64 `json:"roles"`
	Tasks  map[string]float64 `json:"tasks"`
	// Ready is false until the first read is done.
	Ready bool `json:"ready"`
}

type uiSpend struct {
	Today float64 `json:"today"`
	Week  float64 `json:"week"`
}

// uiCostCache reads the cost in the background and hands out the last figures, so a request never waits on the
// transcripts.
type uiCostCache struct {
	mu      sync.Mutex
	last    uiCost
	reading bool
	read    func() uiCost
}

func (c *uiCostCache) get() uiCost {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.reading && time.Since(c.last.At) >= uiCostEvery {
		c.reading = true

		go func() {
			v := c.read()

			c.mu.Lock()
			c.last, c.reading = v, false
			c.mu.Unlock()
		}()
	}

	return c.last
}

// readUICost reads every crew's week and sums it the page's way.
func (a *app) readUICost() uiCost {
	now := time.Now()
	out := uiCost{At: now, Ready: true, Crews: map[string]uiSpend{}, Roles: map[string]float64{},
		Tasks: map[string]float64{}}

	prices, err := cost.Load(pricesPath())
	if err != nil {
		return out
	}

	out.Prices = prices.Date

	cells, _ := usageCells(uiCrews(a.extraCrew()), "", now.Add(-uiCostWeek), prices)
	today := now.Format(time.DateOnly)

	for _, c := range cells {
		s := out.Crews[c.Crew]
		s.Week += c.Cost
		out.Week += c.Cost

		if c.Day == today {
			s.Today += c.Cost
			out.Today += c.Cost
			out.Roles[c.Crew+"/"+c.Role] += c.Cost
		}

		out.Crews[c.Crew] = s

		if c.How == cost.ByCwd || c.How == cost.ByWindow {
			out.Tasks[c.Crew+"/"+c.Task] += c.Cost
		}
	}

	return out
}
