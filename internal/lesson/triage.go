package lesson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The triage turns new lessons into edits of the crew's instruction files and offers them as ONE draft merge
// request on the crew's agentic-os. The sorting seat only reads and returns a plan (a JSON object); zmina applies the
// plan itself, one commit per lesson, after its own checks, on a branch cut from origin/<branch>, and pushes that
// one branch. Nothing here merges, approves or closes: the owner's merge is the only way a lesson applies.

const (
	// maxChanged is the most lines one lesson's edit may change; a bigger one goes to the owner as a question.
	maxChanged = 40
	// BranchPrefix names every branch the triage pushes, and the only refs it ever pushes.
	BranchPrefix = "lessons/"
)

// Verdicts the seat gives a lesson.
const (
	VerdictEvery    = "every"    // a rule for every seat: the repository's CLAUDE.md or AGENTS.md
	VerdictRole     = "role"     // one role's: its AGENT.md
	VerdictSkill    = "skill"    // a skill's SKILL.md
	VerdictWritten  = "written"  // already in a file
	VerdictStale    = "stale"    // a one-off, or no longer true
	VerdictDrop     = "drop"     // not a rule for the files (a model, a budget, a permission, a secret)
	VerdictConflict = "conflict" // against an instruction or a decision: a question for the owner, never applied
)

// Item is the seat's verdict on one lesson, with the edit for every, role and skill.
type Item struct {
	ID       string `json:"id"`
	Verdict  string `json:"verdict"`
	File     string `json:"file,omitempty"`
	Old      string `json:"old,omitempty"`
	New      string `json:"new,omitempty"`
	Reason   string `json:"reason"`
	Question string `json:"question,omitempty"`
}

// Plan is the seat's answer.
type Plan struct {
	Items     []Item   `json:"lessons"`
	Questions []string `json:"questions,omitempty"`
}

// PlanSchema is the JSON schema the seat's answer must follow.
const PlanSchema = `{"type":"object","properties":{"lessons":{"type":"array","items":{"type":"object","properties":{` +
	`"id":{"type":"string"},"verdict":{"type":"string","enum":["every","role","skill","written","stale","drop","conflict"]},` +
	`"file":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},"reason":{"type":"string"},` +
	`"question":{"type":"string"}},"required":["id","verdict","reason"]}},"questions":{"type":"array","items":{"type":"string"}}},` +
	`"required":["lessons"]}`

// CheckSchema is the JSON schema of the checker's answer.
const CheckSchema = `{"type":"object","properties":{"checks":{"type":"array","items":{"type":"object","properties":{` +
	`"id":{"type":"string"},"ok":{"type":"boolean"},"problem":{"type":"string","enum":["","duplicate","conflict",` +
	`"wrong-file","unclear"]},"reason":{"type":"string"}},"required":["id","ok","reason"]}}},"required":["checks"]}`

// Check is the checker's word on one proposed edit.
type Check struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
	Reason  string `json:"reason"`
}

// Seat runs a read-only seat in dir with the prompt, its answer held to the schema, stopped at the budget, and
// returns its raw JSON answer (claude -p --output-format json).
type Seat func(ctx context.Context, dir, prompt, schema string, budget float64) ([]byte, error)

// ClaudeSeat is the seat on Claude Code: a headless run that may only read (Read, Grep, Glob), with no MCP servers,
// stopped at the budget by the CLI itself, its answer held to the schema.
func ClaudeSeat(model string) Seat {
	return func(ctx context.Context, dir, prompt, schema string, budget float64) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "claude", "-p", "--model", model,
			"--max-budget-usd", fmt.Sprintf("%.2f", budget), "--output-format", "json", "--json-schema", schema,
			"--allowedTools", "Read,Grep,Glob",
			"--disallowedTools", "Bash,Edit,Write,NotebookEdit,WebFetch,WebSearch,Agent,Task",
			"--strict-mcp-config", "--no-session-persistence")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(prompt)

		var stderr bytes.Buffer

		cmd.Stderr = &stderr

		out, err := cmd.Output()
		if err != nil && len(out) == 0 {
			return nil, fmt.Errorf("claude: %w: %s", err, strings.TrimSpace(lastLine(stderr.String())))
		}

		return out, nil
	}
}

// Answer is the part of claude -p's JSON result the triage reads.
type Answer struct {
	IsError bool            `json:"is_error"`
	Subtype string          `json:"subtype"`
	Cost    float64         `json:"total_cost_usd"`
	Plan    json.RawMessage `json:"structured_output"`
	Result  string          `json:"result"`
}

// Config is one run.
type Config struct {
	// Repo is the agentic-os checkout, Branch the target branch, Agents the crew's agents dir (inside Repo).
	Repo, Branch, Agents string
	// Roles are the crew's role names (their AGENT.md files are the role verdict's targets).
	Roles []string
	// Work is a directory of zmina's own for the run's worktree.
	Work string
	// Deny are the crew's own words no edit may carry.
	Deny []string
	// Remote is the remote to fetch from and push to (default origin).
	Remote string
	// Budget is what the run may cost in all: the sorting seat gets 60% of it, the checker the rest.
	Budget float64
	Now    time.Time
}

// Applied is a lesson committed on the branch.
type Applied struct {
	ID, File, Commit string
}

// Result is what a run did.
type Result struct {
	Branch    string
	Base      string // the origin commit the branch was cut from
	Tip       string // the branch's last commit, when anything was committed
	Applied   []Applied
	Dropped   []Item // written, stale, drop
	Questions []string
	Cost      float64
	MRURL     string
	Pushed    bool
	// PushNote says why the push did not open a merge request by itself (a remote without push options).
	PushNote string
	// Draft is whether GitLab holds the MR as a draft, read back after the push: "yes", "no", "" (not read: no
	// glab or gl profile for its host on this node). DraftVia names the client that read it.
	Draft, DraftVia string
}

// Git runs git in a directory and returns its output.
func Git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(out)), nil
}

// FindRepo is the git toplevel of the crew's agents dir: the crew's own agentic-os.
func FindRepo(agents string) (string, error) {
	if agents == "" {
		return "", errors.New("the crew names no agents dir")
	}

	top, err := Git(agents, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("the agents dir %s is not in a git repository: %w", agents, err)
	}

	return top, nil
}

// DefaultBranch is the remote's default branch (origin/HEAD), else main.
func DefaultBranch(repo, remote string) string {
	if ref, err := Git(repo, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		return strings.TrimPrefix(ref, remote+"/")
	}

	return "main"
}

// Triage sorts the lessons with the seat and commits the plan on a new branch cut from <remote>/<branch>. It does
// not push: Push does, once the caller has recorded what it is about to push.
func Triage(ctx context.Context, cfg Config, lessons []Lesson, seat Seat) (Result, error) {
	if cfg.Remote == "" {
		cfg.Remote = "origin"
	}

	res := Result{Branch: BranchPrefix + cfg.Now.Format("2006-01-02")}

	if _, err := Git(cfg.Repo, "fetch", "-q", cfg.Remote, cfg.Branch); err != nil {
		return res, err
	}

	base, err := Git(cfg.Repo, "rev-parse", cfg.Remote+"/"+cfg.Branch)
	if err != nil {
		return res, err
	}

	res.Base = base

	for n := 2; branchTaken(cfg.Repo, cfg.Remote, res.Branch); n++ {
		res.Branch = fmt.Sprintf("%s%s-%d", BranchPrefix, cfg.Now.Format("2006-01-02"), n)
	}

	wt := filepath.Join(cfg.Work, strings.ReplaceAll(res.Branch, "/", "-"))
	_, _ = Git(cfg.Repo, "worktree", "remove", "--force", wt)

	if _, err := Git(cfg.Repo, "worktree", "add", "-q", "-b", res.Branch, wt, base); err != nil {
		return res, err
	}

	defer func() { _, _ = Git(cfg.Repo, "worktree", "remove", "--force", wt) }()

	allowed := allowlist(cfg)

	var plan Plan

	cost, err := ask(ctx, seat, wt, prompt(cfg, lessons, allowed), PlanSchema, cfg.Budget*0.6, &plan)
	res.Cost += cost

	if err != nil {
		return res, fmt.Errorf("the sorting seat: %w", err)
	}

	// The checker is a second, independent reading of every proposed edit: one it finds a duplicate, a conflict or
	// aimed at the wrong file goes to the owner as a question instead of into a commit.
	if edits := proposed(plan); len(edits) > 0 {
		var checked struct {
			Checks []Check `json:"checks"`
		}

		cost, err := ask(ctx, seat, wt, checkPrompt(cfg, lessons, edits), CheckSchema, cfg.Budget*0.4, &checked)
		res.Cost += cost

		if err != nil {
			return res, fmt.Errorf("the checker: %w", err)
		}

		plan = vetted(plan, checked.Checks)
	}

	res.Questions = append(res.Questions, plan.Questions...)

	byID := map[string]Lesson{}
	for _, l := range lessons {
		byID[l.ID] = l
	}

	seen := map[string]bool{}

	for _, it := range plan.Items {
		it.ID = Normalize(it.ID)

		l, ok := byID[it.ID]
		if !ok || seen[it.ID] {
			continue
		}

		seen[it.ID] = true

		switch it.Verdict {
		case VerdictWritten, VerdictStale, VerdictDrop:
			res.Dropped = append(res.Dropped, it)
		case VerdictEvery, VerdictRole, VerdictSkill:
			sha, why := apply(wt, it, l, allowed, cfg.Deny)
			if why != "" {
				res.Questions = append(res.Questions, fmt.Sprintf("%s (%s): not applied, %s", it.ID, l.Rule, why))

				continue
			}

			res.Applied = append(res.Applied, Applied{ID: it.ID, File: it.File, Commit: sha})
		default: // conflict, or a verdict the schema does not know: never applied
			q := it.Question
			if q == "" {
				q = it.Reason
			}

			res.Questions = append(res.Questions, fmt.Sprintf("%s (%s): %s", it.ID, l.Rule, q))
		}
	}

	for _, l := range lessons {
		if !seen[l.ID] {
			res.Questions = append(res.Questions, fmt.Sprintf("%s (%s): the seat gave it no verdict", l.ID, l.Rule))
		}
	}

	if len(res.Applied) > 0 {
		res.Tip, _ = Git(wt, "rev-parse", "HEAD")
	}

	return res, nil
}

// ask runs a seat and reads its structured answer into out; it returns the run's cost even when it fails.
func ask(ctx context.Context, seat Seat, dir, prompt, schema string, budget float64, out any) (float64, error) {
	raw, err := seat(ctx, dir, prompt, schema, budget)
	if err != nil {
		return 0, err
	}

	var ans Answer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return 0, fmt.Errorf("its answer is not JSON: %w", err)
	}

	if ans.IsError || len(ans.Plan) == 0 {
		return ans.Cost, fmt.Errorf("no answer (%s): %s", ans.Subtype, firstLine(ans.Result))
	}

	if err := json.Unmarshal(ans.Plan, out); err != nil {
		return ans.Cost, fmt.Errorf("its answer: %w", err)
	}

	return ans.Cost, nil
}

// proposed is the plan's edits: every, role and skill.
func proposed(plan Plan) []Item {
	var out []Item

	for _, it := range plan.Items {
		switch it.Verdict {
		case VerdictEvery, VerdictRole, VerdictSkill:
			out = append(out, it)
		}
	}

	return out
}

// vetted turns every edit the checker did not pass into a conflict with its reason as the question. An edit the
// checker said nothing of is not passed either: the second reading is a gate, not advice.
func vetted(plan Plan, checks []Check) Plan {
	byID := map[string]Check{}
	for _, c := range checks {
		byID[Normalize(c.ID)] = c
	}

	for i, it := range plan.Items {
		switch it.Verdict {
		case VerdictEvery, VerdictRole, VerdictSkill:
		default:
			continue
		}

		c, ok := byID[Normalize(it.ID)]
		if ok && c.OK {
			continue
		}

		why := "the checker did not look at this edit"
		if ok {
			why = "the checker: " + strings.TrimSpace(c.Problem+" "+c.Reason)
		}

		plan.Items[i].Verdict, plan.Items[i].Question = VerdictConflict, fmt.Sprintf("edit to %s held back, %s", it.File, why)
	}

	return plan
}

func branchTaken(repo, remote, branch string) bool {
	if _, err := Git(repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch); err == nil {
		return true
	}

	out, err := Git(repo, "ls-remote", "--heads", remote, branch)

	return err == nil && out != ""
}

// allowlist is the files an edit may touch, as path patterns relative to the repository: its CLAUDE.md and AGENTS.md,
// each role's AGENT.md / AGENTS.md in the agents dir, and skills' SKILL.md.
func allowlist(cfg Config) []string {
	rel, err := filepath.Rel(cfg.Repo, cfg.Agents)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = ""
	}

	out := []string{"CLAUDE.md", "AGENTS.md"}

	if rel != "" && rel != "." {
		out = append(out, path.Join(filepath.ToSlash(rel), "*", "AGENT.md"), path.Join(filepath.ToSlash(rel), "*", "AGENTS.md"))
	}

	return append(out, "skills/*/SKILL.md", "*/skills/*/SKILL.md", "*/*/skills/*/SKILL.md")
}

func allowed(file string, patterns []string) bool {
	if file == "" || path.IsAbs(file) || strings.Contains(file, "..") || strings.Contains(file, "\\") {
		return false
	}

	for _, p := range patterns {
		if ok, _ := path.Match(p, file); ok {
			return true
		}
	}

	return false
}

// apply makes one lesson's edit in the worktree and commits it; why says what stopped it.
func apply(wt string, it Item, l Lesson, patterns, deny []string) (string, string) {
	file := path.Clean(filepath.ToSlash(it.File))
	if !allowed(file, patterns) {
		return "", fmt.Sprintf("%q is not an instruction file it may change", it.File)
	}

	full := filepath.Join(wt, filepath.FromSlash(file))

	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Sprintf("%s is not a file in the repository", file)
	}

	raw, err := os.ReadFile(full)
	if err != nil {
		return "", err.Error()
	}

	text := string(raw)

	var next string

	switch {
	case strings.TrimSpace(it.New) == "":
		return "", "the edit is empty"
	case it.Old == "":
		next = strings.TrimRight(text, "\n") + "\n\n" + strings.TrimRight(it.New, "\n") + "\n"
	case strings.Count(text, it.Old) != 1:
		return "", fmt.Sprintf("the text to replace is found %d times in %s, not once", strings.Count(text, it.Old), file)
	default:
		next = strings.Replace(text, it.Old, it.New, 1)
	}

	if n := changedLines(it.Old, it.New); n > maxChanged {
		return "", fmt.Sprintf("the edit changes %d lines, more than %d", n, maxChanged)
	}

	if f := Scan(it.New, deny); len(f) > 0 {
		return "", "the edit carries " + f[0].Rule
	}

	if err := os.WriteFile(full, []byte(next), info.Mode().Perm()); err != nil {
		return "", err.Error()
	}

	msg := commitMessage(l)
	if _, err := Git(wt, "add", "--", file); err != nil {
		return "", err.Error()
	}

	if _, err := Git(wt, "commit", "-q", "-m", msg); err != nil {
		_, _ = Git(wt, "checkout", "--", file)

		return "", err.Error()
	}

	sha, _ := Git(wt, "rev-parse", "HEAD")

	return sha, ""
}

func changedLines(old, next string) int {
	a, b := strings.Count(old, "\n")+1, strings.Count(next, "\n")+1
	if old == "" {
		a = 0
	}

	return max(a, b)
}

// commitMessage is one lesson's commit: its rule, then why, who learned it and its id.
func commitMessage(l Lesson) string {
	subject := strings.Join(strings.Fields(l.Rule), " ")
	if len(subject) > 72 {
		subject = strings.TrimSpace(subject[:69]) + "..."
	}

	body := []string{"Why: " + strings.Join(strings.Fields(l.Why), " ")}

	from := l.Role
	if l.Task != "" {
		from += ", " + l.Task
	}

	if from != "" {
		body = append(body, "From: "+strings.TrimPrefix(from, ", "))
	}

	body = append(body, "Lesson: "+l.ID)

	return subject + "\n\n" + strings.Join(body, "\n")
}

// mrURL finds the merge request a GitLab push created or names, in its remote messages.
var mrURL = regexp.MustCompile(`https?://\S+/-/merge_requests/\d+`)

// Push pushes the run's branch, and only it, to the remote, asking GitLab (push options) to open a draft merge
// request into the target branch. It refuses a branch whose commits over <remote>/<branch> are not exactly the run's
// own: nothing of the local checkout's unpushed work ever goes with it.
func Push(cfg Config, res *Result, title, description string) error {
	if cfg.Remote == "" {
		cfg.Remote = "origin"
	}

	if !strings.HasPrefix(res.Branch, BranchPrefix) || len(res.Applied) == 0 {
		return fmt.Errorf("refusing to push %q: not a lessons branch with commits", res.Branch)
	}

	commits, err := Git(cfg.Repo, "rev-list", "--reverse", res.Base+".."+res.Branch)
	if err != nil {
		return err
	}

	var ours []string
	for _, a := range res.Applied {
		ours = append(ours, a.Commit)
	}

	if !slices.Equal(strings.Fields(commits), ours) || !lessonCommits(cfg.Repo, res.Applied) {
		return fmt.Errorf("refusing to push %s: its commits are not exactly the lessons this run made", res.Branch)
	}

	ref := "refs/heads/" + res.Branch + ":refs/heads/" + res.Branch
	opts := []string{"merge_request.create", "merge_request.draft", "merge_request.target=" + cfg.Branch,
		"merge_request.title=" + oneLine(DraftTitle(title)), "merge_request.description=" + oneLine(description),
		"merge_request.remove_source_branch"}

	args := []string{"push", "--no-verify"}
	for _, o := range opts {
		args = append(args, "-o", o)
	}

	out, err := gitCombined(cfg.Repo, append(args, cfg.Remote, ref)...)
	if err != nil && strings.Contains(out, "push options") {
		res.PushNote = "the remote takes no push options: the branch is pushed, open the merge request by hand"
		out, err = gitCombined(cfg.Repo, "push", "--no-verify", cfg.Remote, ref)
	}

	if err != nil {
		return fmt.Errorf("push %s: %w: %s", res.Branch, err, lastLine(out))
	}

	res.Pushed = true
	res.MRURL = mrURL.FindString(out)

	return nil
}

// lessonCommits reports whether each commit is a lesson's own: its message names that lesson and it changes that
// lesson's file and nothing else.
func lessonCommits(repo string, applied []Applied) bool {
	for _, a := range applied {
		msg, err := Git(repo, "show", "-s", "--format=%B", a.Commit)
		if err != nil || !strings.Contains(msg, "\nLesson: "+a.ID) {
			return false
		}

		files, err := Git(repo, "diff-tree", "--no-commit-id", "--name-only", "-r", a.Commit)
		if err != nil || files != a.File {
			return false
		}
	}

	return true
}

func gitCombined(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()

	return string(out), err
}

// Finished reports whether a pushed lessons branch is done with: merged into <remote>/<branch> (its tip is there),
// or gone from the remote (merged with its source branch removed, or closed and deleted). merged tells which.
func Finished(repo, remote, branch, target, tip string) (done, merged bool, err error) {
	if remote == "" {
		remote = "origin"
	}

	if _, err := Git(repo, "fetch", "-q", remote, target); err != nil {
		return false, false, err
	}

	merged = tip != "" && isAncestor(repo, tip, remote+"/"+target)

	heads, err := Git(repo, "ls-remote", "--heads", remote, branch)
	if err != nil {
		return false, false, err
	}

	return merged || heads == "", merged, nil
}

func isAncestor(repo, a, b string) bool {
	_, err := Git(repo, "merge-base", "--is-ancestor", a, b)

	return err == nil
}

// Catchup brings the checkout the roles read up to <remote>/<branch> when that is a plain fast-forward: on that
// branch, clean, not ahead. Otherwise it changes nothing and says the one command for the owner.
func Catchup(checkout, remote, branch string) (bool, string) {
	if remote == "" {
		remote = "origin"
	}

	cmd := fmt.Sprintf("git -C %s pull --no-rebase %s %s", checkout, remote, branch)

	cur, err := Git(checkout, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || cur != branch {
		return false, cmd
	}

	if st, err := Git(checkout, "status", "--porcelain", "--untracked-files=no"); err != nil || st != "" {
		return false, cmd
	}

	if ahead, err := Git(checkout, "rev-list", "--count", remote+"/"+branch+"..HEAD"); err != nil || ahead != "0" {
		return false, cmd
	}

	if _, err := Git(checkout, "merge", "--ff-only", "-q", remote+"/"+branch); err != nil {
		return false, cmd
	}

	return true, ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstLine(s string) string { l, _, _ := strings.Cut(strings.TrimSpace(s), "\n"); return l }

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}

	return s
}
