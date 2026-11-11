package route

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// jsonDecision is the answer as zmina route --json prints it.
type jsonDecision struct {
	To           string        `json:"to"`
	Action       Action        `json:"action"`
	Rule         *int          `json:"rule"`
	Name         string        `json:"name"`
	Why          string        `json:"why"`
	Alternatives []Alternative `json:"alternatives"`
	Limits       LimitsView    `json:"limits"`
	Stale        bool          `json:"stale"`
	Explain      []Step        `json:"explain,omitempty"`
}

// JSON is the answer as a value to print as JSON; with explain it carries every rule asked. A none answer has a null
// rule (rule 0 is a rule, so zero cannot stand for none).
func (d Decision) JSON(explain bool) any {
	out := jsonDecision{To: d.To, Action: d.Action, Name: d.Name, Why: d.Why, Alternatives: d.Alternatives,
		Limits: d.Limits, Stale: d.Stale}

	if out.Alternatives == nil {
		out.Alternatives = []Alternative{}
	}

	if d.Matched() {
		rule := d.Rule
		out.Rule = &rule
	}

	if explain {
		out.Explain = d.Trace
	}

	return out
}

// WriteExplain prints every rule asked, a row each: its number, name, yes or no, and why.
func (d Decision) WriteExplain(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	for _, s := range d.Trace {
		answer := "no"
		if s.Matched {
			answer = "yes"
		}

		fmt.Fprintf(tw, "%d %s\t%s\t%s\n", s.Rule, s.Name, answer, s.Why)
	}

	return tw.Flush()
}
