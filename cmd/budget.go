package cmd

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/iamnikolie/zmina/internal/cost"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// budgetEvery is how often the ticker checks the budgets: reading the transcripts takes a while.
const budgetEvery = 10 * time.Minute

// budgetedMax is how many crossed budgets the state remembers.
const budgetedMax = 200

// over is a crossed budget: its key in the state and the note the dispatcher hears.
type over struct{ key, note string }

// budgetTick tells the dispatcher of each budget crossed since the last check: a role's (its copies with it) over
// the day, a task's over its life. It reads nothing while no budget is set, and runs outside the beat's lock.
func (a *app) budgetTick(now time.Time) {
	var (
		roles    map[string]float64
		tasks    []task.Task
		due      bool
		crewFile string
	)

	_ = a.readState(func(s *shift.Shift) error {
		roles = roleBudgets(s.Crew)
		all, _ := taskStore(s.Crew).All()

		for _, t := range all {
			if t.Budget > 0 && t.State.Active() {
				tasks = append(tasks, t)
			}
		}

		due = now.Sub(s.State.BudgetAt) >= budgetEvery
		crewFile = s.Crew.Path

		return nil
	})

	if !due || len(roles)+len(tasks) == 0 {
		return
	}

	prices, err := cost.Load(pricesPath())
	if err != nil {
		return
	}

	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	since := day
	for _, t := range tasks {
		if t.Created.Before(since) {
			since = t.Created
		}
	}

	cells, _ := usageCells([]uiCrew{{Key: a.crewKey(), Path: crewFile}}, "", since, prices)
	found := crossed(cells, roles, tasks, day.Format(time.DateOnly))

	_ = a.withState(func(s *shift.Shift) error {
		s.State.BudgetAt = now

		var notes []string

		for _, o := range found {
			if !slices.Contains(s.State.Budgeted, o.key) {
				s.State.Budgeted = append(s.State.Budgeted, o.key)
				notes = append(notes, o.note)
				s.Log("", "budget", "", o.note)
			}
		}

		if n := len(s.State.Budgeted); n > budgetedMax {
			s.State.Budgeted = s.State.Budgeted[n-budgetedMax:]
		}

		s.AddNotes(notes)

		return nil
	})
}

// roleBudgets are the crew's roles with a budget.
func roleBudgets(c *crew.Crew) map[string]float64 {
	out := map[string]float64{}

	for _, name := range c.Names() {
		if b := c.Roles[name].Budget; b > 0 {
			out[name] = b
		}
	}

	return out
}

// crossed is the budgets the cells are over: a role's by what it spent on the day, a task's by its own turns
// (not the ones it shares).
func crossed(cells []cost.Cell, roles map[string]float64, tasks []task.Task, day string) []over {
	spent := map[string]float64{}
	ofTask := map[string]float64{}

	for _, c := range cells {
		if c.Day == day {
			spent[c.Role] += c.Cost
		}

		ofTask[c.Task] += c.Cost
	}

	var out []over

	for _, name := range slices.Sorted(maps.Keys(roles)) {
		if b := roles[name]; spent[name] > b {
			out = append(out, over{key: "role:" + name + ":" + day, note: fmt.Sprintf(
				"budget: %s spent %s today (API-equivalent), over its %s a day", name, dollars(spent[name]), dollars(b))})
		}
	}

	for _, t := range tasks {
		if ofTask[t.ID] > t.Budget {
			out = append(out, over{key: fmt.Sprintf("task:%s:%g", t.ID, t.Budget), note: fmt.Sprintf(
				"budget: %s (%s) cost %s so far (API-equivalent), over its %s", t.ID, t.Role, dollars(ofTask[t.ID]),
				dollars(t.Budget))})
		}
	}

	return out
}
