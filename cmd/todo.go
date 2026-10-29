package cmd

import (
	"cmp"
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
	// Title is the item in a few words: the ask's title, the waiting task's headline.
	Title string `json:"title,omitempty"`
	// Project and URL are the task's, when the item is about one.
	Project string `json:"project,omitempty"`
	URL     string `json:"url,omitempty"`
	// Rec and Reason are a decision's recommendation; Note is a waiting task's note.
	Rec    string `json:"rec,omitempty"`
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
	Role   string `json:"role,omitempty"`
	Task   string `json:"task,omitempty"`
	Blocks string `json:"blocks,omitempty"`
	// Holds is set on the ui page: something stands until the owner answers (a waiting task, a blocking ask).
	Holds   bool      `json:"holds,omitempty"`
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

	// An ask about a task is that task's question: the task's waiting item folds into it, never shows twice.
	asked := map[string]int{}

	for _, a := range all {
		if a.Open() {
			if a.Task != "" {
				asked[a.Task] = len(items)
			}

			items = append(items, todoItem{ID: a.ID, Kind: string(a.Kind), Text: a.Text, Title: a.Title, Rec: a.Rec,
				Reason: a.Reason, Role: a.Role, Task: a.Task, Blocks: a.Blocks, Project: a.Project, Created: a.Created})
		}
	}

	tasks, p, _ := taskStore(c).Scan()
	problems = append(problems, p...)

	for _, t := range tasks {
		if i, ok := asked[t.ID]; ok {
			items[i].Project, items[i].URL = cmp.Or(items[i].Project, t.Where()), t.URL
		}

		// A task that waits on someone else is not the owner's to answer.
		if t.State != task.Waiting || t.On != "" || t.Answered.After(t.Updated) || t.Ignored.After(t.Updated) {
			continue
		}

		if i, ok := asked[t.ID]; ok {
			// The ask carries the question; the task adds its note and holds its role up the same way.
			if items[i].Note == "" {
				items[i].Note = t.Note
			}

			if items[i].Blocks == "" && idle[t.Role] {
				items[i].Blocks = t.Role
			}

			continue
		}

		it := todoItem{ID: t.ID, Kind: "waiting", Text: t.Asked, Title: t.Headline(), Note: t.Note, Role: t.Role,
			Task: t.ID, Project: t.Where(), URL: t.URL, Created: t.Updated}
		if idle[t.Role] {
			it.Blocks = t.Role
		}

		items = append(items, it)
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

	c.AddCommand(a.todoAddCommand(), a.todoLsCommand(), a.todoEditCommand(), a.todoAnswerCommand("yes"),
		a.todoAnswerCommand("done"), a.todoAnswerCommand("answer"), a.todoAnswerCommand("park"),
		a.todoAnswerCommand("ignore"))

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
	c.Flags().StringVar(&ask.Title, "title", "", "the ask in a few plain words (the page shows it, the text on expand)")
	c.Flags().StringVar(&ask.Project, "project", "", "the project it is for (default: its --task's)")

	return c
}

func (a *app) todoEditCommand() *cobra.Command {
	var e struct{ text, title, rec, reason, blocks, project string }

	c := &cobra.Command{
		Use:   "edit <id> [--title …] [--text …] [--rec …] [--reason …] [--blocks …] [--project …]",
		Short: "Change an open ask's words (- clears a field)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				store := askStore(s.Crew)

				ask, err := store.Get(args[0])
				if err != nil {
					return err
				}

				if !ask.Open() {
					return fmt.Errorf("%s is already %s", ask.ID, ask.State)
				}

				var changed []string

				for _, f := range []struct {
					name, value string
					field       *string
				}{{"text", e.text, &ask.Text}, {"title", e.title, &ask.Title}, {"rec", e.rec, &ask.Rec},
					{"reason", e.reason, &ask.Reason}, {"blocks", e.blocks, &ask.Blocks},
					{"project", e.project, &ask.Project}} {
					switch f.value {
					case "":
					case "-":
						if f.name == "text" {
							return errors.New("an ask keeps its text")
						}

						*f.field = ""
						changed = append(changed, f.name+" cleared")
					default:
						*f.field = f.value
						changed = append(changed, f.name)
					}
				}

				if len(changed) == 0 {
					return errors.New("nothing to change: give --title, --text, --rec, --reason, --blocks or --project")
				}

				// A recommendation makes it a decision; without one it is a check.
				if ask.Rec != "" {
					ask.Kind = asks.Decision
				} else {
					ask.Kind = asks.Check
				}

				ask.Updated = s.Now()
				if err := store.Put(ask); err != nil {
					return err
				}

				s.Log(by(), "asked", ask.Task, ask.ID+" edited: "+strings.Join(changed, ", "))
				fmt.Fprintf(a.out, "%s · %s\n", ask.ID, strings.Join(changed, ", "))

				return nil
			})
		},
	}

	c.Flags().StringVar(&e.title, "title", "", "the ask in a few plain words")
	c.Flags().StringVar(&e.text, "text", "", "the question or the check")
	c.Flags().StringVar(&e.rec, "rec", "", "the recommended answer (a decision; - makes it a check)")
	c.Flags().StringVar(&e.reason, "reason", "", "why, in one line")
	c.Flags().StringVar(&e.blocks, "blocks", "", "the role it holds up")
	c.Flags().StringVar(&e.project, "project", "", "the project it is for")

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
		"ignore": "Set it aside for good (no parked entry; a waiting task only leaves the todo)",
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
			"park": asks.Parked, "ignore": asks.Ignored}[action]
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
		case "ignore":
			note = ask.ID + " · ignored"
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
		case "ignore":
			// Off the todo only: the task itself stays as it is.
			t.Ignored = s.Now()
			if err := taskStore(s.Crew).Put(t); err != nil {
				return "", err
			}

			note = t.ID + " · ignored"
		default:
			return "", fmt.Errorf("%s is a task: answer, park or ignore it", t.ID)
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

	// The task's turn is the dispatcher's now: it leaves the todo until the task changes.
	if tid := taskOf(s, id); tid != "" {
		store := taskStore(s.Crew)
		if t, err := store.Get(tid); err == nil && t.State == task.Waiting {
			t.Answered = s.Now()
			if err := store.Put(t); err != nil {
				return "", err
			}
		}
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
