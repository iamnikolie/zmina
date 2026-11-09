package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/cost"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// pricesPath is the override file of the price table.
func pricesPath() string { return crew.ExpandHome("~/.zmina/prices.json") }

// outsideRole is the row of sessions that are no crew's role but ran in a task's worktree: the owner's own, a
// headless claude -p a role started there.
const outsideRole = "(outside)"

// usageReport is what usage --json prints.
type usageReport struct {
	By      string     `json:"by"`
	Since   time.Time  `json:"since"`
	Rows    []cost.Row `json:"rows"`
	Total   cost.Row   `json:"total"`
	Prices  string     `json:"prices"`
	Missing []string   `json:"missing,omitempty"`
	// Restarts are the roles' restarts for their context in the window (restart_at, restart_idle).
	Restarts []usageRestart `json:"restarts,omitempty"`
	// Lessons are the lessons triage runs in the window and their cost.
	Lessons []usageLesson `json:"lessons,omitempty"`
	// Fills are what the tools put into the roles' context, by tool kind, with --by tool.
	Fills []registry.Fill `json:"fills,omitempty"`
}

// peerCells is what a peer answers usage --cells with.
type peerCells struct {
	Cells    []cost.Cell    `json:"cells"`
	Problems []string       `json:"problems,omitempty"`
	Restarts []usageRestart `json:"restarts,omitempty"`
	Lessons  []usageLesson  `json:"lessons,omitempty"`
	Fills    []usageFill    `json:"fills,omitempty"`
}

// peerUsageTimeout is how long --all waits for one peer's answer.
const peerUsageTimeout = 90 * time.Second

func (a *app) usageCommand() *cobra.Command {
	var (
		since, by, role string
		asJSON, all     bool
		local, cells    bool
	)

	c := &cobra.Command{
		Use: "usage",
		Short: "What the work cost (--since 24h, --by role|task|model|day|crew|trigger|how|ctx|tool, --all): turns, tokens, " +
			"API-equivalent dollars and the model mix, from the transcripts",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := parseAge(since)
			if err != nil {
				return err
			}

			if !slices.Contains(cost.Views, by) && by != toolView {
				return fmt.Errorf("--by %q: one of %s, %s", by, strings.Join(cost.Views, ", "), toolView)
			}

			prices, err := cost.Load(pricesPath())
			if err != nil {
				return err
			}

			from := time.Now().Add(-d)

			crews := []uiCrew{{Key: a.crewKey(), Path: a.crewPath()}}
			if all {
				crews = uiCrews(a.extraCrew())
			}

			node := ""
			if me, err := openNode(); err == nil {
				node = me.Name
			}

			got, fills, problems := usageRead(crews, node, from, prices, by == toolView)
			restarts := usageRestarts(crews, node, from)
			lessons := usageLessons(crews, node, from)

			if cells {
				return json.NewEncoder(a.out).Encode(peerCells{Cells: got, Problems: problems, Restarts: restarts,
					Lessons: lessons, Fills: fills})
			}

			if all && !local {
				// The parsed duration goes into the peer's command line, never the flag's text.
				peers := peersUsage(cmd.Context(), d.String(), by == toolView)
				got = append(got, peers.Cells...)
				problems = append(problems, peers.Problems...)
				restarts = append(restarts, peers.Restarts...)
				lessons = append(lessons, peers.Lessons...)
				fills = append(fills, peers.Fills...)
			}

			if role != "" {
				got = slices.DeleteFunc(got, func(c cost.Cell) bool { return c.Role != role })
				restarts = slices.DeleteFunc(restarts, func(r usageRestart) bool { return r.Role != role })
				fills = slices.DeleteFunc(fills, func(f usageFill) bool { return f.Role != role })
			}

			if by == toolView {
				rows := groupFills(fills)
				if asJSON {
					return a.printJSON(usageReport{By: by, Since: from, Rows: []cost.Row{}, Total: cost.Total(got),
						Prices: prices.Date + " (" + prices.From + ")", Missing: problems, Fills: rows})
				}

				return printFills(a.out, rows, problems)
			}

			rows := cost.Group(got, by)
			total := cost.Total(got)

			if asJSON {
				if rows == nil {
					rows = []cost.Row{}
				}

				return a.printJSON(usageReport{By: by, Since: from, Rows: rows, Total: total,
					Prices: prices.Date + " (" + prices.From + ")", Missing: problems, Restarts: restarts, Lessons: lessons})
			}

			if err := printUsage(a.out, by, rows, total, len(crewsOf(got)) > 1); err != nil {
				return err
			}

			printRestarts(a.out, restarts, len(crewsOf(got)) > 1)
			printLessons(a.out, lessons)

			printUsageNotes(a.out, prices, total, problems)

			return nil
		},
	}

	c.Flags().StringVar(&since, "since", "24h", "how far back: 90m, 12h, 7d")
	c.Flags().StringVar(&by, "by", "role", "rows by: "+strings.Join(cost.Views, ", ")+", "+toolView+
		" (what the tools put into the context)")
	c.Flags().StringVar(&role, "role", "", "only this role's turns")
	c.Flags().BoolVar(&all, "all", false, "every crew under ~/.zmina, and through the mesh every peer's crews")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVar(&local, "local", false, "with --all: this machine's crews only, no peers")
	c.Flags().BoolVar(&cells, "cells", false, "print the priced cells as JSON (what a peer answers --all with)")
	_ = c.Flags().MarkHidden("cells")

	return c
}

func crewsOf(cells []cost.Cell) map[string]bool {
	out := map[string]bool{}
	for _, c := range cells {
		out[c.Node+"/"+c.Crew] = true
	}

	return out
}

// usageCells reads, prices and attributes the crews' sessions since a time. A crew that cannot be read is a
// problem, not a failure: the others still count.
func usageCells(crews []uiCrew, node string, since time.Time, prices cost.Table) ([]cost.Cell, []string) {
	cells, _, problems := usageRead(crews, node, since, prices, false)

	return cells, problems
}

// usageFill is what one tool kind put into one role's context (usage --by tool).
type usageFill struct {
	Node string `json:"node,omitempty"`
	Crew string `json:"crew,omitempty"`
	Role string `json:"role"`
	registry.Fill
}

// usageRead is usageCells, and with fills what the tools put into the roles' context.
func usageRead(crews []uiCrew, node string, since time.Time, prices cost.Table, fills bool) ([]cost.Cell, []usageFill,
	[]string) {
	var (
		filled   []usageFill
		problems []string
		loaded   []*crew.Crew
		keys     []string
	)

	for _, uc := range crews {
		c, err := crew.Load(uc.Path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("crew %s: %v", uc.Key, err))

			continue
		}

		loaded = append(loaded, c)
		keys = append(keys, uc.Key)
	}

	// Every crew's session names, so one crew's role is never another's outside session.
	roleOf := map[string]bool{}
	for _, c := range loaded {
		for s := range crewSessions(c) {
			roleOf[s] = true
		}
	}

	transcripts := map[string][]registry.Transcript{}
	ledgers := map[string]cost.Ledger{}

	var sessions []cost.Session

	for i, c := range loaded {
		key := keys[i]

		ts, ok := transcripts[c.Namespace]
		if !ok {
			var err error
			if ts, err = (&registry.Registry{Namespace: c.Namespace}).Transcripts(since); err != nil {
				problems = append(problems, fmt.Sprintf("crew %s: %v", key, err))
			}

			transcripts[c.Namespace] = ts
		}

		tasks, err := taskStore(c).All()
		if err != nil {
			problems = append(problems, fmt.Sprintf("crew %s: tasks: %v", key, err))
		}

		evs, _ := eventLog(c).Read(events.Filter{})
		ledgers[key] = cost.NewLedger(tasks, evs)
		us, usageErr := (&codex.Registry{Dir: filepath.Join(c.StateDir(), "codex")}).UsageSessions(since)
		if usageErr != nil {
			problems = append(problems, fmt.Sprintf("crew %s: Codex usage: %v", key, usageErr))
		}
		for _, u := range us {
			if u.Message.UnpricedReason != "" {
				problem := fmt.Sprintf("crew %s: Codex role %s: %s", key, u.Role, u.Message.UnpricedReason)
				if !slices.Contains(problems, problem) {
					problems = append(problems, problem)
				}
			}
			sessions = append(sessions, cost.Session{Node: node, Crew: key, Role: u.Role, Group: u.Group, Dispatcher: dispatcherSession(c), Messages: []registry.Message{u.Message}})
		}

		names := crewSessions(c)
		outside := worktreeDirs(c, tasks)

		for _, t := range ts {
			who, mine := names[t.Name]
			if !mine {
				if roleOf[t.Name] || !outside[filepath.Dir(t.Path)] {
					continue
				}

				who = [2]string{outsideRole, outsideRole}
			}

			ms, err := registry.Messages(t, since)
			if err != nil {
				problems = append(problems, fmt.Sprintf("crew %s: %s: %v", key, t.ID, err))

				continue
			}

			if fills {
				fs, err := registry.Fills(t, since)
				if err != nil {
					problems = append(problems, fmt.Sprintf("crew %s: %s: %v", key, t.ID, err))
				}

				for _, f := range fs {
					filled = append(filled, usageFill{Node: node, Crew: key, Role: who[1], Fill: f})
				}
			}

			sessions = append(sessions, cost.Session{Node: node, Crew: key, Role: who[0], Group: who[1],
				Dispatcher: dispatcherSession(c), Messages: ms})
		}
	}

	return cost.Cells(sessions, ledgers, prices, time.Local), filled, problems
}

func dispatcherSession(c *crew.Crew) string {
	if r := c.Roles[c.Dispatcher]; r != nil {
		return r.Session
	}

	return ""
}

// crewSessions maps the crew's session names to the role or instance each runs and the row it is counted under:
// zm-dev to dev, zm-dev-2 to dev#2 under dev.
func crewSessions(c *crew.Crew) map[string][2]string {
	out := map[string][2]string{}

	for _, name := range c.Names() {
		r := c.Roles[name]
		out[r.Session] = [2]string{name, name}

		for n := 2; n <= crew.MaxInstance; n++ {
			out[r.Session+"-"+strconv.Itoa(n)] = [2]string{name + "#" + strconv.Itoa(n), name}
		}
	}

	return out
}

// worktreeDirs are the transcript directories of sessions started in one of the crew's task worktrees.
func worktreeDirs(c *crew.Crew, tasks []task.Task) map[string]bool {
	reg := &registry.Registry{Namespace: c.Namespace}
	out := map[string]bool{}

	for _, t := range tasks {
		if t.Worktree != "" {
			out[reg.ProjectDir(filepath.Clean(t.Worktree))] = true
		}
	}

	return out
}

// peersUsage asks every peer with an address for its crews' cells, all at once; a peer that does not answer is
// missing, with why.
func peersUsage(ctx context.Context, since string, fills bool) peerCells {
	me, err := openNode()
	if err != nil {
		return peerCells{}
	}

	peers, err := me.Peers()
	if err != nil {
		return peerCells{Problems: []string{"mesh: " + err.Error()}}
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		out      []cost.Cell
		missing  []string
		restarts []usageRestart
		lessons  []usageLesson
		filled   []usageFill
	)

	for _, p := range peers {
		if len(p.Addrs) == 0 {
			mu.Lock()
			missing = append(missing, p.Name+": no address of its own")
			mu.Unlock()

			continue
		}

		wg.Add(1)

		go func(p mesh.Peer) {
			defer wg.Done()

			cells, err := peerUsage(ctx, p, since, fills)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				missing = append(missing, p.Name+": "+err.Error())

				return
			}

			out = append(out, cells.Cells...)
			restarts = append(restarts, cells.Restarts...)
			lessons = append(lessons, cells.Lessons...)
			filled = append(filled, cells.Fills...)
			for _, pr := range cells.Problems {
				missing = append(missing, p.Name+": "+pr)
			}
		}(p)
	}

	wg.Wait()
	slices.Sort(missing)

	return peerCells{Cells: out, Problems: missing, Restarts: restarts, Lessons: lessons, Fills: filled}
}

func peerUsage(ctx context.Context, p mesh.Peer, since string, fills bool) (peerCells, error) {
	ctx, cancel := context.WithTimeout(ctx, peerUsageTimeout)
	defer cancel()

	var errs []error

	for _, addr := range p.Addrs {
		args := []string{"usage", "--all", "--local", "--cells", "--since", since}
		if fills {
			args = append(args, "--by", toolView)
		}

		raw, err := runPeer(ctx, addr, nil, args...)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		var pc peerCells
		if err := json.Unmarshal(raw, &pc); err != nil {
			return pc, fmt.Errorf("its zmina does not answer usage --cells (older than this one?): %w", err)
		}

		for i := range pc.Cells {
			if pc.Cells[i].Node == "" {
				pc.Cells[i].Node = p.Name
			}
		}

		for i := range pc.Restarts {
			if pc.Restarts[i].Node == "" {
				pc.Restarts[i].Node = p.Name
			}
		}

		for i := range pc.Fills {
			if pc.Fills[i].Node == "" {
				pc.Fills[i].Node = p.Name
			}
		}

		for i := range pc.Lessons {
			if pc.Lessons[i].Node == "" {
				pc.Lessons[i].Node = p.Name
			}
		}

		return pc, nil
	}

	return peerCells{}, firstLine(errors.Join(errs...))
}

func firstLine(err error) error {
	if err == nil {
		return errors.New("no answer")
	}

	line, _, _ := strings.Cut(err.Error(), "\n")

	return errors.New(line)
}

// usageHead names the key column of a view.
var usageHead = map[string]string{"role": "ROLE", "task": "TASK", "model": "MODEL", "day": "DAY", "crew": "CREW",
	"trigger": "TRIGGER", "how": "HOW", "ctx": "BAND"}

// contextFlag is the context a role's turns carry on average past which the table marks it: most of such a
// turn's price is reading the conversation again.
const contextFlag = 200_000

func printUsage(out io.Writer, by string, rows []cost.Row, total cost.Row, manyCrews bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	crewCol := manyCrews && (by == "role" || by == "task")

	head := []string{usageHead[by]}
	if crewCol {
		head = append(head, "CREW")
	}

	head = append(head, "TURNS")
	if by == "ctx" {
		head = append(head, "%TURNS")
	}

	head = append(head, "SUB", "WAITS", "TOKENS", "CACHED", "REREAD", "$ API")
	if by == "ctx" {
		head = append(head, "%COST")
	}

	head = append(head, "$/TURN", "CTX", "MODELS")
	if by == "task" {
		head = append(head, "HOW")
	}

	fmt.Fprintln(w, strings.Join(head, "\t"))

	line := func(r cost.Row, crewName string) {
		cols := []string{r.Key}
		if crewCol {
			cols = append(cols, crewName)
		}

		ctx := r.Input + r.Write + r.Write5m + r.Write1h + r.Read
		perTurn := ctx / max(r.Turns, 1)

		mark := ""
		if by == "role" && perTurn > contextFlag {
			mark = " !"
		}

		cached := 0.0
		if ctx > 0 {
			cached = float64(r.Read) / float64(ctx)
		}

		waits := "0"
		if r.Waits > 0 {
			waits = strconv.Itoa(r.Waits) + " " + dollars(r.WaitCost)
		}

		cols = append(cols, strconv.Itoa(r.Turns))
		if by == "ctx" {
			cols = append(cols, share(float64(r.Turns), float64(total.Turns)))
		}

		cols = append(cols, strconv.Itoa(r.Sub), waits, shift.Tokens(r.Tokens()), fmt.Sprintf("%.0f%%", 100*cached),
			reread(r.Cell), dollars(r.Cost))
		if by == "ctx" {
			cols = append(cols, share(r.Cost, total.Cost))
		}

		cols = append(cols, dollars(r.Cost/float64(max(r.Turns-r.Unpriced, 1))), shift.Tokens(perTurn)+mark, r.Mix())

		if by == "task" {
			cols = append(cols, r.HowMix())
		}

		fmt.Fprintln(w, strings.Join(cols, "\t"))
	}

	for _, r := range rows {
		line(r, crewLabel(r.Node, r.Crew))
	}

	line(total, "")

	return w.Flush()
}

// share is part of whole in percent, "-" of nothing.
func share(part, whole float64) string {
	if whole <= 0 {
		return "-"
	}

	return fmt.Sprintf("%.0f%%", 100*part/whole)
}

// reread is how many times a token written to the cache was read from it: each turn reads the whole context
// again, so a long conversation reads what it wrote once many times over. "-" without cache writes.
func reread(c cost.Cell) string {
	w := c.Write + c.Write5m + c.Write1h
	if w == 0 {
		return "-"
	}

	return fmt.Sprintf("%.0fx", float64(c.Read)/float64(w))
}

func crewLabel(node, c string) string {
	if node == "" {
		return c
	}

	return node + "/" + c
}

func dollars(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("$%.0f", v)
	case v >= 0.995 || v == 0:
		return fmt.Sprintf("$%.2f", v)
	default:
		return fmt.Sprintf("$%.3f", v)
	}
}

// staleAfter is how old the price table may get before usage says so.
const staleAfter = 120 * 24 * time.Hour

func printUsageNotes(out io.Writer, prices cost.Table, total cost.Row, problems []string) {
	fmt.Fprintf(out, "\n$ API is API-equivalent: what these tokens would cost at API rates — the crews run on a "+
		"subscription. Prices of %s (%s).\n", prices.Date, prices.From)

	if t := total.Cost; t > 0 {
		in := max(t-total.WriteCost-total.ReadCost-total.OutputCost, 0)
		fmt.Fprintf(out, "Of the cost: cache reads %s, cache writes %s, output %s, input %s.\n", share(total.ReadCost, t),
			share(total.WriteCost, t), share(total.OutputCost, t), share(in, t))
	}

	if age := prices.Age(time.Now()); age > staleAfter {
		fmt.Fprintf(out, "The prices are %d days old: check them, newer ones go in %s.\n", int(age.Hours()/24),
			pricesPath())
	}

	if total.Unpriced > 0 {
		fmt.Fprintf(out, "%d turns unpriced, models %s: add them to %s.\n", total.Unpriced,
			strings.Join(total.Unknowns, ", "), pricesPath())
	}

	for _, p := range problems {
		fmt.Fprintln(out, "missing:", p)
	}
}

// closedCost prints a closed task's cost line, nothing when no turn is found for it.
func (a *app) closedCost(t task.Task) {
	_ = a.readState(func(s *shift.Shift) error {
		if line := a.taskCost(t, s); !strings.HasSuffix(line, noTurns) {
			fmt.Fprintln(a.out, line)
		}

		return nil
	})
}

const noTurns = "no turns of its own found"

// taskCost is one task's cost line: its own turns, and the ones it shares with other tasks held at the same time.
func (a *app) taskCost(t task.Task, s *shift.Shift) string {
	prices, err := cost.Load(pricesPath())
	if err != nil {
		return "cost: " + err.Error()
	}

	cells, _ := usageCells([]uiCrew{{Key: a.crewKey(), Path: s.Crew.Path}}, "", t.Created.Add(-time.Minute), prices)

	var own, shared []cost.Cell

	for _, c := range cells {
		switch {
		case c.Task == t.ID:
			own = append(own, c)
		case c.How == cost.Shared && slices.Contains(strings.Split(strings.TrimPrefix(c.Task, cost.Shared+" "), "+"), t.ID):
			shared = append(shared, c)
		}
	}

	r := cost.Total(own)
	if r.Turns == 0 && len(shared) == 0 {
		return "cost: " + noTurns
	}

	line := fmt.Sprintf("cost: %s API-equivalent · %d turns", dollars(r.Cost), r.Turns)
	if r.Turns > 0 {
		line += " · " + r.Mix() + " · by " + r.HowMix()
	}

	if sh := cost.Total(shared); sh.Turns > 0 {
		line += fmt.Sprintf(" · and %s in %d turns shared with other tasks held at once", dollars(sh.Cost), sh.Turns)
	}

	if r.Unpriced > 0 {
		line += fmt.Sprintf(" · %d turns unpriced", r.Unpriced)
	}

	if t.Budget > 0 {
		state := "within"
		if r.Cost > t.Budget {
			state = "over"
		}

		line += fmt.Sprintf(" · %s its %s budget", state, dollars(t.Budget))
	}

	return line
}

// toolView is usage --by tool: not a cost view, the tools' results that fill the context.
const toolView = "tool"

// groupFills sums the fills by tool kind across roles and crews, the most characters first.
func groupFills(fills []usageFill) []registry.Fill {
	by := map[string]*registry.Fill{}

	for _, f := range fills {
		r := by[f.Tool]
		if r == nil {
			r = &registry.Fill{Tool: f.Tool}
			by[f.Tool] = r
		}

		r.Add(f.Fill)
	}

	out := make([]registry.Fill, 0, len(by))
	for _, r := range by {
		out = append(out, *r)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Chars != out[j].Chars {
			return out[i].Chars > out[j].Chars
		}

		return out[i].Tool < out[j].Tool
	})

	return out
}

// printFills is the --by tool table: what each kind of tool call put into the context, which every later turn reads
// again until a compaction or a restart.
func printFills(out io.Writer, rows []registry.Fill, problems []string) error {
	var total registry.Fill
	for _, r := range rows {
		total.Add(r)
	}

	total.Tool = "all"

	// The long tail in one row (--json has every tool): under 1% of the characters, no big result, no image.
	var (
		shown []registry.Fill
		rest  registry.Fill
		n     int
	)

	for _, r := range rows {
		if 100*r.Chars < total.Chars && r.Big == 0 && r.Images == 0 {
			rest.Add(r)
			n++

			continue
		}

		shown = append(shown, r)
	}

	if n > 0 {
		rest.Tool = fmt.Sprintf("%d more under 1%%", n)
		shown = append(shown, rest)
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TOOL\tCALLS\tCHARS\t%CHARS\tOVER 20K\tIMAGES")

	for _, r := range append(shown, total) {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\t%d\n", r.Tool, r.Calls, shift.Tokens(r.Chars),
			share(float64(r.Chars), float64(total.Chars)), r.Big, r.Images)
	}

	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nCHARS: the text of the tools' results (about 4 a token), read again with every turn after it until "+
		"a compaction or a restart; an image counts apart.")

	for _, p := range problems {
		fmt.Fprintln(out, "missing:", p)
	}

	return nil
}
