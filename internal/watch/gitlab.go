package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// commandMax caps an author instruction quoted in a note.
const commandMax = 200

// GitLab is the gitlab-mr kind: review threads and merge conflicts on the author's open merge requests, or with
// reviewer: answers, pushes and review requests on the merge requests the reviewer reviews; read through gl
// (gl --config <profile> api GET <path> --json).
type GitLab struct {
	W    *crew.Watch
	Bin  string
	Seen Seen
}

type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type glMR struct {
	IID                 int64    `json:"iid"`
	Title               string   `json:"title"`
	ProjectID           int64    `json:"project_id"`
	WebURL              string   `json:"web_url"`
	SHA                 string   `json:"sha"`
	TargetBranch        string   `json:"target_branch"`
	Draft               bool     `json:"draft"`
	WorkInProgress      bool     `json:"work_in_progress"`
	HasConflicts        bool     `json:"has_conflicts"`
	DetailedMergeStatus string   `json:"detailed_merge_status"`
	UpdatedAt           string   `json:"updated_at"`
	Author              glUser   `json:"author"`
	Reviewers           []glUser `json:"reviewers"`
	References          struct {
		Full string `json:"full"`
	} `json:"references"`
}

// ref is project!iid; project is the path without !iid.
func (m glMR) ref() string { return m.project() + "!" + strconv.FormatInt(m.IID, 10) }

func (m glMR) project() string {
	p, _, _ := strings.Cut(m.References.Full, "!")

	return p
}

func (m glMR) conflicted() bool { return m.HasConflicts || m.DetailedMergeStatus == "conflict" }

type glDiscussion struct {
	ID    string   `json:"id"`
	Notes []glNote `json:"notes"`
}

type glNote struct {
	ID         int64     `json:"id"`
	Body       string    `json:"body"`
	Author     glUser    `json:"author"`
	System     bool      `json:"system"`
	Resolvable bool      `json:"resolvable"`
	Resolved   bool      `json:"resolved"`
	CreatedAt  time.Time `json:"created_at"`
}

// mrCache is the cheap path: an MR whose updated_at and last_seen did not change keeps its review event (on the
// reviewer side, its events).
type mrCache struct {
	UpdatedAt string        `json:"updated_at"`
	Seen      string        `json:"seen"`
	Review    *cachedEvent  `json:"review,omitempty"`
	Events    []cachedEvent `json:"events,omitempty"`
}

// cachedEvent keeps what an event's JSON leaves out, so a cached info event stays info.
type cachedEvent struct {
	Event

	Info bool   `json:"info,omitempty"`
	Fold string `json:"fold,omitempty"`
}

func toCache(e *Event) *cachedEvent {
	if e == nil {
		return nil
	}

	return &cachedEvent{Event: *e, Info: e.Info, Fold: e.Fold}
}

func (c *cachedEvent) event() *Event {
	if c == nil {
		return nil
	}

	e := c.Event
	e.Info, e.Fold = c.Info, c.Fold

	return &e
}

// Run lists the author's open merge requests and reports a review and a conflict event per merge request.
func (g *GitLab) Run(ctx context.Context, rawCache json.RawMessage) Result {
	cache := map[string]mrCache{}
	if len(rawCache) > 0 {
		_ = json.Unmarshal(rawCache, &cache)
	}

	if g.W.Reviewer != "" {
		return g.runReviewer(ctx, cache)
	}

	author, err := g.user(ctx, g.W.Author)
	if err != nil {
		return Result{Err: err}
	}

	var mrs []glMR

	q := url.Values{"scope": {"all"}, "state": {"opened"}, "author_id": {strconv.FormatInt(author, 10)},
		"per_page": {"100"}, "with_merge_status_recheck": {"true"}}
	if err := g.get(ctx, "/merge_requests?"+q.Encode(), true, &mrs); err != nil {
		return Result{Err: err}
	}

	opts := reviewOpts{AuthorID: author, Voices: g.W.Voices, Allow: g.W.Allow, Command: g.W.CommandPrefix()}
	next := map[string]mrCache{}

	var events []Event

	for _, m := range mrs {
		if !g.wanted(m, author) {
			continue
		}

		reviewKey := m.ref() + ":review"
		seen := g.Seen[reviewKey]

		review, cached := (*Event)(nil), cache[m.ref()]
		if cached.UpdatedAt == m.UpdatedAt && cached.Seen == seen && m.UpdatedAt != "" {
			review = cached.Review.event()
		} else {
			var ds []glDiscussion

			path := fmt.Sprintf("/projects/%d/merge_requests/%d/discussions?per_page=100", m.ProjectID, m.IID)
			if err := g.get(ctx, path, true, &ds); err != nil {
				return Result{Err: err}
			}

			review = reviewEvent(m, ds, seen, opts)
		}

		next[m.ref()] = mrCache{UpdatedAt: m.UpdatedAt, Seen: seen, Review: toCache(review)}

		if review != nil {
			events = append(events, *review)
		}

		if !g.W.NotesConflicts() || !m.conflicted() || len(m.Reviewers) == 0 {
			continue
		}

		var branch struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}

		path := fmt.Sprintf("/projects/%d/repository/branches/%s", m.ProjectID, url.PathEscape(m.TargetBranch))
		if err := g.get(ctx, path, false, &branch); err != nil {
			return Result{Err: err}
		}

		c := Event{Key: m.ref() + ":conflict", Fingerprint: m.SHA + ":" + branch.Commit.ID, URL: m.WebURL,
			Text: fmt.Sprintf("%s — conflicts with %s (source %s, target %s)", m.ref(), m.TargetBranch, short(m.SHA),
				short(branch.Commit.ID))}
		// A review still to be told shares its moment, so both go out together.
		if review != nil && review.Fingerprint != seen && (!review.Info || g.W.Info) {
			c.At = review.At
		}

		events = append(events, c)
	}

	raw, _ := json.Marshal(next)

	return Result{Events: events, Cache: raw}
}

// wanted keeps the author's own merge requests in the listed projects, drafts only when asked.
func (g *GitLab) wanted(m glMR, author int64) bool {
	if m.Author.ID != author {
		return false
	}

	return g.listed(m)
}

// listed keeps merge requests in the listed projects, drafts only when asked.
func (g *GitLab) listed(m glMR) bool {
	if (m.Draft || m.WorkInProgress) && !g.W.Drafts {
		return false
	}

	if len(g.W.Projects) == 0 {
		return true
	}

	return slices.ContainsFunc(g.W.Projects, func(p string) bool { return strings.HasPrefix(m.project(), p) })
}

// user is the watched user's id: the token owner for me, else looked up by username.
func (g *GitLab) user(ctx context.Context, name string) (int64, error) {
	if name == "me" {
		var u glUser
		if err := g.get(ctx, "/user", false, &u); err != nil {
			return 0, err
		}

		return u.ID, nil
	}

	var us []glUser
	if err := g.get(ctx, "/users?username="+url.QueryEscape(name), false, &us); err != nil {
		return 0, err
	}

	if len(us) == 0 {
		return 0, fmt.Errorf("no GitLab user %q", name)
	}

	return us[0].ID, nil
}

func (g *GitLab) get(ctx context.Context, path string, paginate bool, v any) error {
	args := []string{"--config", g.W.GLConfig, "api", "GET", path, "--json"}
	if paginate {
		args = append(args, "--paginate")
	}

	cmd := exec.CommandContext(ctx, g.bin(), args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gl api GET %s: %s", strings.SplitN(path, "?", 2)[0], firstOr(lines(stderr.String()), err.Error()))
	}

	if err := json.Unmarshal(stdout.Bytes(), v); err != nil {
		return fmt.Errorf("gl api GET %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}

	return nil
}

func (g *GitLab) bin() string {
	if g.Bin != "" {
		return g.Bin
	}

	if p, err := exec.LookPath("gl"); err == nil {
		return p
	}

	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "bin", "gl")
	}

	return "gl"
}

type reviewOpts struct {
	AuthorID int64
	Voices   string
	Allow    []string
	Command  string
}

// review is what a merge request's threads say since last_seen.
type review struct {
	// Newest is the newest counted note; zero when nothing counts.
	Newest glNote
	// ReviewThreads are triggers by an unresolved note, CommandThreads by an author's instruction.
	ReviewThreads  int
	CommandThreads int
	// Commands are the author's instructions among the counted notes, in order.
	Commands []string
	// Voices are the usernames of counted notes by others, in order.
	Voices []string
	// Counted is the number of counted notes.
	Counted int
}

// decide reads the threads: a thread counts when its last voice is not the author's own answer; a counted note is
// a voice's note (or the author's command) newer than lastSeen. A thread whose last voice is a command, or that
// has an unresolved note, is a trigger; any other counted thread is info.
func decide(ds []glDiscussion, reviewers []glUser, lastSeen int64, o reviewOpts) review {
	var r review

	isAuthor := func(n glNote) bool { return n.Author.ID == o.AuthorID }
	isCommand := func(n glNote) bool { return isAuthor(n) && command(n.Body, o.Command) }
	isVoice := func(n glNote) bool {
		switch {
		case isAuthor(n), o.Voices == crew.VoicesAnyone, slices.Contains(o.Allow, n.Author.Username):
			return true
		case o.Voices == crew.VoicesReviewers:
			return slices.ContainsFunc(reviewers, func(u glUser) bool { return u.ID == n.Author.ID })
		}

		return false
	}

	for _, d := range ds {
		var notes, voices []glNote

		for _, n := range d.Notes {
			if n.System {
				continue
			}

			notes = append(notes, n)

			if isVoice(n) {
				voices = append(voices, n)
			}
		}

		if len(voices) == 0 {
			continue
		}

		last := voices[len(voices)-1]
		if isAuthor(last) && !isCommand(last) {
			// The author answered the thread.
			continue
		}

		var counted []glNote

		for _, n := range voices {
			if (!isAuthor(n) || isCommand(n)) && n.ID > lastSeen {
				counted = append(counted, n)
			}
		}

		if len(counted) == 0 {
			continue
		}

		switch {
		case isCommand(last):
			r.CommandThreads++
		case slices.ContainsFunc(notes, func(n glNote) bool { return n.Resolvable && !n.Resolved }):
			r.ReviewThreads++
		}

		for _, n := range counted {
			r.Counted++

			if n.ID > r.Newest.ID {
				r.Newest = n
			}

			if isCommand(n) {
				r.Commands = append(r.Commands, cut(OneLine(n.Body), commandMax))
			} else if !slices.Contains(r.Voices, n.Author.Username) {
				r.Voices = append(r.Voices, n.Author.Username)
			}
		}
	}

	return r
}

// command reports a note body that starts with the prefix followed by whitespace or its end, any case.
func command(body, prefix string) bool {
	if prefix == "" {
		return false
	}

	body = strings.TrimLeft(body, " \t\r\n")
	if len(body) < len(prefix) || !strings.EqualFold(body[:len(prefix)], prefix) {
		return false
	}

	rest := body[len(prefix):]

	return rest == "" || strings.ContainsAny(rest[:1], " \t\r\n")
}

// reviewEvent is the merge request's review event. With nothing counted it keeps the key alive at its stored
// fingerprint, so a review already told is not gone (and forgotten) while the merge request stays open.
func reviewEvent(m glMR, ds []glDiscussion, seen string, o reviewOpts) *Event {
	lastSeen, _ := strconv.ParseInt(seen, 10, 64)
	r := decide(ds, m.Reviewers, lastSeen, o)
	key := m.ref() + ":review"

	if r.Counted == 0 {
		if seen == "" {
			return nil
		}

		return &Event{Key: key, Fingerprint: seen, Text: m.ref() + " — no new review notes", URL: m.WebURL}
	}

	var parts []string

	who := ""
	if len(r.Voices) > 0 {
		who = " (" + strings.Join(r.Voices, ", ") + ")"
	}

	info := r.ReviewThreads+r.CommandThreads == 0

	switch {
	case r.ReviewThreads > 0:
		parts = append(parts, plural(r.ReviewThreads, "new review thread")+who)
	case info:
		parts = append(parts, plural(r.Counted, "new comment")+", no open threads"+who)
	}

	for _, c := range r.Commands {
		parts = append(parts, fmt.Sprintf("author instruction: %q", c))
	}

	at := r.Newest.CreatedAt

	return &Event{Key: key, Fingerprint: strconv.FormatInt(r.Newest.ID, 10), At: &at, URL: m.WebURL,
		Text: m.ref() + " — " + strings.Join(parts, "; "), Info: info}
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}

	return strconv.Itoa(n) + " " + what + "s"
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}

	return sha
}
