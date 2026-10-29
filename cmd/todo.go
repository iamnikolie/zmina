package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// overdueAfter is how old a todo item gets before it is shown as overdue.
const overdueAfter = 24 * time.Hour

// todoItem is one thing waiting on the owner: an ask (decision or check) or a task in state waiting.
type todoItem struct {
	// Crew is the crew's key on the ui page, which shows every crew's todo as one list.
	Crew string `json:"crew,omitempty"`
	ID   string `json:"id"`
	Kind string `json:"kind"` // decision | check | waiting
	Text string `json:"text"`
	// Rec and Reason are a decision's recommendation; Note is a waiting task's note.
	Rec     string    `json:"rec,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Note    string    `json:"note,omitempty"`
	Role    string    `json:"role,omitempty"`
	Task    string    `json:"task,omitempty"`
	Blocks  string    `json:"blocks,omitempty"`
	Created time.Time `json:"created"`
	Overdue bool      `json:"overdue,omitempty"`
}

// todo is what waits on the owner, in the order it is shown: blocking first, then overdue, then the rest, oldest
// first inside each group. A waiting task blocks only while its role is up and idle (held up by it, not busy
// elsewhere or down); an ask blocks when it says so. Files that do not read are returned as problems.
func todo(c *crew.Crew, now time.Time, views []shift.View) ([]todoItem, []string) {
	idle := map[string]bool{}
	for _, v := range views {
		idle[v.Role] = v.State == shift.PresenceUp && v.Status == "idle"
	}

	var (
		items    []todoItem
		problems []string
	)

	all, p, _ := askStore(c).Scan()
	problems = append(problems, p...)

	for _, a := range all {
		if a.Open() {
			items = append(items, todoItem{ID: a.ID, Kind: string(a.Kind), Text: a.Text, Rec: a.Rec, Reason: a.Reason,
				Role: a.Role, Task: a.Task, Blocks: a.Blocks, Created: a.Created})
		}
	}

	tasks, p, _ := taskStore(c).Scan()
	problems = append(problems, p...)

	for _, t := range tasks {
		if t.State == task.Waiting {
			it := todoItem{ID: t.ID, Kind: "waiting", Text: t.Asked, Note: t.Note, Role: t.Role, Task: t.ID,
				Created: t.Updated}
			if idle[t.Role] {
				it.Blocks = t.Role
			}

			items = append(items, it)
		}
	}

	for i := range items {
		items[i].Overdue = now.Sub(items[i].Created) >= overdueAfter
	}

	sortTodo(items)

	return items, problems
}

// sortTodo puts blocking items first, then overdue ones, then the rest, oldest first inside each group.
func sortTodo(items []todoItem) {
	group := func(it todoItem) int {
		switch {
		case it.Blocks != "":
			return 0
		case it.Overdue:
			return 1
		}

		return 2
	}

	sort.SliceStable(items, func(i, j int) bool {
		if gi, gj := group(items[i]), group(items[j]); gi != gj {
			return gi < gj
		}

		return items[i].Created.Before(items[j].Created)
	})
}

func askStore(c *crew.Crew) *asks.Store {
	return &asks.Store{Dir: c.Layout().Asks()}
}

func (a *app) todoCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "todo",
		Short: "What waits on the owner: the dispatcher's asks (decisions, checks) and tasks in state waiting",
	}

	c.AddCommand(a.todoAddCommand(), a.todoLsCommand(), a.todoAnswerCommand("yes"), a.todoAnswerCommand("done"),
		a.todoAnswerCommand("answer"), a.todoAnswerCommand("park"))

	return c
}

func (a *app) todoAddCommand() *cobra.Command {
	var ask asks.Ask

	var kind string

	c := &cobra.Command{
		Use:   "add <text...>",
		Short: "Ask the owner: a decision (--rec, --reason) or a check; prints its id (A-3)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := asks.ParseKind(kind, ask.Rec)
			if err != nil {
				return err
			}

			ask.Kind, ask.Text, ask.By = k, strings.Join(args, " "), by()
			if ask.Role == "" {
				ask.Role = ask.By
			}

			if ask.Task != "" {
				ask.Task = task.Normalize(ask.Task)
			}

			return a.withState(func(s *shift.Shift) error {
				added, err := askStore(s.Crew).Add(ask, s.Now())
				if err != nil {
					return err
				}

				s.Log(added.Role, "asked", added.Task, added.ID+" "+string(added.Kind)+": "+added.Text)
				fmt.Fprintln(a.out, added.ID)

				return nil
			})
		},
	}

	c.Flags().StringVar(&kind, "kind", "", "decision or check (default: decision with --rec, else check)")
	c.Flags().StringVar(&ask.Role, "from", "", "the role that asks (default $ZMINA_ROLE, else hand)")
	c.Flags().StringVar(&ask.Task, "task", "", "the task it is about")
	c.Flags().StringVar(&ask.Rec, "rec", "", "the recommended answer: yes takes it")
	c.Flags().StringVar(&ask.Reason, "reason", "", "why, in one line")
	c.Flags().StringVar(&ask.Blocks, "blocks", "", "the role it holds up until answered")

	return c
}

func (a *app) todoLsCommand() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "What waits on the owner: blocking first, then overdue, oldest first; the parked list's count last",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				items, problems := todo(s.Crew, s.Now(), s.Views())
				for _, p := range problems {
					fmt.Fprintln(a.err, "zmina: warning:", p)
				}

				parked := laterStore(s.Crew).Parked()

				if asJSON {
					if items == nil {
						items = []todoItem{}
					}

					if parked == nil {
						parked = []later.Item{}
					}

					return a.printJSON(struct {
						Items []todoItem   `json:"items"`
						Later []later.Item `json:"later"`
					}{items, parked})
				}

				if len(items) == 0 {
					fmt.Fprintln(a.out, "nothing waits on you")
				}

				w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)

				for _, it := range items {
					meta := it.Role
					if it.Task != "" && it.Task != it.ID {
						meta += " · " + it.Task
					}

					if it.Blocks != "" {
						meta += " · blocks " + it.Blocks
					}

					line := it.Text
					switch {
					case it.Rec != "":
						line += " · recommended: " + it.Rec
						if it.Reason != "" {
							line += " (" + it.Reason + ")"
						}
					case it.Note != "":
						line += " · " + it.Note
					}

					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", it.ID, it.Kind, age(s.Now().Sub(it.Created)), meta, line)
				}

				if err := w.Flush(); err != nil {
					return err
				}

				if len(parked) > 0 {
					fmt.Fprintf(a.out, "later: %d parked (zmina later ls)\n", len(parked))
				}

				return nil
			})
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

func (a *app) todoAnswerCommand(action string) *cobra.Command {
	short := map[string]string{
		"yes":    "Take a decision's recommendation",
		"done":   "Say a check (or a decision) is dealt with",
		"answer": "Answer an ask or a waiting task in your own words",
		"park":   "Put it on the parked list for later",
	}[action]

	use, args := action+" <id>", cobra.ExactArgs(1)
	if action == "answer" {
		use, args = "answer <id> <text...>", cobra.MinimumNArgs(2)
	}

	var from string

	c := &cobra.Command{
		Use:   use,
		Short: short + "; the dispatcher hears it as a note",
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				from = by()
			}

			return a.withState(func(s *shift.Shift) error {
				out, err := answerTodo(s, from, action, args[0], strings.Join(args[1:], " "))
				if err != nil {
					return err
				}

				fmt.Fprintln(a.out, out)

				return nil
			})
		},
	}

	c.Flags().StringVar(&from, "from", "", "who answers (default $ZMINA_ROLE, else hand)")

	return c
}

// answerTodo closes an ask (yes, done, answer, park) or answers a waiting task (answer, park), and queues the
// answer for the dispatcher as a note from `from` — "A-3 · yes: <rec>", "T-62 · <words>", "A-4 · later". The
// caller holds the state lock. It returns the line to print.
func answerTodo(s *shift.Shift, from, action, id, words string) (string, error) {
	id = asks.Normalize(id)
	if action == "answer" && strings.TrimSpace(words) == "" {
		return "", errors.New("an answer needs words")
	}

	var note string

	switch {
	case strings.HasPrefix(id, "A-"):
		store := askStore(s.Crew)

		ask, err := store.Get(id)
		if err != nil {
			return "", err
		}

		st := map[string]asks.State{"yes": asks.Yes, "done": asks.Done, "answer": asks.Answered,
			"park": asks.Parked}[action]
		if action == "yes" && ask.Rec == "" {
			return "", fmt.Errorf("%s has no recommendation to say yes to: answer it", ask.ID)
		}

		if ask, err = store.Close(ask.ID, st, words, s.Now()); err != nil {
			return "", err
		}

		switch action {
		case "yes":
			note = ask.ID + " · yes: " + ask.Rec
		case "done":
			note = ask.ID + " · done"
		case "answer":
			note = ask.ID + " · " + words
		case "park":
			it, err := laterStore(s.Crew).Add(later.Item{Text: ask.Text, Note: "from " + ask.ID, By: from}, s.Now())
			if err != nil {
				return "", err
			}

			note = ask.ID + " · later (" + it.ID + ")"
		}

		note += " — " + clip(ask.Text, 120)
		id = ask.ID
	case strings.HasPrefix(id, "T-"):
		t, err := taskStore(s.Crew).Get(id)
		if err != nil {
			return "", err
		}

		switch action {
		case "answer":
			note = t.ID + " · " + words
		case "park":
			note = t.ID + " · later"
		default:
			return "", fmt.Errorf("%s is a task: answer it or park it", t.ID)
		}

		id = t.ID
	case strings.HasPrefix(id, "L-"):
		store := laterStore(s.Crew)

		it, err := store.Get(id)
		if err != nil {
			return "", err
		}

		switch action {
		case "back":
			// The dispatcher brings it back: it takes the item into a task (zmina later take) or asks.
			if !it.Open() {
				return "", fmt.Errorf("%s is already %s", it.ID, it.State)
			}

			note = it.ID + " · bring back — " + clip(it.Text, 120)
		case "drop":
			if it, err = store.Close(it.ID, later.Dropped, "", s.Now()); err != nil {
				return "", err
			}

			note = it.ID + " · dropped — " + clip(it.Text, 120)
		default:
			return "", fmt.Errorf("%s is parked: bring it back or drop it", it.ID)
		}

		id = it.ID
	default:
		return "", fmt.Errorf("%s: not an ask (A-n), a waiting task (T-n) or a parked item (L-n)", id)
	}

	if _, err := s.Note(from, note, false); err != nil {
		return "", err
	}

	s.Log(from, "answered", taskOf(s, id), note)

	return id + " · " + action + " · queued for the dispatcher", nil
}

// taskOf is the task an ask or a task id is about, for the log.
func taskOf(s *shift.Shift, id string) string {
	if strings.HasPrefix(id, "T-") {
		return id
	}

	if ask, err := askStore(s.Crew).Get(id); err == nil {
		return ask.Task
	}

	return ""
}
