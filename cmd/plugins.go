package cmd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/plugin"
	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) pluginCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "plugin",
		Short: "Plugins: programs told of what reaches the owner (a question on his todo or a role's screen), able to answer back",
	}

	c.AddCommand(a.pluginLsCommand(), a.pluginTestCommand(), a.pluginDeliverCommand())

	return c
}

// pluginLast is how a plugin's newest run ended, for ls.
type pluginLast struct {
	At    time.Time `json:"at"`
	Event string    `json:"event"`
	ID    string    `json:"id"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
}

// pluginRow is one plugin in ls.
type pluginRow struct {
	Name string `json:"name"`
	// Program is where its program is, empty when it is not found (Problem says why, and where it is looked for).
	Program         string      `json:"program"`
	Found           bool        `json:"found"`
	Via             string      `json:"via,omitempty"`
	Problem         string      `json:"problem,omitempty"`
	Events          []string    `json:"events"`
	QuietWhenActive string      `json:"quiet_when_active"`
	Timeout         string      `json:"timeout"`
	DialogAfter     string      `json:"dialog_after"`
	Last            *pluginLast `json:"last,omitempty"`
	// Open is what it was told of that is still open, Waiting the events waiting for another try. Partial is what the last
	// look could not read, empty when it read all.
	Open    int    `json:"open"`
	Waiting int    `json:"waiting"`
	Partial string `json:"partial,omitempty"`
}

// pluginRows reads every plugin of the crew: where its program is, and what its told file says of its last run.
func pluginRows(c *crew.Crew) ([]pluginRow, []string) {
	var (
		rows  []pluginRow
		warns []string
	)

	for _, pl := range c.Plugins {
		row := pluginRow{Name: pl.Name, Events: pl.Events(), QuietWhenActive: spanOff(pl.QuietD()), Timeout: pl.TimeoutD().String(),
			DialogAfter: pl.DialogAfterD().String()}

		if prog, err := plugin.Resolve(pl, c.Layout().Dir); err != nil {
			row.Problem = err.Error()
		} else {
			row.Program, row.Found, row.Via = prog.Path, true, prog.Via
		}

		st, err := plugin.Load(c.Layout().PluginTold(pl.Name))
		if err != nil {
			warns = append(warns, err.Error())
		}

		// What the told file says is data once it is read: quoted as any quotation, however it came to say it.
		if l := st.Last; l != nil {
			row.Last = &pluginLast{At: l.At, Event: shift.QuoteN(l.Event, 40), ID: shift.QuoteN(l.ID, 80), OK: l.OK, Error: shift.QuoteN(l.Err, 200)}
		}

		row.Open, row.Waiting, row.Partial = st.Open(), len(st.Queue), shift.QuoteN(st.Partial, 200)
		rows = append(rows, row)
	}

	return rows, warns
}

func spanOff(d time.Duration) string {
	if d <= 0 {
		return "off"
	}

	return d.String()
}

func (a *app) pluginLsCommand() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "The crew's plugins: each one's program (or not found), the events it is told of, and how its last run ended",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.loadCrew()
			if err != nil {
				return err
			}

			rows, warns := pluginRows(c)
			for _, w := range warns {
				fmt.Fprintln(a.err, "zmina: warning:", w)
			}

			if asJSON {
				if rows == nil {
					rows = []pluginRow{}
				}

				return a.printJSON(rows)
			}

			if len(rows) == 0 {
				fmt.Fprintln(a.out, "no plugins in the crew file")

				return nil
			}

			w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)

			for _, r := range rows {
				program := r.Program
				if !r.Found {
					program = "not found"
				} else if r.Via == "PATH" {
					program += " (on PATH)"
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, program, strings.Join(r.Events, ","), lastWords(c, r))
			}

			if err := w.Flush(); err != nil {
				return err
			}

			for _, r := range rows {
				if !r.Found {
					fmt.Fprintf(a.out, "%s: %s\n", r.Name, r.Problem)
				}
			}

			return nil
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// lastWords is a plugin's last run in a few words: what it was about, how it ended, when; and what waits.
func lastWords(c *crew.Crew, r pluginRow) string {
	out := "never run"

	if l := r.Last; l != nil {
		when := l.At.In(c.Location()).Format("Mon 15:04")
		if l.OK {
			out = fmt.Sprintf("%s %s ok %s", l.Event, l.ID, when)
		} else {
			out = fmt.Sprintf("%s %s failed %s: %s", l.Event, l.ID, when, clip(oneLine(l.Error), 80))
		}
	}

	if r.Waiting > 0 {
		out += fmt.Sprintf(" · %d waiting for a retry", r.Waiting)
	}

	if r.Partial != "" {
		out += " · last look partial: " + r.Partial
	}

	return out
}

func (a *app) pluginTestCommand() *cobra.Command {
	var event string

	c := &cobra.Command{
		Use:   "test <name> [--event ask]",
		Short: "Run a plugin once on an event made up for the test (marked \"test\": true) and print how it went",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.loadCrew()
			if err != nil {
				return err
			}

			pl, ok := c.PluginNamed(args[0])
			if !ok {
				return fmt.Errorf("no plugin %q in the crew file (plugins: %s)", args[0], pluginNames(c))
			}

			b := plugin.Builder{Crew: c.Path, Plugin: pl.Name, Clean: shift.QuoteN}

			ev, ok := b.Sample(event, time.Now())
			if !ok {
				return fmt.Errorf("--event %q: one of ask, blocking, dialog, reminder, answered", event)
			}

			prog, err := plugin.Resolve(pl, c.Layout().Dir)
			if err != nil {
				return err
			}

			res := plugin.Exec(plugin.Spec{Program: prog, Dir: c.Dir, Stdin: ev.Line(), Timeout: pl.TimeoutD(),
				Env: plugin.Env(os.Environ(), pl.Name, ev.Event, c.Path)})

			fmt.Fprintf(a.out, "%s: %s (test) → %s, %s\n", pl.Name, ev.Event, outcome(res, pl.TimeoutD()), res.Took.Round(time.Millisecond))
			fmt.Fprintf(a.out, "program: %s\nexit code: %d\n", prog.Path, res.Code)

			for _, o := range []struct{ label, text string }{{"stdout", res.Stdout}, {"stderr", res.Stderr}} {
				if text := printable(o.text); strings.TrimSpace(text) != "" {
					fmt.Fprintf(a.out, "%s:\n%s\n", o.label, indent(strings.TrimRight(text, "\n")))
				}
			}

			if !res.OK() {
				return &exitError{code: 1, msg: fmt.Sprintf("%s: %s", pl.Name, res.Reason(pl.TimeoutD()))}
			}

			return nil
		},
	}

	c.Flags().StringVar(&event, "event", "ask", "the event to send: ask, blocking, dialog, reminder or answered")

	return c
}

func outcome(r plugin.Result, timeout time.Duration) string {
	if r.OK() {
		return "taken"
	}

	return "failed (" + r.Reason(timeout) + ")"
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

// printable is what a plugin printed, fit for the owner's terminal: its lines as they were, with every control character but the
// tab gone (an escape sequence in it is data, not something for the terminal to act on).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == ' ' || unicode.IsGraphic(r) {
			return r
		}

		return -1
	}, s)
}

func pluginNames(c *crew.Crew) string {
	var names []string
	for _, p := range c.Plugins {
		names = append(names, p.Name)
	}

	if len(names) == 0 {
		return "none"
	}

	return strings.Join(names, ", ")
}

// pluginDeliverCommand is what the beat starts to deliver to the plugins (see pluginsBeat): hidden, run by the ticker and by
// tests. Run by hand it does the beat's delivery once.
func (a *app) pluginDeliverCommand() *cobra.Command {
	var beat bool

	c := &cobra.Command{
		Use:    "deliver",
		Short:  "Tell the plugins of what is owed them (the ticker starts this after each beat)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A role has no business telling the owner's plugins of anything: it could stand its own program in their place, or
			// page him as it likes.
			if role := os.Getenv("ZMINA_ROLE"); role != "" {
				return fmt.Errorf("zmina plugin deliver is the ticker's and the owner's; a role (%s) does not run it", role)
			}

			// The log names who ran it: the beat that started it, or the hand that ran it.
			caller := by()
			if beat {
				caller = "ticker"
			}

			return a.pluginsDeliver(time.Now(), caller)
		},
	}

	c.Flags().BoolVar(&beat, "beat", false, "started by the ticker's beat (the log names the ticker)")

	return c
}

// chooseCommand is zmina choose: press a choice of the question on a role's screen, by number.
func (a *app) chooseCommand() *cobra.Command {
	var dialog, voice string

	c := &cobra.Command{
		Use:   "choose <role> <n> [--dialog <id>]",
		Short: "Answer the question on a role's screen with its numbered choice n, as the arrows and Enter would",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 || n > 9 {
				return errors.New("choose <role> <n>: n is the number of a choice, 1 to 9")
			}

			role := args[0]

			return a.withState(func(s *shift.Shift) error {
				via, err := pluginVoice(s.Crew, voice)
				if err != nil {
					return err
				}

				// The id is role:fingerprint, or the fingerprint, and for a plugin's press with the nonce of the event it was
				// told in: role:fingerprint:nonce.
				id := strings.TrimPrefix(dialog, role+":")
				fingerprint, nonce, _ := strings.Cut(id, ":")

				// A plugin answers the dialog it was told of and no other, and once: a late tap on a phone must never press a key in
				// another question, nor a second time in this one.
				var (
					before func(d *shift.Dialog) error
					rearm  func()
				)

				if via != "" {
					if dialog == "" {
						return errors.New("--by needs --dialog <id>: a plugin answers only the question it was told of")
					}

					if nonce == "" {
						return errors.New("--dialog has no one-shot mark: give the id of the event as it was told (role:fingerprint:mark)")
					}

					armed := plugin.Armed{Path: s.Crew.Layout().PluginArmed(via), Lock: s.Crew.Layout().PluginArmedLock(via)}
					now := s.Now()

					spent := false

					before = func(d *shift.Dialog) error {
						if err := armed.Consume(nonce, plugin.DialogID(role, d.ID), now); err != nil {
							return err
						}

						spent = true

						return nil
					}

					// Keys that could not be sent are no press: the mark is given back.
					rearm = func() {
						if spent {
							_ = armed.Rearm(nonce, now)
						}
					}
				}

				d, err := pressChoice(s, role, n, fingerprint, dialog != "", before)
				if err != nil {
					if rearm != nil {
						rearm()
					}

					return err
				}

				label := ""
				for _, o := range d.Options {
					if o.N == n {
						label = o.Label
					}
				}

				detail := fmt.Sprintf("%d: %s", n, shift.QuoteN(label, 200))
				if via != "" {
					detail += " (answered through plugin " + via + ")"
				}

				s.Log(role, "chose", "", detail)

				// What was chosen is kept under the id the plugin was told, so that its answered says so.
				key := plugin.DialogID(role, d.ID)
				if via != "" {
					key = plugin.EventDialogID(role, d.ID, nonce)
				}

				noteChosen(s, role, key, answeredBy(by(), via), n, label)

				fmt.Fprintf(a.out, "%s · chose %d: %s\n", role, n, label)

				return nil
			})
		},
	}

	c.Flags().StringVar(&dialog, "dialog", "", "the question's id (role:fingerprint, as a plugin was told it): refused when the screen shows another")
	c.Flags().StringVar(&voice, "by", "", "a plugin answering in the owner's voice: its name, with ZMINA_PLUGIN=<name> set; needs --dialog")

	return c
}

// noteChosen keeps what was chosen of a dialog under its id (role:fingerprint, with the event's nonce when a plugin pressed), for the
// plugins' run to say how a dialog that left the screen ended; a choice
// made in a terminal says the owner is at the machine too.
func noteChosen(s *shift.Shift, role, id, by string, n int, label string) {
	if len(s.Crew.Plugins) == 0 {
		return
	}

	now := s.Now()
	_ = plugin.UpdateOwner(s.Crew.Layout().PluginOwner(), now, func(o *plugin.Owner) {
		if o.Chosen == nil {
			o.Chosen = map[string]plugin.Chosen{}
		}

		o.Chosen[id] = plugin.Chosen{By: by, N: n, Text: shift.QuoteN(label, plugin.OptionMax), At: now}

		if by == "owner" {
			o.Active = now
		}
	})
}
