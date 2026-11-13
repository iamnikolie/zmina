package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
)

// explainOut is what zmina explain --json prints: the explanation (shift.Explained, flat) with the words ls shows for the
// role, so that the two are read side by side.
type explainOut struct {
	// Status is the role's STATUS column in zmina ls --wide, Shows its state and status as the compact zmina ls writes them
	// (`up idle (dialog)`): the same functions, over the same look.
	Status string `json:"status"`
	Shows  string `json:"shows"`
	shift.Explained
	// LastEvent is the newest event of the log about a role that is not up.
	LastEvent *explainEvent `json:"last_event,omitempty"`
}

// explainEvent is one line of the event log.
type explainEvent struct {
	At     time.Time `json:"at"`
	Event  string    `json:"event"`
	Detail string    `json:"detail,omitempty"`
}

// lastEventKinds are the events that say how a role came to be not up.
var lastEventKinds = map[string]bool{"up": true, "down": true, "ended": true, "release": true, "sleep": true, "broken": true,
	"restart": true, "restart-pending": true, "restart-failed": true, "crash-loop": true}

func (a *app) explainCommand() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:     "explain <role>",
		Aliases: []string{"why"},
		Short:   "Say why a role's status is what it is: the source that decided (hook record, session file, screen rule) with its evidence, the sources that lost, and what holds its queue",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				v, why, ok := s.Look(args[0])
				if !ok {
					return fmt.Errorf("no role %q in the crew", args[0])
				}

				out := explainOut{Status: statusCell(v), Shows: roleCell(v), Explained: s.Explain(v, why)}

				if v.State != shift.PresenceUp {
					out.LastEvent = lastEvent(s, v.Role)
				}

				if asJSON {
					return a.printJSON(out)
				}

				writeExplain(a.out, s, v, out)

				return nil
			})
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// lastEvent is the newest event of the log about a role that says how it came to be down.
func lastEvent(s *shift.Shift, role string) *explainEvent {
	evs, err := eventLog(s.Crew).Read(events.Filter{Role: role, Since: s.Now().Add(-30 * 24 * time.Hour)})
	if err != nil {
		return nil
	}

	for i := len(evs) - 1; i >= 0; i-- {
		if lastEventKinds[evs[i].Event] {
			return &explainEvent{At: evs[i].TS, Event: evs[i].Event, Detail: shift.Quote(evs[i].Detail)}
		}
	}

	return nil
}

// when is a moment in the crew's own time, with how long before it was: "12:03:10 (4m ago)".
func when(s *shift.Shift, at time.Time, ageSeconds int) string {
	if at.IsZero() {
		return ""
	}

	text := at.In(s.Crew.Location()).Format("15:04:05")
	if ageSeconds > 0 {
		text += " (" + age(time.Duration(ageSeconds)*time.Second) + " ago)"
	}

	return text
}

// writeExplain prints the explanation in plain words: the status, who decided it and on what evidence, who lost and what
// each said, what the screen says besides, and what bears on the queue.
func writeExplain(w io.Writer, s *shift.Shift, v shift.View, out explainOut) {
	role := v.Role
	if role == s.Crew.Dispatcher {
		role += " *"
	}

	if v.State != shift.PresenceUp {
		fmt.Fprintf(w, "%s: %s", role, out.Summary)

		if k := out.LastKnown; k != nil {
			fmt.Fprintf(w, "; last known: %s", k.Words)
		}

		if e := out.LastEvent; e != nil {
			fmt.Fprintf(w, "; last event: %s %s", e.Event, e.At.In(s.Crew.Location()).Format("01-02 15:04"))

			if e.Detail != "" {
				fmt.Fprintf(w, " (%s)", e.Detail)
			}
		}

		fmt.Fprintln(w)
		writeQueue(w, out)

		return
	}

	fmt.Fprintf(w, "%s: %s\n", role, out.Shows)
	fmt.Fprintf(w, "status shown: %s\n", out.Status)
	writeSaid(w, s, "decided by", out.Decided)

	if len(out.Lost) > 0 {
		fmt.Fprintln(w, "consulted and not decisive, in the order they are trusted (hook record, session file, screen):")

		for _, l := range out.Lost {
			writeSaid(w, s, "  -", l)
		}
	}

	writeScreen(w, s, out)
	writeQueue(w, out)
	fmt.Fprintf(w, "context: %s\n", out.Context.Words)

	if out.Restart.Words != "" {
		fmt.Fprintf(w, "restart: %s\n", out.Restart.Words)
	}
}

// sourceWords names a source as the owner knows it.
func sourceWords(src shift.Source) string {
	switch src {
	case shift.SourceHook:
		return "hook record"
	case shift.SourceRegistry:
		return "session file"
	case shift.SourceScreen:
		return "screen rule"
	case shift.SourceTmux:
		return "tmux"
	default:
		return "no source"
	}
}

// writeSaid prints what one source said: its kind, its words, its evidence and age, and why it did not decide.
func writeSaid(w io.Writer, s *shift.Shift, lead string, d shift.Said) {
	line := fmt.Sprintf("%s %s %s: %s", lead, sourceWords(d.Source), d.Kind, d.Says)

	if d.Source == shift.SourceNone {
		line = fmt.Sprintf("%s %s", lead, d.Says)
	}

	if d.Evidence != "" {
		line += " [" + d.Evidence + "]"
	}

	if at := when(s, d.At, d.AgeSeconds); at != "" {
		line += " at " + at
	}

	if d.Why != "" {
		line += " — not decisive: " + d.Why
	}

	fmt.Fprintln(w, line)
}

// writeScreen prints what the screen says apart from the status.
func writeScreen(w io.Writer, s *shift.Shift, out explainOut) {
	f := out.Screen

	if f.Error != "" {
		fmt.Fprintf(w, "screen: not read: %s\n", f.Error)

		return
	}

	if f.Draft != "" {
		fmt.Fprintf(w, "screen: input line %s\n", f.Draft)
	}

	if l := f.Limit; l != nil {
		fmt.Fprintf(w, "screen: rule %s matched %q: %s\n", l.Kind, l.Evidence, l.Says)
	}

	if d := f.Dialog; d != nil {
		fmt.Fprintf(w, "screen: a dialog is shown, its words from the %s: %q (%d choices)", sourceWords(d.From), d.Question, d.Options)

		if d.Why != "" {
			fmt.Fprintf(w, "; %s", d.Why)
		}

		fmt.Fprintln(w)
	}

	if n := f.Now; n != nil {
		fmt.Fprintf(w, "screen: last said %q", n.Evidence)

		if at := when(s, n.At, 0); at != "" {
			fmt.Fprintf(w, " since %s", at)
		}

		fmt.Fprintln(w)
	}
}

// writeQueue prints what bears on the lines queued for the role.
func writeQueue(w io.Writer, out explainOut) {
	q := out.Queue

	var parts []string

	if h := q.Held; h != nil {
		parts = append(parts, fmt.Sprintf("held for a failed turn (%s): %s", clip(h.Text, 80), h.Why))
	}

	parts = append(parts, fmt.Sprintf("%d %s waiting", q.Lines, plural(q.Lines, "line", "lines")))

	if q.Attention > 0 || q.Failed > 0 {
		parts = append(parts, strings.Join(failedMarks(shift.View{Failed: q.Failed, Attention: q.Attention}), ", "))
	}

	if f := q.InFlight; f != nil {
		parts = append(parts, fmt.Sprintf("typed and not landed: a %s of %d characters", f.Kind, f.Length))
	}

	parts = append(parts, q.Waits...)

	fmt.Fprintf(w, "queue: %s\n", strings.Join(parts, "; "))
}
