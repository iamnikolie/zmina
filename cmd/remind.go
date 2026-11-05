package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

// notifyWait is how long the crew's notify command may take for one reminder.
const notifyWait = 30 * time.Second

// whenHelp is what every --until and <when> takes.
const whenHelp = "morning (the next working day's start), today, tomorrow, a weekday (thu), a date (2026-11-08), " +
	"any of them followed by 09:00, 09:00 alone, or +90m / +3h / +2d; on the crew's timezone, a day at its day_start"

func alarmStore(c *crew.Crew) *alarm.Store { return &alarm.Store{Dir: c.Layout().Alarms()} }

// readNote reads --note: a file, or - for stdin; empty is no note.
func readNote(in io.Reader, path string) (string, error) {
	var (
		raw []byte
		err error
	)

	switch path {
	case "":
		return "", nil
	case "-":
		raw, err = io.ReadAll(in)
	default:
		raw, err = os.ReadFile(crew.ExpandHome(path))
	}

	if err != nil {
		return "", fmt.Errorf("--note: %w", err)
	}

	return strings.TrimSpace(string(raw)), nil
}

// at writes an alarm's time on the crew's clock with how far it is: Mon 2 Nov 09:00 (in 2h).
func at(s *shift.Shift, t time.Time) string {
	out := t.In(s.Crew.Location()).Format("Mon 2 Jan 15:04")

	if d := t.Sub(s.Now()); d > 0 {
		out += " (in " + age(d) + ")"
	} else {
		out += " (" + age(-d) + " ago)"
	}

	return out
}

func (a *app) remindCommand() *cobra.Command {
	var (
		note, role, to string
	)

	c := &cobra.Command{
		Use: "remind <when> <text...>",
		Short: "Set a reminder: for you (it lands on the todo and goes through the crew's notify command), for a role " +
			"(--role: started if down, told with Enter) or for another crew's dispatcher (--to node/crew)",
		Long: "Set a reminder. <when>: " + whenHelp + ". An alarm missed while the machine slept or the ticker was " +
			"down rings once on the first beat after it.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(strings.Fields(strings.Join(args[1:], " ")), " ")

			body, err := readNote(cmd.InOrStdin(), note)
			if err != nil {
				return err
			}

			if role != "" && to != "" {
				return errors.New("--role or --to, not both")
			}

			if to != "" {
				if _, err := mesh.ParseAddress(to); err != nil {
					return err
				}
			}

			return a.withState(func(s *shift.Shift) error {
				if role != "" {
					if err := alarmRole(s, role); err != nil {
						return err
					}
				}

				when, err := alarm.When(args[0], s.Now(), s.Clock())
				if err != nil {
					return err
				}

				al, err := s.Alarms.Add(alarm.Alarm{Kind: alarm.Remind, At: when, When: args[0], Text: text, Note: body,
					Role: role, To: to, By: s.By}, s.Now())
				if err != nil {
					return err
				}

				s.Log(role, "alarm", "", al.ID+" set for "+al.For()+" at "+at(s, al.At)+": "+text)
				fmt.Fprintf(a.out, "%s · %s · %s\n", al.ID, al.For(), at(s, al.At))

				return nil
			})
		},
	}

	c.Flags().StringVar(&note, "note", "", "a markdown file with what is told when it rings (- reads stdin)")
	c.Flags().StringVar(&role, "role", "", "the role it is for: started if down (on its parked work's conversation), "+
		"told the text and note with Enter")
	c.Flags().StringVar(&to, "to", "", "another crew, <node>[/<crew>]: its dispatcher is told at the time")
	c.AddCommand(a.remindLsCommand(), a.remindMoveCommand(), a.remindOffCommand())

	return c
}

// alarmRole checks a role an alarm is for: one of the crew, not a copy (gone once down).
func alarmRole(s *shift.Shift, name string) error {
	if _, _, ok := shift.SplitInstance(name); ok {
		return fmt.Errorf("%s is a copy, gone once down: set the alarm for its role", name)
	}

	_, err := s.Crew.Role(name)

	return err
}

func (a *app) remindLsCommand() *cobra.Command {
	var all, asJSON bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "The alarms still to ring, soonest first (reminders, sleeping roles, parked items to bring back)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				list := alarmStore(s.Crew).Pending()

				if all {
					var problems []string

					var err error
					if list, problems, err = alarmStore(s.Crew).Scan(); err != nil {
						return err
					}

					for _, p := range problems {
						fmt.Fprintln(a.err, "zmina: warning:", p)
					}
				}

				if asJSON {
					if list == nil {
						list = []alarm.Alarm{}
					}

					return a.printJSON(list)
				}

				if len(list) == 0 {
					fmt.Fprintln(a.out, "no alarms set")

					return nil
				}

				w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)

				for _, al := range list {
					line := al.ID + "\t" + at(s, al.At) + "\t" + al.For() + "\t" + string(al.Kind) + "\t"
					if all {
						line += alarmState(al) + "\t"
					}

					line += al.Text
					if al.Kind == alarm.Sleep && al.Note != "" {
						line = strings.TrimSuffix(line, al.Text) + strings.Join(strings.Fields(al.Note), " ")
					}
					if al.Later != "" {
						line = strings.TrimSuffix(line, al.Text) + al.Later + " · " + al.Text
					}

					fmt.Fprintln(w, line)
				}

				return w.Flush()
			})
		},
	}

	c.Flags().BoolVar(&all, "all", false, "also the alarms that rang or were taken off")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// alarmState is set, off, or rang with what it became.
func alarmState(al alarm.Alarm) string {
	out := string(al.State)
	if al.State == alarm.Rang && al.Ask != "" {
		out += " → " + al.Ask
	}

	if al.Err != "" {
		out += " (" + al.Err + ")"
	}

	return out
}

func (a *app) remindMoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "move <id> <when>",
		Short: "Move an alarm to another time (a reminder, a role's wake-up, a parked item's return)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				words := strings.Join(args[1:], " ")

				line, err := moveAlarm(s, args[0], words)
				if err != nil {
					return err
				}

				fmt.Fprintln(a.out, line)

				return nil
			})
		},
	}
}

// moveAlarm sets a pending alarm to the time words say; the caller holds the lock.
func moveAlarm(s *shift.Shift, id, words string) (string, error) {
	when, err := alarm.When(words, s.Now(), s.Clock())
	if err != nil {
		return "", err
	}

	al, err := alarmStore(s.Crew).Move(id, when, words, s.Now())
	if err != nil {
		return "", err
	}

	s.Log(al.Role, "alarm", al.Later, al.ID+" moved to "+at(s, al.At))

	return al.ID + " · " + al.For() + " · " + at(s, al.At), nil
}

func (a *app) remindOffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "off <id>",
		Short: "Take an alarm off; a sleeping role then stays down until started by hand",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				line, err := alarmOff(s, args[0])
				if err != nil {
					return err
				}

				fmt.Fprintln(a.out, line)

				return nil
			})
		},
	}
}

func alarmOff(s *shift.Shift, id string) (string, error) {
	al, err := alarmStore(s.Crew).TakeOff(id, s.Now())
	if err != nil {
		return "", err
	}

	s.Log(al.Role, "alarm", al.Later, al.ID+" off: "+al.Text)

	return al.ID + " · off", nil
}

func (a *app) sleepCommand() *cobra.Command {
	var (
		until, note string
		clear       bool
	)

	c := &cobra.Command{
		Use: "sleep <role> --until <when>",
		Short: "Put a role down holding what it holds, and wake it at --until: it starts on its last conversation " +
			"and is told --note with Enter",
		Long: "Put a role to sleep. --until: " + whenHelp + ". A role asleep already has its alarm moved.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if until == "" {
				return errors.New("--until: when it wakes")
			}

			body, err := readNote(cmd.InOrStdin(), note)
			if err != nil {
				return err
			}

			name := args[0]

			var (
				al   alarm.Alarm
				wait bool
				line string
			)

			err = a.withState(func(s *shift.Shift) error {
				when, err := alarm.When(until, s.Now(), s.Clock())
				if err != nil {
					return err
				}

				if al, wait, err = s.Sleep(name, when, until, body, shift.DownOptions{Clear: clear}); err != nil {
					return err
				}

				line = fmt.Sprintf("%s asleep until %s · %s", name, at(s, al.At), al.ID)
				if tasks := heldBy(s, name); len(tasks) > 0 {
					line += " · holds " + strings.Join(tasks, ", ")
				}

				return nil
			})
			if err != nil {
				return err
			}

			if wait {
				s, err := a.newShift()
				if err != nil {
					return err
				}

				shift.WaitGone(s.Tmux, []string{name}, shift.ExitWait)

				if err := a.withState(func(s *shift.Shift) error { return s.FinishDown(name) }); err != nil {
					return err
				}
			}

			fmt.Fprintln(a.out, line)

			return nil
		},
	}

	c.Flags().StringVar(&until, "until", "", "when it wakes: "+whenHelp)
	c.Flags().StringVar(&note, "note", "", "a markdown file with what it is told when it wakes (- reads stdin); "+
		"default: go on with the tasks it holds")
	c.Flags().BoolVar(&clear, "clear", false, "drop a draft (C-c) or close a dialog (Escape) first instead of refusing")

	return c
}

func heldBy(s *shift.Shift, name string) []string {
	var out []string

	for _, t := range s.Tasks.Active() {
		if t.Role == name {
			out = append(out, t.ID)
		}
	}

	return out
}

// rang does what the alarms rung by a beat left outside the lock: a reminder for the owner goes through the crew's
// notify command, a line for another crew is queued for it (the mesh round after the beat takes it). It returns
// lines for the log; a failure is also in the event log.
func (a *app) rang(rungs []shift.Rung) []string {
	c, err := a.loadCrew()
	if err != nil {
		return []string{"alarms: " + err.Error()}
	}

	var lines []string

	fail := func(id, what string, err error) {
		line := id + ": " + what + ": " + err.Error()
		lines = append(lines, line)
		_ = eventLog(c).Add(events.Event{TS: time.Now(), Event: "alarm-failed", By: "ticker", Detail: line})
	}

	for _, r := range rungs {
		switch {
		case r.Notify != "" && !c.Notify.Empty():
			if err := notify(c, r.ID, r.Notify); err != nil {
				fail(r.ID, "notify", err)
			} else {
				lines = append(lines, r.ID+": notified")
			}
		case r.Tell != "":
			if line, err := a.tellAlarm(r.Tell, r.Line); err != nil {
				fail(r.ID, "tell "+r.Tell, err)
			} else {
				lines = append(lines, r.ID+": "+line)
			}
		}
	}

	return lines
}

// notify runs the crew's notify command for one reminder: the text is its last argument (for a shell string, $1)
// and $ZMINA_REMIND; $ZMINA_ALARM is the alarm's id.
func notify(c *crew.Crew, id, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), notifyWait)
	defer cancel()

	line := c.Notify.Line()
	if c.Notify.Argv != nil {
		line = append(append([]string{}, line...), text)
	} else {
		line = append(line, "zmina", text)
	}

	cmd := exec.CommandContext(ctx, line[0], line[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), "ZMINA_REMIND="+text, "ZMINA_ALARM="+id, "ZMINA_CREW="+c.Path)

	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			line, _, _ := strings.Cut(msg, "\n")

			return fmt.Errorf("%w: %s", err, line)
		}

		return err
	}

	return nil
}

// tellAlarm queues an alarm's line for another crew's dispatcher, as zmina tell does: at once for a crew on this
// node, signed in the outbox for another node (the mesh round after the beat hands it over).
func (a *app) tellAlarm(to, line string) (string, error) {
	addr, err := mesh.ParseAddress(to)
	if err != nil {
		return "", err
	}

	me, err := mesh.Open(meshDir())
	if err != nil {
		return "", err
	}

	from := mesh.Address{Node: me.Name, Crew: a.crewKey()}

	if addr.Node == me.Name {
		if _, err := a.deliverHere(addr.Crew, from, "", line); err != nil {
			return "", err
		}

		return "told " + addr.String(), nil
	}

	e, err := me.NewEnvelope(from.Crew, addr, line, time.Now())
	if err != nil {
		return "", err
	}

	if err := me.Queue(e); err != nil {
		return "", err
	}

	return "queued for " + addr.String(), nil
}
