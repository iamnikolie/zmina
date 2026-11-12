package cmd

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
)

// Every id zmina prints is one of six families. zmina show reads any of them as a few plain lines — what it is in
// words, who has it, where it stands and since when, the one command to act — so output that names only an id
// is never a dead end.

// anyIDRe is an id of any family, any case: T-14, A-3, L-7, S-12, R-4, LS-2.
var anyIDRe = regexp.MustCompile(`^(?i)(T|A|L|S|R|LS)-?([0-9]{1,7})$`)

// idFamilies names the families in words, for a miss.
const idFamilies = "T-n (task), A-n (question or check for the owner), L-n (parked), S-n (a line waiting for an ack), " +
	"R-n (reminder or alarm), LS-n (lesson)"

func (a *app) showCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>...",
		Short: "Any id in words: a task, question, parked item, line, reminder or lesson (T-, A-, L-, S-, R-, LS-)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]string, 0, len(args))

			for _, arg := range args {
				id, err := normalID(arg)
				if err != nil {
					return err
				}

				ids = append(ids, id)
			}

			return a.readState(func(s *shift.Shift) error {
				var x *itemIndex

				for i, id := range ids {
					if i > 0 {
						fmt.Fprintln(a.out)
					}

					fam, _, _ := strings.Cut(id, "-")
					switch fam {
					case "T", "L", "A":
						if x == nil {
							x = readItemIndex(s)
						}

						fmt.Fprint(a.out, itemText(x.card(id), x.now))
					case "S":
						fmt.Fprint(a.out, a.lineText(s, id))
					case "R":
						fmt.Fprint(a.out, alarmText(s, id))
					case "LS":
						fmt.Fprint(a.out, lessonText(s, id))
					}
				}

				return nil
			})
		},
	}
}

// normalID reads an id of any family in any case, with or without its dash: t-14, T14, ls-2.
func normalID(s string) (string, error) {
	m := anyIDRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("%q is no id zmina knows: %s", s, idFamilies)
	}

	return strings.ToUpper(m[1]) + "-" + strings.TrimLeft(m[2], "0"), nil
}

// textCard is a card of the plain families as lines: the head line (id and what it is), the details, the one command
// to act. zmina show prints it; the cockpit's /api/items serves it as it is.
type textCard struct {
	Head  string   `json:"head"`
	Lines []string `json:"lines,omitempty"`
	Act   string   `json:"act,omitempty"`
}

func (c textCard) String() string {
	var b strings.Builder

	fmt.Fprintln(&b, c.Head)

	for _, l := range c.Lines {
		if l != "" {
			fmt.Fprintln(&b, "  "+l)
		}
	}

	if c.Act != "" {
		fmt.Fprintln(&b, "  act: "+c.Act)
	}

	return b.String()
}

// cardText is a textCard's lines.
func cardText(head string, lines []string, act string) string {
	return textCard{Head: head, Lines: lines, Act: act}.String()
}

// itemText is a task, parked item or ask as the cockpit's card says it, in lines.
func itemText(c itemCard, now time.Time) string {
	if !c.Found {
		return cardText(c.ID+" · not found in this crew's ledger", nil, "")
	}

	if c.Ask != nil {
		q := c.Ask
		who := "from " + dash(q.Role)
		if q.Task != "" {
			who += " · about " + q.Task
		}

		state := map[string]string{"need": "waits on the owner", "parked": "parked", "done": "answered"}[c.State]
		act := ""
		if c.State == "need" {
			act = "zmina todo answer " + q.ID + " \"<answer>\""
			switch {
			case c.Decision != nil && c.Decision.Action == "done":
				act = "zmina todo done " + q.ID
			case c.Decision != nil && c.Decision.Action == "yes":
				act = "zmina todo yes " + q.ID + " (its recommendation), or " + act
			}
		}

		title := clip(firstOf(q.Title, oneLine(q.Text)), 160)
		asks := clip(oneLine(q.Text), 300)
		if asks == title {
			asks = ""
		}

		return cardText(q.ID+" · "+title, []string{
			string(q.Kind) + " for the owner · " + state + " · " + age(now.Sub(c.Since)) + " · " + who,
			ifSet("asks: ", asks),
			ifSet("recommends: ", q.Rec),
		}, act)
	}

	var at itemStep
	chain := make([]string, 0, len(c.Chain))

	for i, st := range c.Chain {
		word := st.ID
		if i == c.At {
			at = st
			word += " (this)"
		}

		chain = append(chain, word)
	}

	head := c.Chain[len(c.Chain)-1]
	kind := map[string]string{"task": "task", "later": "parked item"}[at.Kind]

	where := ""
	if at.Project != "" {
		where = " · project " + at.Project
	}

	lines := []string{
		kind + " · " + itemState(c) + " · " + age(now.Sub(c.Since)) + where,
	}

	if len(chain) > 1 {
		lines = append(lines, "one piece of work: "+strings.Join(chain, " → "))
	}

	lines = append(lines, ifSet("note: ", clip(oneLine(at.Note), 300)), ifSet("result: ", clip(oneLine(at.Result), 300)))

	act := "zmina task show " + head.ID
	if head.Kind == "later" {
		act = "zmina later take " + head.ID + " <role>"
		if c.State != "parked" {
			act = "zmina log --task " + head.ID
		}
	}

	return cardText(at.ID+" · "+clip(firstOf(oneLine(at.Title), "(no title)"), 160), lines, act)
}

// itemState is where a piece of work stands, in words.
func itemState(c itemCard) string {
	switch c.State {
	case "need":
		return "waits on the owner"
	case "busy":
		return c.Who + " is on it"
	case "with":
		if c.Idle {
			return "with " + c.Who + " (idle)"
		}

		return "with " + c.Who
	case "queued":
		return "handed to " + c.Who + ", not taken yet"
	case "on":
		return "waits on " + c.Who
	case "parked":
		if c.Back != "" {
			return "waits to be taken back"
		}

		if c.Who != "" {
			return "parked (from " + c.Who + ")"
		}

		return "parked"
	}

	return c.State
}

// lineText is a line that waits, or waited, for its reader's ack.
func (a *app) lineText(s *shift.Shift, id string) string {
	c, _ := lineCard(s, id)

	return c.String()
}

// lineCard is lineText's lines; false when the line waits for no ack and is not in the log.
func lineCard(s *shift.Shift, id string) (textCard, bool) {
	if ack, ok := s.State.Acks[id]; ok {
		state := "queued for " + ack.Role + ", not delivered yet"
		if !ack.First.IsZero() {
			state = "delivered to " + ack.Role + " " + age(s.Now().Sub(ack.First)) + " ago, waits for its ack"
			if ack.Renotes > 0 {
				state += fmt.Sprintf(" (said again %d×)", ack.Renotes)
			}
		}

		return textCard{Head: id + " · " + clip(oneLine(ack.Text), 120), Lines: []string{"a line · " + state},
			Act: "zmina pending --role " + ack.Role}, true
	}

	evs, err := eventLog(s.Crew).Read(events.Filter{})
	if err == nil {
		for _, e := range slices.Backward(evs) {
			if (e.Event == "acked" || e.Event == "ack-dropped") && strings.HasPrefix(e.Detail, id+" ") {
				text := strings.TrimPrefix(e.Detail, id+" ")
				state := e.Event + " by " + dash(e.By)
				if e.Event == "ack-dropped" {
					state = "dropped unacked (too many waiting)"
				}

				return textCard{Head: id + " · " + clip(oneLine(text), 120),
					Lines: []string{"a line for " + dash(e.Role) + " · " + state + " · " + e.TS.Local().Format(time.DateTime)},
					Act:   "zmina log --role " + e.Role}, true
			}
		}
	}

	return textCard{Head: id + " · waits for no ack and is not in the log"}, false
}

// alarmText is a reminder or alarm.
func alarmText(s *shift.Shift, id string) string {
	c, _ := alarmCard(s, id)

	return c.String()
}

// alarmCard is alarmText's lines; false when the alarm does not read.
func alarmCard(s *shift.Shift, id string) (textCard, bool) {
	al, err := alarmStore(s.Crew).Get(id)
	if err != nil {
		return textCard{Head: id + " · " + err.Error()}, false
	}

	when := al.At.In(s.Crew.Location()).Format("Mon 2 Jan 15:04")
	state := "rings " + when + " (in " + age(al.At.Sub(s.Now())) + ")"

	switch al.State {
	case alarm.Rang:
		state = "rang " + when + ifSet(" → ", al.Ask)
	case alarm.Off:
		state = "taken off"
	case alarm.Failed:
		state = "failed, not delivered after " + tries(al.Tries) + " · was due " + when
	default:
		if al.Owed != "" {
			state = "due " + when + ", not delivered yet (" + tries(al.Tries) + ")"
		}
	}

	what := map[alarm.Kind]string{alarm.Later: "brings back " + al.Later}[al.Kind]
	if what == "" {
		what = string(al.Kind)
	}

	act := "zmina remind ls"
	if al.Pending() || al.State == alarm.Failed {
		act = "zmina remind off " + al.ID
	}

	return textCard{Head: al.ID + " · " + clip(oneLine(al.Text), 160), Lines: []string{
		what + " · for " + al.For() + " · " + state,
		ifSet("error: ", al.Err),
		ifSet("last try failed: ", al.TryErr),
	}, Act: act}, true
}

// lessonText is a lesson.
func lessonText(s *shift.Shift, id string) string {
	c, _ := lessonCard(s, id)

	return c.String()
}

// lessonCard is lessonText's lines; false when the lesson does not read.
func lessonCard(s *shift.Shift, id string) (textCard, bool) {
	l, err := lessonStore(s.Crew).Get(id)
	if err != nil {
		return textCard{Head: id + " · " + err.Error()}, false
	}

	from := "from " + dash(l.Role)
	if l.Task != "" {
		from += " · " + l.Task
	}

	return textCard{Head: l.ID + " · " + clip(oneLine(l.Rule), 160), Lines: []string{
		"lesson · " + string(l.State) + " · " + age(s.Now().Sub(l.At)) + " · " + from,
		ifSet("why: ", clip(oneLine(l.Why), 300)),
	}, Act: "zmina lesson show " + l.ID}, true
}

// plainCard is the card of an id of the families that have no chain: a line (S-), a reminder or alarm (R-), a lesson
// (LS-), the same lines zmina show prints. The cockpit's /api/items serves it.
func plainCard(s *shift.Shift, id string) itemCard {
	c := itemCard{ID: id}

	var (
		card textCard
		ok   bool
	)

	switch {
	case strings.HasPrefix(id, "LS-"):
		c.Kind = "lesson"
		card, ok = lessonCard(s, id)
	case strings.HasPrefix(id, "S-"):
		c.Kind = "line"
		card, ok = lineCard(s, id)
	default:
		c.Kind = "alarm"
		card, ok = alarmCard(s, id)
	}

	if !ok {
		return itemCard{ID: id}
	}

	c.Found, c.Plain = true, &card

	return c
}

func ifSet(prefix, s string) string {
	if s == "" {
		return ""
	}

	return prefix + s
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}

	return ""
}
