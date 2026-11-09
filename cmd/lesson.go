package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/lesson"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// Lessons: roles file the rules they learned; a daily triage (lessons: on in the crew file) sorts them into one
// draft merge request on the crew's agentic-os, which only the owner's merge applies.

const (
	// lessonsCheckEvery is how often an open lessons merge request is looked at on the remote.
	lessonsCheckEvery = time.Hour
	// lessonsRunStale is how long a run may hold the record before it is taken as gone.
	lessonsRunStale = time.Hour
)

// lessonSeat is the seat a run uses (a variable for tests).
var lessonSeat = lesson.ClaudeSeat

func lessonDir(c *crew.Crew) string { return filepath.Join(c.StateDir(), "lessons") }

func lessonStore(c *crew.Crew) *lesson.Store {
	return &lesson.Store{Dir: filepath.Join(lessonDir(c), "items")}
}

func (a *app) lessonCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "lesson",
		Short: "Lessons the roles learned: add one, list them; a daily triage offers them as one draft MR (lessons: on)",
	}

	c.AddCommand(a.lessonAddCommand(), a.lessonLsCommand(), a.lessonShowCommand(), a.lessonDropCommand(),
		a.lessonTriageCommand())

	return c
}

func (a *app) lessonAddCommand() *cobra.Command {
	var (
		why, applies, inFile, taskID, file, role string
	)

	c := &cobra.Command{
		Use:   "add [<rule>]",
		Short: "File a lesson (--why, --applies every|role|task|unsure) or --file in the ### rule / Why: / Applies to: format",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var items []lesson.Lesson

			switch {
			case file != "":
				raw, err := readInput(file)
				if err != nil {
					return err
				}

				if items, err = lesson.ParseMarkdown(string(raw)); err != nil {
					return err
				}
			case len(args) == 1:
				ap, err := lesson.ParseApplies(applies)
				if err != nil {
					return err
				}

				if strings.TrimSpace(why) == "" {
					return errors.New("a lesson needs --why")
				}

				items = []lesson.Lesson{{Rule: args[0], Why: why, Applies: ap, InFile: inFile}}
			default:
				return errors.New("give the rule, or --file")
			}

			if role == "" {
				role = by()
			}

			if taskID != "" {
				taskID = task.Normalize(taskID)
			}

			return a.withState(func(s *shift.Shift) error {
				added, err := addLessons(s, items, role, taskID)
				for _, l := range added {
					fmt.Fprintln(a.out, l.ID)
				}

				return err
			})
		},
	}

	c.Flags().StringVar(&why, "why", "", "why it holds (what went wrong without it)")
	c.Flags().StringVar(&applies, "applies", "unsure", "every (every seat), role (this role), task (one task) or unsure")
	c.Flags().StringVar(&inFile, "in-file", "", "the instruction file that says it already, if one does")
	c.Flags().StringVar(&taskID, "task", "", "the task it came from")
	c.Flags().StringVar(&file, "file", "", "lessons in the markdown format (- for stdin)")
	c.Flags().StringVar(&role, "from", "", "the role that learned it (default $ZMINA_ROLE, else hand)")

	return c
}

func readInput(file string) ([]byte, error) {
	if file == "-" {
		return readAllStdin()
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("lessons: %w", err)
	}

	return raw, nil
}

func readAllStdin() ([]byte, error) {
	var b strings.Builder

	buf := make([]byte, 32<<10)

	for {
		n, err := os.Stdin.Read(buf)
		b.Write(buf[:n])

		if err != nil {
			break
		}
	}

	return []byte(b.String()), nil
}

// addLessons scans and stores lessons. One that carries a secret or a customer's data is refused whole — the scan
// names the rule and the line, never the text — and nothing of it is kept. The rest are stored.
func addLessons(s *shift.Shift, items []lesson.Lesson, role, taskID string) ([]lesson.Lesson, error) {
	deny := lessonDeny(s.Crew)

	var (
		added   []lesson.Lesson
		refused []string
	)

	for i, l := range items {
		text := l.Rule + "\n" + l.Why + "\n" + l.InFile
		if f := lesson.Scan(text, deny); len(f) > 0 {
			refused = append(refused, fmt.Sprintf("lesson %d refused: it carries %s (line %d)", i+1, f[0].Rule, f[0].Line))
			s.Log(role, "lesson-reject", taskID, f[0].Rule)

			continue
		}

		l.Role, l.Task = role, taskID

		got, err := lessonStore(s.Crew).Add(l, s.Now())
		if err != nil {
			return added, err
		}

		s.Log(role, "lesson-add", taskID, got.ID+": "+got.Rule)
		added = append(added, got)
	}

	if len(refused) > 0 {
		return added, errors.New(strings.Join(refused, "; ") + "; no secret or customer data goes into a lesson")
	}

	return added, nil
}

// lessonDeny is the crew's own words no lesson may carry (lessons.deny).
func lessonDeny(c *crew.Crew) []string {
	if c.Lessons.Deny == "" {
		return nil
	}

	raw, err := os.ReadFile(c.Lessons.Deny)
	if err != nil {
		return nil
	}

	var out []string

	for _, line := range strings.Split(string(raw), "\n") {
		if w := strings.TrimSpace(line); w != "" && !strings.HasPrefix(w, "#") {
			out = append(out, w)
		}
	}

	return out
}

// lessonsFromCheckpoint stores the lessons a checkpoint or result carries in a section named lessons, in the
// markdown format; empty, none or unknown means none. A bad section is said, the checkpoint is saved anyway.
func lessonsFromCheckpoint(s *shift.Shift, values map[string]string, role, taskID string) error {
	text := strings.TrimSpace(values["lessons"])
	switch strings.ToLower(text) {
	case "", "none", "unknown", "no", "-":
		return nil
	}

	items, err := lesson.ParseMarkdown(text)
	if err != nil {
		return fmt.Errorf("the lessons section: %w", err)
	}

	_, err = addLessons(s, items, role, taskID)

	return err
}

func (a *app) lessonLsCommand() *cobra.Command {
	var all bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "The lessons waiting (--all: every one), the open lessons MR and the last run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				items, err := lessonStore(s.Crew).All()
				if err != nil {
					return err
				}

				for _, l := range items {
					if all || l.State == lesson.New {
						fmt.Fprintf(a.out, "%-6s %-7s %-10s %-12s %s\n", l.ID, l.State, l.Role, l.Applies, l.Rule)
					}
				}

				if line := lessonsLine(s.Crew); line != "" {
					fmt.Fprintln(a.out, line)
				}

				return nil
			})
		},
	}

	c.Flags().BoolVar(&all, "all", false, "every lesson, also those in an MR, merged or dropped")

	return c
}

func (a *app) lessonShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "One lesson whole",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				l, err := lessonStore(s.Crew).Get(args[0])
				if err != nil {
					return err
				}

				fmt.Fprintf(a.out, "%s · %s · from %s %s · %s\nrule: %s\nwhy: %s\napplies to: %s\n", l.ID, l.State, l.Role,
					l.Task, l.At.Local().Format(time.DateTime), l.Rule, l.Why, l.Applies)

				for _, kv := range [][2]string{{"in a file already", l.InFile}, {"verdict", l.Verdict},
					{"reason", l.Reason}, {"mr", l.MR}} {
					if kv[1] != "" {
						fmt.Fprintf(a.out, "%s: %s\n", kv[0], kv[1])
					}
				}

				return nil
			})
		},
	}
}

func (a *app) lessonDropCommand() *cobra.Command {
	var reason string

	c := &cobra.Command{
		Use:   "drop <id...>",
		Short: "Drop lessons still waiting (--reason)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				for _, id := range args {
					l, err := lessonStore(s.Crew).Get(id)
					if err != nil {
						return err
					}

					if l.State != lesson.New {
						return fmt.Errorf("%s is %s, not waiting", l.ID, l.State)
					}

					l.State, l.Reason, l.Updated = lesson.Dropped, or(reason, "dropped by "+by()), s.Now()
					if err := lessonStore(s.Crew).Put(l); err != nil {
						return err
					}

					s.Log(by(), "lesson-drop", l.Task, l.ID+": "+l.Reason)
				}

				return nil
			})
		},
	}

	c.Flags().StringVar(&reason, "reason", "", "why")

	return c
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}

	return v
}

// lessonsLine is what ls says of the lessons: how many wait, the open MR, the last run's trouble. Empty when the
// crew has none and lessons are off.
func lessonsLine(c *crew.Crew) string {
	items, _ := lessonStore(c).All()

	waiting := 0

	for _, l := range items {
		if l.State == lesson.New {
			waiting++
		}
	}

	st, _ := lesson.LoadState(lessonDir(c))

	if !c.Lessons.On && waiting == 0 && st.Open == nil {
		return ""
	}

	parts := []string{fmt.Sprintf("lessons: %d waiting", waiting)}

	switch {
	case st.Open != nil && st.Open.URL != "":
		parts = append(parts, "MR open "+st.Open.URL)
	case st.Open != nil:
		parts = append(parts, "branch "+st.Open.Branch+" open")
	}

	if !c.Lessons.On {
		parts = append(parts, "triage off (lessons: on in the crew file)")
	} else if st.LastError != "" {
		parts = append(parts, "last run failed: "+st.LastError)
	}

	return strings.Join(parts, " · ")
}

func (a *app) lessonTriageCommand() *cobra.Command {
	var dry, scheduled bool

	c := &cobra.Command{
		Use:   "triage",
		Short: "Sort the waiting lessons now and open the draft MR (the ticker does it daily); --dry-run prints the plan",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.lessonTriage(cmd.Context(), dry, scheduled)
		},
	}

	c.Flags().BoolVar(&dry, "dry-run", false, "run the seat and print its plan; no branch is pushed, nothing changes")
	c.Flags().BoolVar(&scheduled, "scheduled", false, "started by the ticker")
	_ = c.Flags().MarkHidden("scheduled")

	return c
}

// lessonRun is what the lock-held start of a run hands to the part that runs outside it.
type lessonRun struct {
	cfg     lesson.Config
	lessons []lesson.Lesson
	model   string
	budget  float64
	dir     string
	crew    *crew.Crew
}

// lessonTriage runs one triage: the start under the lock (is one due, which lessons, mark it running), the seat and
// the git work outside it, then the record under the lock again.
func (a *app) lessonTriage(ctx context.Context, dry, scheduled bool) error {
	var run lessonRun

	err := a.withState(func(s *shift.Shift) error {
		r, err := lessonStart(s, dry, scheduled)
		run = r

		return err
	})
	if err != nil || run.crew == nil {
		return err
	}

	if ctx == nil {
		ctx = context.Background()
	}

	res, terr := lesson.Triage(ctx, run.cfg, run.lessons, lessonSeat(run.model))

	if dry {
		a.printPlan(res)
		_, _ = lesson.Git(run.cfg.Repo, "branch", "-D", res.Branch)

		return a.withState(func(s *shift.Shift) error { return lessonFinish(s, run, res, terr, true) })
	}

	if terr == nil && len(res.Applied) > 0 {
		title := fmt.Sprintf("Lessons %s: %d rules from the %s crew", run.cfg.Now.Format("2006-01-02"), len(res.Applied),
			run.crew.Name)
		desc := fmt.Sprintf("%d lessons as one commit each (its message: the rule, why, from whom). %d dropped, %d "+
			"questions: zmina todo on the crew's cockpit. Drafted by zmina; it never merges.",
			len(res.Applied), len(res.Dropped), len(res.Questions))

		terr = lesson.Push(run.cfg, &res, title, desc)
		if terr == nil && res.MRURL != "" {
			res.Draft, res.DraftVia = lesson.ReadDraft(res.MRURL)
		}
	}

	err = a.withState(func(s *shift.Shift) error { return lessonFinish(s, run, res, terr, false) })
	if terr != nil {
		return terr
	}

	a.printPlan(res)

	return err
}

// lessonStart decides under the lock whether a run goes, and marks it running.
func lessonStart(s *shift.Shift, dry, scheduled bool) (lessonRun, error) {
	c := s.Crew
	dir := lessonDir(c)

	if !c.Lessons.On && !dry {
		return lessonRun{}, errors.New("lessons are off for this crew: lessons: on in its crew file turns the daily triage on")
	}

	st, err := lesson.LoadState(dir)
	if err != nil {
		return lessonRun{}, err
	}

	if !st.Running.IsZero() && s.Now().Sub(st.Running) < lessonsRunStale {
		return lessonRun{}, quiet(scheduled, "a lessons run is already going since "+st.Running.Local().Format(time.TimeOnly))
	}

	if st.Open != nil && !dry {
		return lessonRun{}, quiet(scheduled, "a lessons MR is open ("+or(st.Open.URL, st.Open.Branch)+"): new lessons wait for it")
	}

	items, err := lessonStore(c).All()
	if err != nil {
		return lessonRun{}, err
	}

	var fresh []lesson.Lesson

	for _, l := range items {
		if l.State == lesson.New {
			fresh = append(fresh, l)
		}
	}

	if len(fresh) == 0 {
		return lessonRun{}, quiet(scheduled, "no lessons wait")
	}

	repo := c.Lessons.Repo
	if repo == "" {
		if repo, err = lesson.FindRepo(c.Agents); err != nil {
			return lessonRun{}, err
		}
	}

	branch := c.Lessons.Branch
	if branch == "" {
		branch = lesson.DefaultBranch(repo, "origin")
	}

	// The week's runs stay within week_budget: this run may spend what is left of it, up to its own budget.
	spent := lessonsWeekSpend(c, s.Now())
	budget := min(c.Lessons.RunBudget(), c.Lessons.RunWeekBudget()-spent)

	if budget < 0.5 {
		s.Log("ticker", "triage-skipped", "", fmt.Sprintf("week budget spent: $%.2f of $%.2f", spent, c.Lessons.RunWeekBudget()))

		return lessonRun{}, quiet(scheduled, fmt.Sprintf("this week's lessons budget is spent ($%.2f of $%.2f)", spent,
			c.Lessons.RunWeekBudget()))
	}

	st.Running = s.Now()
	if err := lesson.SaveState(dir, st); err != nil {
		return lessonRun{}, err
	}

	s.Log("ticker", "triage", "", fmt.Sprintf("%d lessons, %s on %s", len(fresh), c.Lessons.RunModel(), filepath.Base(repo)))

	return lessonRun{
		cfg: lesson.Config{Repo: repo, Branch: branch, Agents: c.Agents, Roles: c.Names(), Work: filepath.Join(dir, "work"),
			Deny: lessonDeny(c), Budget: budget, Now: s.Now()},
		lessons: fresh, model: c.Lessons.RunModel(), budget: budget, dir: dir, crew: c,
	}, nil
}

// lessonsWeekSpend is what the crew's lessons runs cost since Monday (the crew's clock), from the log.
func lessonsWeekSpend(c *crew.Crew, now time.Time) float64 {
	local := now.In(c.Location())
	monday := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).
		AddDate(0, 0, -(int(local.Weekday())+6)%7)

	evs, _ := eventLog(c).Read(events.Filter{Since: monday})

	var sum float64

	for _, e := range evs {
		if e.Event == "lessons-cost" {
			v, err := strconv.ParseFloat(strings.TrimPrefix(strings.Fields(e.Detail + " ")[0], "$"), 64)
			if err == nil {
				sum += v
			}
		}
	}

	return sum
}

// quiet is a reason a run does not go: an error by hand, nothing for the ticker.
func quiet(scheduled bool, why string) error {
	if scheduled {
		return nil
	}

	return errors.New(why)
}

// lessonFinish records a run under the lock: the lessons' states, the open MR, the owner's todo, the cost.
func lessonFinish(s *shift.Shift, run lessonRun, res lesson.Result, runErr error, dry bool) error {
	st, err := lesson.LoadState(run.dir)
	if err != nil {
		return err
	}

	st.Running, st.LastRun, st.LastCost = time.Time{}, s.Now(), res.Cost
	st.LastError = ""

	// Every run's cost goes in the log, a failed one too: zmina usage sums it as the lessons triage.
	if res.Cost > 0 {
		s.Log("ticker", "lessons-cost", "", fmt.Sprintf("$%.2f", res.Cost))
	}

	if runErr != nil {
		st.LastError = runErr.Error()
		s.Log("ticker", "triage-failed", "", runErr.Error())
		s.AddNotes([]string{fmt.Sprintf("lessons triage failed: %v", runErr)})

		return lesson.SaveState(run.dir, st)
	}

	if dry {
		return lesson.SaveState(run.dir, st)
	}

	store := lessonStore(s.Crew)
	mr := or(res.MRURL, res.Branch)

	var ids []string

	mark := func(id string, state lesson.State, verdict, reason string) {
		l, err := store.Get(id)
		if err != nil {
			return
		}

		l.State, l.Verdict, l.Reason, l.Updated = state, verdict, reason, s.Now()
		if state == lesson.InMR {
			l.MR = mr
		}

		_ = store.Put(l)
	}

	for _, ap := range res.Applied {
		mark(ap.ID, lesson.InMR, "applied", ap.File)
		ids = append(ids, ap.ID)
	}

	for _, d := range res.Dropped {
		mark(d.ID, lesson.Dropped, d.Verdict, d.Reason)
	}

	// A lesson the run asked the owner about rides with the MR (or with the todo when nothing was committed).
	for _, l := range run.lessons {
		if q := questionFor(res.Questions, l.ID); q != "" {
			if res.Pushed {
				mark(l.ID, lesson.InMR, "question", q)
				ids = append(ids, l.ID)
			} else {
				mark(l.ID, lesson.Dropped, "question", q)
			}
		}
	}

	text := lessonTodoText(res, run, 0)

	switch {
	case res.Pushed:
		ask, err := askStore(s.Crew).Add(asks.Ask{Kind: asks.Check, Title: "Lessons MR to review",
			Text: text, Role: "ticker", By: "ticker"}, s.Now())
		if err != nil {
			return err
		}

		st.Open = &lesson.Open{Branch: res.Branch, Target: run.cfg.Branch, URL: res.MRURL, Base: res.Base, Tip: res.Tip,
			IDs: ids, Todo: ask.ID, Opened: s.Now()}
		s.Log("ticker", "lessons-mr", "", or(res.MRURL, res.Branch)+" "+ask.ID)
		s.Log("ticker", "lessons-draft", "", lessonDraftLine(res, run.cfg.Remote))
	case len(res.Questions) > 0:
		ask, err := askStore(s.Crew).Add(asks.Ask{Kind: asks.Check, Title: "Lessons: questions",
			Text: text, Role: "ticker", By: "ticker"}, s.Now())
		if err == nil {
			s.Log("ticker", "lessons-asked", "", ask.ID)
		}
	}

	return lesson.SaveState(run.dir, st)
}

func questionFor(questions []string, id string) string {
	for _, q := range questions {
		if strings.HasPrefix(q, id+" ") {
			return q
		}
	}

	return ""
}

// lessonDraftLine says what the push left on the forge: the MR read back as a draft or not, or the pushed ref when
// nothing on this node could read the MR.
func lessonDraftLine(res lesson.Result, remote string) string {
	ref := or(remote, "origin") + " refs/heads/" + res.Branch

	switch {
	case res.MRURL == "":
		return "pushed " + ref + " (no MR url in the push reply)"
	case res.Draft == "yes":
		return "draft " + res.MRURL + " (read by " + res.DraftVia + ")"
	case res.Draft == "no":
		return "NOT A DRAFT " + res.MRURL + " (read by " + res.DraftVia + "): its title starts with Draft:, check it"
	}

	return "pushed " + ref + "; " + res.MRURL + " not read back (no glab or gl profile for its host)"
}

// lessonTodoText is the owner's todo for a run: the MR, the lessons in it, what was dropped, the questions, and how
// many new lessons wait for it.
func lessonTodoText(res lesson.Result, run lessonRun, waiting int) string {
	var b strings.Builder

	if res.Pushed {
		fmt.Fprintf(&b, "Draft MR %s on %s: %d lessons, one commit each. Review and merge it in GitLab (zmina never "+
			"merges); mark this done if you close it instead.", or(res.MRURL, res.Branch), filepath.Base(run.cfg.Repo),
			len(res.Applied))

		if res.PushNote != "" {
			b.WriteString(" " + res.PushNote + ".")
		}
	} else {
		b.WriteString("The lessons run committed nothing; it has questions for you.")
	}

	rule := map[string]string{}
	for _, l := range run.lessons {
		rule[l.ID] = l.Rule
	}

	for _, ap := range res.Applied {
		fmt.Fprintf(&b, "\n- %s -> %s: %s", ap.ID, ap.File, rule[ap.ID])
	}

	for _, d := range res.Dropped {
		fmt.Fprintf(&b, "\n- %s dropped (%s): %s", d.ID, d.Verdict, d.Reason)
	}

	for _, q := range res.Questions {
		b.WriteString("\nQuestion: " + q)
	}

	if waiting > 0 {
		fmt.Fprintf(&b, "\n%d new lessons wait for this MR.", waiting)
	}

	return b.String()
}

func (a *app) printPlan(res lesson.Result) {
	for _, ap := range res.Applied {
		fmt.Fprintf(a.out, "%s -> %s (%s)\n", ap.ID, ap.File, shortSHA(ap.Commit))
	}

	for _, d := range res.Dropped {
		fmt.Fprintf(a.out, "%s %s: %s\n", d.ID, d.Verdict, d.Reason)
	}

	for _, q := range res.Questions {
		fmt.Fprintln(a.out, "question:", q)
	}

	switch {
	case res.MRURL != "":
		fmt.Fprintln(a.out, "draft MR:", res.MRURL)
	case res.Pushed:
		fmt.Fprintln(a.out, "pushed", res.Branch+";", res.PushNote)
	}

	fmt.Fprintf(a.out, "cost $%.2f\n", res.Cost)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}

	return s
}

// lessonsBeat is the ticker's part: follow the open lessons MR (hourly) and start the day's run when one is due. The
// run goes in a process of its own, so a beat never waits for the seat.
func (a *app) lessonsBeat(now time.Time) {
	var (
		c       *crew.Crew
		st      lesson.RunState
		due     bool
		waiting int
	)

	_ = a.readState(func(s *shift.Shift) error {
		c = s.Crew
		st, _ = lesson.LoadState(lessonDir(c))
		items, _ := lessonStore(c).All()

		for _, l := range items {
			if l.State == lesson.New {
				waiting++
			}
		}

		due = lessonsDue(c, st, waiting, now)

		return nil
	})

	if c == nil || !c.Lessons.On {
		return
	}

	if st.Open != nil {
		a.lessonsFollow(c, st, waiting, now)

		return
	}

	if due {
		a.spawnLessonRun(c)
	}
}

// lessonsDue reports whether the day's run should start: on, past its time today, not run today, lessons waiting,
// no MR open, no run going.
func lessonsDue(c *crew.Crew, st lesson.RunState, waiting int, now time.Time) bool {
	if !c.Lessons.On || waiting == 0 || st.Open != nil {
		return false
	}

	if !st.Running.IsZero() && now.Sub(st.Running) < lessonsRunStale {
		return false
	}

	local := now.In(c.Location())
	h, m := c.Lessons.RunAt()
	at := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, local.Location())

	if local.Before(at) {
		return false
	}

	last := st.LastRun.In(c.Location())

	return st.LastRun.IsZero() || last.Year() != local.Year() || last.YearDay() != local.YearDay()
}

// lessonsFollow looks at the open MR: the owner marked its todo done, or it is merged or gone on the remote. Then
// its lessons are settled, the todo is done, and the roles' checkout is brought up to it (or the owner gets the one
// command). While it is open, its todo tells how many new lessons wait.
func (a *app) lessonsFollow(c *crew.Crew, st lesson.RunState, waiting int, now time.Time) {
	open := st.Open

	ownerDone := false
	if ask, err := askStore(c).Get(open.Todo); err == nil && !ask.Open() {
		ownerDone = true
	}

	done, merged := ownerDone, false

	if !ownerDone && now.Sub(st.Checked) >= lessonsCheckEvery {
		repo := c.Lessons.Repo
		if repo == "" {
			repo, _ = lesson.FindRepo(c.Agents)
		}

		if repo != "" {
			if d, m, err := lesson.Finished(repo, "origin", open.Branch, open.Target, open.Tip); err == nil {
				done, merged = d, m
			}
		}

		st.Checked = now
	}

	var catchupCmd string

	if done && merged {
		repo := c.Lessons.Repo
		if repo == "" {
			repo, _ = lesson.FindRepo(c.Agents)
		}

		if ok, cmdLine := lesson.Catchup(repo, "origin", open.Target); !ok {
			catchupCmd = cmdLine
		}
	}

	_ = a.withState(func(s *shift.Shift) error {
		cur, err := lesson.LoadState(lessonDir(c))
		if err != nil || cur.Open == nil || cur.Open.Branch != open.Branch {
			return err
		}

		cur.Checked = st.Checked

		if !done {
			if waiting != cur.Open.Waiting {
				if ask, err := askStore(s.Crew).Get(cur.Open.Todo); err == nil && ask.Open() {
					ask.Text = withWaiting(ask.Text, waiting)
					ask.Updated = s.Now()
					_ = askStore(s.Crew).Put(ask)
				}

				cur.Open.Waiting = waiting
			}

			return lesson.SaveState(lessonDir(c), cur)
		}

		store := lessonStore(s.Crew)

		for _, id := range cur.Open.IDs {
			l, err := store.Get(id)
			if err != nil || l.State != lesson.InMR {
				continue
			}

			l.State, l.Updated = lesson.Dropped, s.Now()
			if merged && l.Verdict == "applied" {
				l.State = lesson.Merged
			} else if !merged {
				l.Reason = "the MR was closed without a merge"
			}

			_ = store.Put(l)
		}

		what := "closed"
		if merged {
			what = "merged"
		}

		if ask, err := askStore(s.Crew).Get(cur.Open.Todo); err == nil && ask.Open() {
			_, _ = askStore(s.Crew).Close(ask.ID, asks.Done, "the MR was "+what, s.Now())
		}

		if catchupCmd != "" {
			_, _ = askStore(s.Crew).Add(asks.Ask{Kind: asks.Check, Title: "Pull the merged lessons",
				Text: "The lessons MR is merged, but the checkout the roles read is ahead, dirty or on another branch, " +
					"so zmina left it alone. Bring it up with: " + catchupCmd, Role: "ticker", By: "ticker"}, s.Now())
		}

		s.Log("ticker", "lessons-"+what, "", or(cur.Open.URL, cur.Open.Branch))
		cur.Open, cur.Catchup = nil, catchupCmd

		return lesson.SaveState(lessonDir(c), cur)
	})
}

// withWaiting rewrites the todo's last line about waiting lessons.
func withWaiting(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.HasSuffix(lines[len(lines)-1], "new lessons wait for this MR.") {
		lines = lines[:len(lines)-1]
	}

	if n > 0 {
		lines = append(lines, fmt.Sprintf("%d new lessons wait for this MR.", n))
	}

	return strings.Join(lines, "\n")
}

// spawnLessonRun starts `zmina lesson triage --scheduled` for the crew in a process of its own, its output in the
// lessons dir.
func (a *app) spawnLessonRun(c *crew.Crew) {
	exe, err := os.Executable()
	if err != nil {
		return
	}

	logf, err := os.OpenFile(filepath.Join(lessonDir(c), "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		if os.MkdirAll(lessonDir(c), 0o700) != nil {
			return
		}

		if logf, err = os.OpenFile(filepath.Join(lessonDir(c), "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
			return
		}
	}

	defer func() { _ = logf.Close() }()

	cmd := exec.Command(exe, "--crew", c.Path, "lesson", "triage", "--scheduled")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
	}
}

// lessonsDoctor checks a crew with lessons on: its agentic-os found, a default branch, a push that would go (a dry
// run over the node's own credentials), claude on PATH, the last run. Each is a level and a line.
func lessonsDoctor(c *crew.Crew) [][2]string {
	if !c.Lessons.On {
		return nil
	}

	repo := c.Lessons.Repo
	if repo == "" {
		r, err := lesson.FindRepo(c.Agents)
		if err != nil {
			return [][2]string{{"fail", err.Error() + "; set lessons.repo or turn lessons off"}}
		}

		repo = r
	}

	branch := c.Lessons.Branch
	if branch == "" {
		branch = lesson.DefaultBranch(repo, "origin")
	}

	out := [][2]string{{"ok", fmt.Sprintf("on · %s, MRs into %s · %s, $%.0f a run, $%.0f a week", repo, branch,
		c.Lessons.RunModel(), c.Lessons.RunBudget(), c.Lessons.RunWeekBudget())}}

	if _, err := lesson.Git(repo, "push", "--dry-run", "--no-verify", "origin",
		"HEAD:refs/heads/"+lesson.BranchPrefix+"doctor-probe"); err != nil {
		out = append(out, [2]string{"fail", "a push to origin would not go: " + err.Error()})
	}

	if _, err := exec.LookPath("claude"); err != nil {
		out = append(out, [2]string{"fail", "claude is not on PATH: the sorting seat cannot run"})
	}

	if ahead, err := lesson.Git(repo, "rev-list", "--count", "origin/"+branch+"..HEAD"); err == nil && ahead != "0" {
		out = append(out, [2]string{"warn", fmt.Sprintf("the checkout is %s commits ahead of origin/%s: the roles read "+
			"rules a lessons MR does not see, and a merged MR is not pulled in by itself", ahead, branch)})
	}

	if st, err := lesson.LoadState(lessonDir(c)); err == nil && st.LastError != "" {
		out = append(out, [2]string{"warn", "the last run failed: " + st.LastError})
	}

	return out
}
