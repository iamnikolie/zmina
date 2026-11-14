package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/plugin"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// overdueAfter is how old a todo item gets before it is shown as overdue.
const overdueAfter = 24 * time.Hour

// decidedKeep is how long a decided item stays on the list: an older one is not shown, and the next todo decided
// closes it (expireDecided).
const decidedKeep = 7 * 24 * time.Hour

// todoItem is one thing waiting on the owner: an ask (decision or check) or a task in state waiting; or, last, a
// decided item, which waits on nobody and is there for him to read.
type todoItem struct {
	// Crew is the crew's key on the ui page, which shows every crew's todo as one list.
	Crew string `json:"crew,omitempty"`
	ID   string `json:"id"`
	Kind string `json:"kind"` // decision | check | decided | waiting | later
	Text string `json:"text"`
	// Title is the item in a few words: the ask's title, the waiting task's headline.
	Title string `json:"title,omitempty"`
	// Project and URL are the task's, when the item is about one.
	Project string `json:"project,omitempty"`
	URL     string `json:"url,omitempty"`
	// Rec and Reason are a decision's recommendation; Note is a waiting task's note. A decided item has its Reason and
	// what the role did not do (Instead).
	Rec     string `json:"rec,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Instead string `json:"instead,omitempty"`
	// Options are the answers the asker offered (an ask's --option).
	Options []string `json:"options,omitempty"`
	Note    string   `json:"note,omitempty"`
	Role    string   `json:"role,omitempty"`
	Task    string   `json:"task,omitempty"`
	Blocks  string   `json:"blocks,omitempty"`
	// Holds is set on the ui page: something stands until the owner answers (a waiting task, a blocking ask).
	Holds   bool      `json:"holds,omitempty"`
	Created time.Time `json:"created"`
	Overdue bool      `json:"overdue,omitempty"`
	// Pushed is when it last went through the notify command (see pushTick).
	Pushed time.Time `json:"pushed,omitzero"`
	// byAlarm: an ask a reminder made when it rang (its push went with the reminder).
	byAlarm bool
	// named is the title someone gave it (an ask's --title, a task's title), never one made from its text: the only
	// words of it a push sends.
	named string
}

// decided reports whether the item is a decided one: a role took the decision itself, and nothing waits on the owner.
func (it todoItem) decided() bool { return it.Kind == string(asks.Decided) }

// todo is what waits on the owner, in the order it is shown: blocking first, then overdue, then the rest, oldest
// first inside each group, and last the decided items (never overdue, never blocking), newest first. A waiting task
// blocks only while its role is up and idle (held up by it, not busy elsewhere or down); an ask blocks when it says
// so. A decided item older than decidedKeep is left out. Files that do not read are returned as problems.
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
	// A decided item about a task is no question: it only takes the task's project and link.
	decidedAbout := map[string][]int{}

	for _, a := range all {
		if a.Kind == asks.Decided {
			if a.Open() && now.Sub(a.Created) <= decidedKeep {
				if a.Task != "" {
					decidedAbout[a.Task] = append(decidedAbout[a.Task], len(items))
				}

				items = append(items, todoItem{ID: a.ID, Kind: string(a.Kind), Text: a.Text, Title: a.Title, Reason: a.Reason,
					Instead: a.Instead, Role: a.Role, Task: a.Task, Project: a.Project, Created: a.Created, named: a.Title})
			}

			continue
		}

		if a.Open() {
			if a.Task != "" {
				asked[a.Task] = len(items)
			}

			items = append(items, todoItem{ID: a.ID, Kind: string(a.Kind), Text: a.Text, Title: a.Title, Rec: a.Rec,
				Reason: a.Reason, Options: a.Options, Role: a.Role, Task: a.Task, Blocks: a.Blocks, Project: a.Project, Created: a.Created,
				byAlarm: a.By == "alarm", named: a.Title})
		}
	}

	tasks, p, _ := taskStore(c).Scan()
	problems = append(problems, p...)

	for _, t := range tasks {
		if i, ok := asked[t.ID]; ok {
			items[i].Project, items[i].URL = cmp.Or(items[i].Project, t.Where()), t.URL
		}

		for _, i := range decidedAbout[t.ID] {
			items[i].Project, items[i].URL = cmp.Or(items[i].Project, t.Where()), t.URL
		}

		// A task that waits on someone else is not the owner's to answer; one that names the owner is.
		if t.State != task.Waiting || c.OnOthers(t.On) || t.Answered.After(t.Updated) || t.Ignored.After(t.Updated) {
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
			Task: t.ID, Project: t.Where(), URL: t.URL, Created: t.Updated, named: t.Title}
		if idle[t.Role] {
			it.Blocks = t.Role
		}

		items = append(items, it)
	}

	// A parked item its alarm brought back (later add --until) is the owner's again until it is dealt with.
	for _, it := range laterStore(c).Parked() {
		if !it.Back.IsZero() {
			items = append(items, todoItem{ID: it.ID, Kind: "later", Text: it.Text, Title: it.Text, Note: it.Note,
				Created: it.Back})
		}
	}

	for i := range items {
		items[i].Overdue = !items[i].decided() && now.Sub(items[i].Created) >= overdueAfter
	}

	sortTodo(items)

	return items, problems
}

// sortTodo puts blocking items first, then overdue ones, then the rest, oldest first inside each group; the decided
// items are the last group, newest first.
func sortTodo(items []todoItem) {
	group := func(it todoItem) int {
		switch {
		case it.decided():
			return 3
		case it.Blocks != "":
			return 0
		case it.Overdue:
			return 1
		}

		return 2
	}

	sort.SliceStable(items, func(i, j int) bool {
		gi, gj := group(items[i]), group(items[j])
		if gi != gj {
			return gi < gj
		}

		if gi == 3 {
			return items[i].Created.After(items[j].Created)
		}

		return items[i].Created.Before(items[j].Created)
	})
}

func askStore(c *crew.Crew) *asks.Store {
	return &asks.Store{Dir: c.Layout().Asks()}
}

// recommendIsRec lets --recommend stand for --rec.
func recommendIsRec(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if name == "recommend" {
		name = "rec"
	}

	return pflag.NormalizedName(name)
}

// cleanOptions is the options an ask is given: each trimmed to one line, none empty, no more than a phone can show.
func cleanOptions(in []string) ([]string, error) {
	var out []string

	for _, o := range in {
		if o = oneLine(o); o != "" {
			out = append(out, o)
		}
	}

	if len(out) > plugin.OptionsMax {
		return nil, fmt.Errorf("at most %d options", plugin.OptionsMax)
	}

	return out, nil
}

func (a *app) todoCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "todo",
		Short: "What waits on the owner: the dispatcher's asks (decisions, checks) and tasks in state waiting",
	}

	c.AddCommand(a.todoAddCommand(), a.todoDecidedCommand(), a.todoLsCommand(), a.todoEditCommand(), a.todoAnswerCommand("yes"),
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
			opts, err := cleanOptions(ask.Options)
			if err != nil {
				return err
			}

			ask.Options = opts

			// A question with answers to pick from is a decision, with or without a recommendation among them.
			if kind == "" && len(opts) > 0 {
				kind = string(asks.Decision)
			}

			k, err := asks.ParseKind(kind, ask.Rec)
			if err != nil {
				return err
			}

			if k == asks.Decided {
				return errors.New("a decided item is recorded with zmina todo decided, which asks for its reason")
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
	c.Flags().StringVar(&ask.Rec, "rec", "", "the recommended answer: yes takes it (also --recommend)")
	c.Flags().StringArrayVar(&ask.Options, "option", nil, "an answer the owner can pick, in a few words (repeat it for each; a phone shows them as buttons)")
	c.Flags().SetNormalizeFunc(recommendIsRec)
	c.Flags().StringVar(&ask.Reason, "reason", "", "why, in one line")
	c.Flags().StringVar(&ask.Blocks, "blocks", "", "the role it holds up until answered")
	c.Flags().StringVar(&ask.Title, "title", "", "the ask in a few plain words (the page shows it, the text on expand)")
	c.Flags().StringVar(&ask.Project, "project", "", "the project it is for (default: its --task's)")

	return c
}

// todoDecidedCommand records a decision the role took on its own recommendation instead of waiting for the owner, for him
// to review afterwards. It is never blocking and has no recommendation or answers to pick from.
func (a *app) todoDecidedCommand() *cobra.Command {
	var ask asks.Ask

	c := &cobra.Command{
		Use:   "decided <text...> --reason \"<why>\" [--instead \"<what was not done>\"]",
		Short: "Record what you decided instead of waiting, for the owner to review afterwards; prints its id (A-3)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(ask.Reason) == "" {
				return errors.New("a decision needs its reason: --reason \"<why>\"")
			}

			ask.Kind, ask.Text, ask.By = asks.Decided, strings.Join(args, " "), by()
			if ask.Role == "" {
				ask.Role = ask.By
			}

			if ask.Task != "" {
				ask.Task = task.Normalize(ask.Task)
			}

			return a.withState(func(s *shift.Shift) error {
				expireDecided(s)

				added, err := askStore(s.Crew).Add(ask, s.Now())
				if err != nil {
					return err
				}

				s.Log(added.Role, "decided", added.Task, added.ID+": "+added.Text)
				fmt.Fprintln(a.out, added.ID)

				return nil
			})
		},
	}

	c.Flags().StringVar(&ask.Reason, "reason", "", "why you took it, in one line (required)")
	c.Flags().StringVar(&ask.Instead, "instead", "", "what you did not do")
	c.Flags().StringVar(&ask.Title, "title", "", "the decision in a few plain words (the page shows it, the text on expand)")
	c.Flags().StringVar(&ask.Role, "from", "", "the role that decided (default $ZMINA_ROLE, else hand)")
	c.Flags().StringVar(&ask.Task, "task", "", "the task it is about")
	c.Flags().StringVar(&ask.Project, "project", "", "the project it is for (default: its --task's)")

	return c
}

// expireDecided closes the decided items still open past decidedKeep, which the list stopped showing: they would stay
// open for good. zmina closes them itself (state ignored, no closer) and the log says which. The caller holds the state
// lock; an item that cannot be closed stays as it is, for the next time.
func expireDecided(s *shift.Shift) {
	store := askStore(s.Crew)
	all, _, _ := store.Scan()

	var expired []string

	for _, a := range all {
		if a.Kind != asks.Decided || !a.Open() || s.Now().Sub(a.Created) <= decidedKeep {
			continue
		}

		if _, err := store.CloseBy(a.ID, asks.Ignored, "", "", s.Now()); err == nil {
			expired = append(expired, a.ID)
		}
	}

	if len(expired) > 0 {
		s.Log("", "decided-expired", "", strings.Join(expired, " "))
	}
}

func (a *app) todoEditCommand() *cobra.Command {
	var e struct {
		text, title, rec, reason, blocks, project string
		options                                   []string
	}

	c := &cobra.Command{
		Use:   "edit <id> [--title …] [--text …] [--rec …] [--option …] [--reason …] [--blocks …] [--project …]",
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

				if ask.Kind == asks.Decided {
					return fmt.Errorf("%s is a decision already taken: it is not edited (record another with zmina todo decided)", ask.ID)
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

				// --option replaces the list; --option - clears it.
				if len(e.options) > 0 {
					if len(e.options) == 1 && e.options[0] == "-" {
						ask.Options = nil
						changed = append(changed, "options cleared")
					} else {
						opts, err := cleanOptions(e.options)
						if err != nil {
							return err
						}

						ask.Options = opts
						changed = append(changed, "options")
					}
				}

				if len(changed) == 0 {
					return errors.New("nothing to change: give --title, --text, --rec, --option, --reason, --blocks or --project")
				}

				// A recommendation or answers to pick from make it a decision; without either it is a check.
				if ask.Rec != "" || len(ask.Options) > 0 {
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
	c.Flags().StringVar(&e.rec, "rec", "", "the recommended answer (a decision; - makes it a check; also --recommend)")
	c.Flags().StringArrayVar(&e.options, "option", nil, "the answers the owner can pick, replacing the list (repeat it; - alone clears it)")
	c.Flags().SetNormalizeFunc(recommendIsRec)
	c.Flags().StringVar(&e.reason, "reason", "", "why, in one line")
	c.Flags().StringVar(&e.blocks, "blocks", "", "the role it holds up")
	c.Flags().StringVar(&e.project, "project", "", "the project it is for")

	return c
}

func (a *app) todoLsCommand() *cobra.Command {
	var asJSON, full bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "What waits on the owner: blocking first, then overdue, oldest first; then what roles decided, newest first; the parked list's count last",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				items, problems := todo(s.Crew, s.Now(), s.Views())
				withPushed(items, s.State)
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

				// A decided item waits on nobody: it is no reason to say something does.
				if !slices.ContainsFunc(items, func(it todoItem) bool { return !it.decided() }) {
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

					if !it.Pushed.IsZero() {
						meta += " · pushed " + it.Pushed.In(s.Crew.Location()).Format("Mon 15:04")
					}

					line := it.Text
					if !full && it.Title != "" {
						line = it.Title
					}
					switch {
					case it.Rec != "":
						line += " · recommended: " + it.Rec
						if it.Reason != "" {
							line += " (" + it.Reason + ")"
						}
					case it.Note != "":
						line += " · " + it.Note
					}

					if full && len(it.Options) > 0 {
						line += " · options: " + optionsLine(it.Options)
					}

					if full && it.decided() {
						line += " · reason: " + it.Reason
						if it.Instead != "" {
							line += " · instead: " + it.Instead
						}
					}

					if !full {
						line = clip(oneLine(line), 160)
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
	c.Flags().BoolVar(&full, "full", false, "each item's whole text (default: its title, one line)")

	return c
}

// optionsLine is an ask's options for a line: 1) yes  2) no.
func optionsLine(opts []string) string {
	parts := make([]string, len(opts))
	for i, o := range opts {
		parts[i] = fmt.Sprintf("%d) %s", i+1, o)
	}

	return strings.Join(parts, "  ")
}

func (a *app) todoAnswerCommand(action string) *cobra.Command {
	short := map[string]string{
		"yes":    "Take a decision's recommendation",
		"done":   "Say a check (or a decision) is dealt with",
		"answer": "Answer an ask or a waiting task in your own words",
		"park":   "Put it on the parked list for later",
		"ignore": "Set it aside for good (no parked entry; a waiting task only leaves the todo)",
	}[action]

	// The owner's own yes, done, ignore or park on a decided item tells the dispatcher nothing.
	quiet := ""
	if action != "answer" {
		quiet = " (not when the owner closes a decided item)"
	}

	use, args := action+" <id>", cobra.ExactArgs(1)
	if action == "answer" {
		use, args = "answer <id> <text...>", cobra.MinimumNArgs(2)
	}

	var from, voice string

	c := &cobra.Command{
		Use:   use,
		Short: short + "; the dispatcher hears it as a note" + quiet,
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				from = by()
			}

			return a.withState(func(s *shift.Shift) error {
				// A plugin's answer is the owner's own, given through it: it is recorded as his, and as through the plugin.
				via, err := pluginVoice(s.Crew, voice)
				if err != nil {
					return err
				}

				who := from
				if via != "" {
					who = "hand"
				}

				out, err := answerTodo(s, who, via, action, args[0], strings.Join(args[1:], " "))
				if err != nil {
					return err
				}

				fmt.Fprintln(a.out, out)

				return nil
			})
		},
	}

	c.Flags().StringVar(&from, "from", "", "who answers (default $ZMINA_ROLE, else hand)")
	c.Flags().StringVar(&voice, "by", "", "a plugin answering in the owner's voice: its name, with ZMINA_PLUGIN=<name> set (see Plugins)")

	return c
}

// pluginVoice is the plugin an answer is given through (--by): empty when none is named. The name must be a plugin of the
// crew file and ZMINA_PLUGIN must say it too, which the plugin's own run does: so an answer is never taken for a plugin's by
// a mistake. It is no proof of who is speaking: any process of the owner's can set it, and a plugin is trusted with the
// owner's voice and must itself check who is talking to it.
func pluginVoice(c *crew.Crew, by string) (string, error) {
	by = strings.TrimSpace(by)
	if by == "" {
		return "", nil
	}

	if _, ok := c.PluginNamed(by); !ok {
		return "", fmt.Errorf("--by %s: the crew file has no plugin of that name", by)
	}

	if os.Getenv("ZMINA_PLUGIN") != by {
		return "", fmt.Errorf("--by %s: only a plugin's own run answers in its voice; ZMINA_PLUGIN=%s is not set", by, by)
	}

	// A role is never a plugin: whatever a role's environment says, it does not speak with the owner's voice.
	if role := os.Getenv("ZMINA_ROLE"); role != "" {
		return "", fmt.Errorf("--by %s: a role (%s) does not answer in a plugin's voice", by, role)
	}

	return by, nil
}

// answeredBy is who an answer is recorded as coming from: the owner (his hand or the cockpit), a plugin he answered
// through, or the role that recorded what he said in chat.
func answeredBy(from, via string) string {
	switch {
	case via != "":
		return "plugin " + via
	case from == "hand" || from == "ui":
		return "owner"
	}

	return "role " + from
}

// answerTodo closes an ask (yes, done, answer, park) or answers a waiting task (answer, park), and queues the
// answer for the dispatcher as a note from `from` — "A-3 · yes: <rec>", "T-62 · <words>", "A-4 · later" — unless
// `from` is the dispatcher itself. A decided ask is the owner's to read: when he closes it with yes, done, ignore or park
// (in a terminal, on the page, through a plugin) nothing is queued (reviewed, fine), and an answer queues "A-5 · redo:
// <words> — decided: <text>" (he disagrees: the dispatcher acts on it). A role that closes one records what the owner
// said, as for any ask: the dispatcher is told. via is the plugin the owner answered through, when he did: the note says so. The first
// answer wins: an item that is closed or answered already is refused, whoever answers. The caller holds the state lock. It
// returns the line to print.
func answerTodo(s *shift.Shift, from, via, action, id, words string) (string, error) {
	id = asks.Normalize(id)
	if action == "answer" && strings.TrimSpace(words) == "" {
		return "", errors.New("an answer needs words")
	}

	var (
		note string
		// reviewed is a decided ask the owner read and found fine (or parked): nothing is queued. decided is any answer to
		// a decided ask, which is no answer to the task it is about.
		reviewed, decided bool
	)

	switch {
	case strings.HasPrefix(id, "A-"):
		store := askStore(s.Crew)

		ask, err := store.Get(id)
		if err != nil {
			return "", err
		}

		st := map[string]asks.State{"yes": asks.Yes, "done": asks.Done, "answer": asks.Answered,
			"park": asks.Parked, "ignore": asks.Ignored}[action]
		decided = ask.Kind == asks.Decided
		reviewed = decided && action != "answer" && (from == "hand" || from == "ui" || via != "")

		if action == "yes" && ask.Rec == "" && !decided {
			return "", fmt.Errorf("%s has no recommendation to say yes to: answer it", ask.ID)
		}

		if ask, err = store.CloseBy(ask.ID, st, words, answeredBy(from, via), s.Now()); err != nil {
			return "", err
		}

		switch action {
		case "yes":
			note = ask.ID + " · yes"
			if ask.Rec != "" {
				note += ": " + ask.Rec
			}
		case "done":
			note = ask.ID + " · done"
		case "answer":
			note = ask.ID + " · " + words
			if decided {
				note = ask.ID + " · redo: " + words
			}
		case "ignore":
			note = ask.ID + " · ignored"
		case "park":
			it, err := laterStore(s.Crew).Add(later.Item{Text: ask.Text, Note: "from " + ask.ID, By: from}, s.Now())
			if err != nil {
				return "", err
			}

			note = ask.ID + " · later (" + it.ID + ")"
		}

		if decided {
			note += " — decided: " + clip(ask.Text, 120)
		} else {
			note += " — " + clip(ask.Text, 120)
		}

		id = ask.ID
	case strings.HasPrefix(id, "T-"):
		t, err := taskStore(s.Crew).Get(id)
		if err != nil {
			return "", err
		}

		// The first answer wins over a plugin's: a waiting task the owner has answered, or set aside, since it last changed
		// waits on him no more, and a late tap on a phone does not answer it again. At the keyboard a follow-up answer, or one
		// after he set it aside, is his to give as it always was.
		if via != "" && t.State == task.Waiting && (action == "answer" || action == "park" || action == "ignore") &&
			(t.Answered.After(t.Updated) || t.Ignored.After(t.Updated)) {
			return "", fmt.Errorf("%s is already answered (a plugin's answer is its first: answer it in a terminal to say more)", t.ID)
		}

		switch action {
		case "answer":
			note = t.ID + " · " + words
		case "park":
			note = t.ID + " · later"
		case "ignore":
			// Off the todo only: the task itself stays as it is.
			t.Ignored, t.AnsweredBy = s.Now(), answeredBy(from, via)
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

			// Off the todo: the turn is the dispatcher's.
			if !it.Back.IsZero() {
				it.Back = time.Time{}
				if err := store.Put(it); err != nil {
					return "", err
				}
			}

			note = it.ID + " · bring back — " + clip(it.Text, 120)
		case "drop", "done":
			st := map[string]later.State{"drop": later.Dropped, "done": later.Done}[action]
			if it, err = store.Close(it.ID, st, "", s.Now()); err != nil {
				return "", err
			}

			note = it.ID + " · " + string(st) + " — " + clip(it.Text, 120)
		case "park":
			// Back on the parked list, off the todo; nothing for the dispatcher to do.
			if !it.Open() {
				return "", fmt.Errorf("%s is already %s", it.ID, it.State)
			}

			it.Back = time.Time{}
			if err := store.Put(it); err != nil {
				return "", err
			}

			s.Log(from, "later", it.ID, "parked again")

			return it.ID + " · parked again", nil
		default:
			return "", fmt.Errorf("%s is parked: bring it back, park it again, or drop it", it.ID)
		}

		id = it.ID
	default:
		return "", fmt.Errorf("%s: not an ask (A-n), a waiting task (T-n) or a parked item (L-n)", id)
	}

	switch {
	case via != "":
		// Through a plugin: the owner's own answer, given on his phone or wherever the plugin reaches him.
		note += " (answered through plugin " + via + ")"
	case from != "hand" && from != "ui" && os.Getenv("ZMINA_ROLE") != "":
		// A role that records what the owner said in chat says so: the answer is his words relayed, not his own click.
		note += " (the owner's answer in chat, recorded by " + from + ")"
	}

	// The note is how the dispatcher hears an answer given elsewhere; one it records itself it has already.
	role, _, _ := strings.Cut(from, "#")
	self := role == s.Crew.Dispatcher

	if !self && !reviewed {
		if _, err := s.Note(from, note, false); err != nil {
			return "", err
		}
	}

	// The task's turn is the dispatcher's now: it leaves the todo until the task changes. A decided item is not the
	// task's question, so it leaves the task as it is.
	if tid := taskOf(s, id); tid != "" && !decided {
		store := taskStore(s.Crew)
		if t, err := store.Get(tid); err == nil && t.State == task.Waiting {
			t.Answered, t.AnsweredBy = s.Now(), answeredBy(from, via)
			if err := store.Put(t); err != nil {
				return "", shift.Partial(err) // the ask is closed and the dispatcher's note queued: keep the note
			}
		}
	}

	s.Log(from, "answered", taskOf(s, id), note)

	// An answer given in a terminal says the owner is at the machine, which holds a plugin's pushes for a while.
	if from == "hand" && via == "" {
		touchOwner(s)
	}

	if self {
		return id + " · " + action + " · recorded", nil
	}

	if reviewed {
		if action == "park" {
			return id + " · park · parked, nothing queued", nil
		}

		return id + " · " + action + " · reviewed, nothing queued", nil
	}

	// The answer goes to the dispatcher now when it can take it, else with the beat.
	s.Deliver()

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
