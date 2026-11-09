package cmd

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
)

// usageLesson is one lessons triage run and what it cost, from the crew's event log (lessons-cost).
type usageLesson struct {
	Node string    `json:"node,omitempty"`
	Crew string    `json:"crew"`
	At   time.Time `json:"at"`
	Cost float64   `json:"cost"`
}

func usageLessons(crews []uiCrew, node string, since time.Time) []usageLesson {
	var out []usageLesson

	for _, uc := range crews {
		c, err := crew.Load(uc.Path)
		if err != nil {
			continue
		}

		evs, _ := eventLog(c).Read(events.Filter{Since: since})
		for _, e := range evs {
			if e.Event != "lessons-cost" {
				continue
			}

			v, err := strconv.ParseFloat(strings.TrimPrefix(strings.Fields(e.Detail + " ")[0], "$"), 64)
			if err != nil {
				continue
			}

			out = append(out, usageLesson{Node: node, Crew: uc.Key, At: e.TS, Cost: v})
		}
	}

	return out
}

// printLessons says what the lessons triage cost in the window, by crew.
func printLessons(out io.Writer, runs []usageLesson) {
	if len(runs) == 0 {
		return
	}

	by := map[string]float64{}
	n := map[string]int{}

	var (
		keys  []string
		total float64
	)

	for _, r := range runs {
		k := crewLabel(r.Node, r.Crew)
		if _, ok := by[k]; !ok {
			keys = append(keys, k)
		}

		by[k] += r.Cost
		n[k]++
		total += r.Cost
	}

	slices.Sort(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s $%.2f (%s)", k, by[k], times(n[k])))
	}

	fmt.Fprintf(out, "\nLessons triage $%.2f: %s.\n", total, strings.Join(parts, ", "))
}
