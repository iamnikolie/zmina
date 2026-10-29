package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// pushedRe reads GitLab's system note for a push: "added 2 commits".
var pushedRe = regexp.MustCompile(`^added (\d+) commits?\b`)

// runReviewer lists the open merge requests the reviewer is asked to review and reports, per merge request, a
// review request not yet taken up, answers to the reviewer's threads, and pushes since the reviewer's review.
func (g *GitLab) runReviewer(ctx context.Context, cache map[string]mrCache) Result {
	reviewer, err := g.user(ctx, g.W.Reviewer)
	if err != nil {
		return Result{Err: err}
	}

	var mrs []glMR

	q := url.Values{"scope": {"all"}, "state": {"opened"}, "reviewer_id": {strconv.FormatInt(reviewer, 10)},
		"per_page": {"100"}}
	if err := g.get(ctx, "/merge_requests?"+q.Encode(), true, &mrs); err != nil {
		return Result{Err: err}
	}

	next := map[string]mrCache{}

	var events []Event

	for _, m := range mrs {
		i := slices.IndexFunc(m.Reviewers, func(u glUser) bool { return u.ID == reviewer })
		if i < 0 || !g.listed(m) {
			continue
		}

		o := reviewerOpts{Reviewer: m.Reviewers[i], Voices: g.W.Voices, Allow: g.W.Allow}
		seen := strings.Join([]string{g.Seen[m.ref()+":requested"], g.Seen[m.ref()+":answered"],
			g.Seen[m.ref()+":pushed"]}, "|")

		var es []Event

		if cached := cache[m.ref()]; cached.UpdatedAt == m.UpdatedAt && cached.Seen == seen && m.UpdatedAt != "" {
			for _, c := range cached.Events {
				es = append(es, *c.event())
			}
		} else {
			var ds []glDiscussion

			path := fmt.Sprintf("/projects/%d/merge_requests/%d/discussions?per_page=100", m.ProjectID, m.IID)
			if err := g.get(ctx, path, true, &ds); err != nil {
				return Result{Err: err}
			}

			es = reviewerEvents(m, ds, g.Seen, o)
		}

		c := mrCache{UpdatedAt: m.UpdatedAt, Seen: seen}
		for _, e := range es {
			c.Events = append(c.Events, *toCache(&e))
		}

		next[m.ref()] = c
		events = append(events, es...)
	}

	raw, _ := json.Marshal(next)

	return Result{Events: events, Cache: raw}
}

type reviewerOpts struct {
	Reviewer glUser
	Voices   string
	Allow    []string
}

// reviewerEvents reads a merge request's notes for the reviewer: key :requested while the reviewer has left no
// note or approval since the latest review request; :answered when, after the reviewer's last note in a thread the
// reviewer is in, the author (or a counted voice) wrote and the thread is unresolved, or a note after the
// reviewer's last note or approval mentions the reviewer — only notes newer than the last one noted count; :pushed when commits were added after the reviewer's
// last note or approval. The events that are news share the moment of the newest, so they go out together.
func reviewerEvents(m glMR, ds []glDiscussion, seen Seen, o reviewerOpts) []Event {
	ref := m.ref()
	mention := mentionRe(o.Reviewer.Username)
	isReviewer := func(n glNote) bool { return n.Author.ID == o.Reviewer.ID }

	var lastActivity int64

	var request, push glNote

	pushed := 0

	for _, d := range ds {
		for _, n := range d.Notes {
			switch {
			case isReviewer(n) && (!n.System || strings.HasPrefix(n.Body, "approved this merge request")):
				lastActivity = max(lastActivity, n.ID)
			case n.System && strings.HasPrefix(n.Body, "requested review from") && mention.MatchString(n.Body) &&
				n.ID > request.ID:
				request = n
			}
		}
	}

	for _, d := range ds {
		for _, n := range d.Notes {
			if sm := pushedRe.FindStringSubmatch(n.Body); n.System && sm != nil && lastActivity > 0 && n.ID > lastActivity {
				c, _ := strconv.Atoi(sm[1])
				pushed += c

				if n.ID > push.ID {
					push = n
				}
			}
		}
	}

	var events []Event

	event := func(kind, fingerprint, part string, at *time.Time) {
		events = append(events, Event{Key: ref + ":" + kind, Fingerprint: fingerprint, Text: ref + " — " + part,
			At: at, URL: m.WebURL, Fold: ref})
	}

	if lastActivity == 0 || request.ID > lastActivity {
		fingerprint, by, at := strconv.FormatInt(o.Reviewer.ID, 10), m.Author.Username, (*time.Time)(nil)
		if request.ID > 0 {
			fingerprint += ":" + strconv.FormatInt(request.ID, 10)
			by, at = request.Author.Username, &request.CreatedAt
		}

		part := "review requested"
		if by != "" {
			part += " by " + by
		}

		if m.Title != "" {
			part += fmt.Sprintf(": %q", cut(OneLine(m.Title), commandMax))
		}

		event("requested", fingerprint, part, at)
	}

	lastSeen, _ := strconv.ParseInt(seen[ref+":answered"], 10, 64)
	if a := answers(ds, m.Author.ID, lastSeen, lastActivity, o, mention); a.Newest.ID > 0 {
		var parts []string

		switch {
		case a.Threads > 0 && slices.Equal(a.Answerers, []string{m.Author.Username}):
			parts = append(parts, "author answered "+plural(a.Threads, "thread"))
		case a.Threads > 0:
			parts = append(parts, plural(a.Threads, "thread")+" answered ("+strings.Join(a.Answerers, ", ")+")")
		}

		if a.Mentions > 0 {
			parts = append(parts, plural(a.Mentions, "mention")+" ("+strings.Join(a.Mentioners, ", ")+")")
		}

		at := a.Newest.CreatedAt
		event("answered", strconv.FormatInt(a.Newest.ID, 10), strings.Join(parts, ", "), &at)
	} else if s := seen[ref+":answered"]; s != "" {
		// Nothing new keeps the key alive at its stored fingerprint, so answers already told are not forgotten.
		event("answered", s, "no new answers", nil)
	}

	if pushed > 0 {
		at := push.CreatedAt
		event("pushed", m.SHA, "pushed "+plural(pushed, "commit")+" since your review", &at)
	}

	var newest *time.Time

	for _, e := range events {
		if e.Fingerprint != seen[e.Key] && e.At != nil && (newest == nil || e.At.After(*newest)) {
			newest = e.At
		}
	}

	for i, e := range events {
		if e.Fingerprint != seen[e.Key] && newest != nil {
			events[i].At = newest
		}
	}

	return events
}

// answered is what a merge request's threads hold for the reviewer since last_seen.
type answered struct {
	// Newest is the newest counted note; zero when nothing counts.
	Newest glNote
	// Threads are unresolved threads the reviewer is in that were answered after the reviewer's last note.
	Threads int
	// Mentions are other notes that mention the reviewer after the reviewer's last note or approval.
	Mentions int
	// Answerers and Mentioners are the usernames, in order.
	Answerers, Mentioners []string
}

// answers counts, in each thread after the reviewer's last note there, the answers of the author (or a counted
// voice) when the reviewer is in the thread and it is unresolved, and the notes after the reviewer's last activity
// on the merge request that mention the reviewer. Only notes newer than lastSeen count.
func answers(ds []glDiscussion, authorID, lastSeen, lastActivity int64, o reviewerOpts, mention *regexp.Regexp) answered {
	var a answered

	isAnswer := func(n glNote) bool {
		switch {
		case n.Author.ID == o.Reviewer.ID:
			return false
		case n.Author.ID == authorID, o.Voices == crew.VoicesAnyone, slices.Contains(o.Allow, n.Author.Username):
			return true
		}

		return false
	}

	count := func(n glNote, who *[]string) {
		if n.ID > a.Newest.ID {
			a.Newest = n
		}

		if !slices.Contains(*who, n.Author.Username) {
			*who = append(*who, n.Author.Username)
		}
	}

	for _, d := range ds {
		var notes []glNote

		last := -1

		for _, n := range d.Notes {
			if n.System {
				continue
			}

			if n.Author.ID == o.Reviewer.ID {
				last = len(notes)
			}

			notes = append(notes, n)
		}

		open := slices.ContainsFunc(notes, func(n glNote) bool { return n.Resolvable && !n.Resolved })
		threadCounted := false

		for i := last + 1; i < len(notes); i++ {
			n := notes[i]
			if n.ID <= lastSeen || n.Author.ID == o.Reviewer.ID {
				continue
			}

			if last >= 0 && open && isAnswer(n) {
				if !threadCounted {
					a.Threads++
					threadCounted = true
				}

				count(n, &a.Answerers)

				continue
			}

			if n.ID > lastActivity && mention.MatchString(n.Body) {
				a.Mentions++
				count(n, &a.Mentioners)
			}
		}
	}

	return a
}

// mentionRe matches @username as GitLab links it: not inside a longer name or an address.
func mentionRe(username string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.@-])@` + regexp.QuoteMeta(username) + `($|[^A-Za-z0-9_-])`)
}
