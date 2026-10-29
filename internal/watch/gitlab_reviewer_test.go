package watch

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// On the reviewer side the watched user is the reviewer (rev) and the merge request's author is alice (me).
var (
	ropts = reviewerOpts{Reviewer: reviewer, Voices: crew.VoicesReviewers}
	third = u(4, "other-dev")
)

func sysNote(id int64, who glUser, body string) glNote {
	s := n(id, who, body)
	s.System = true

	return s
}

func reviewMR() glMR {
	m := glMR{IID: 12, Title: "Fix the report", SHA: "head1", WebURL: "https://h/g/r/-/merge_requests/12",
		Author: author, Reviewers: []glUser{reviewer}}
	m.References.Full = "g/r!12"

	return m
}

func byKey(es []Event) map[string]Event {
	out := map[string]Event{}
	for _, e := range es {
		out[strings.TrimPrefix(e.Key, "g/r!12:")] = e
	}

	return out
}

func TestReviewerAnswered(t *testing.T) {
	asked := sysNote(1, author, "requested review from @reviewer-x")

	cases := []struct {
		name     string
		ds       []glDiscussion
		o        reviewerOpts
		lastSeen string
		answered bool
	}{
		{name: "the author's reply after the reviewer's note fires",
			ds: []glDiscussion{thread(open(n(5, reviewer, "fix this")), open(n(6, author, "fixed")))}, answered: true},
		{name: "a third voice with voices: reviewers does not",
			ds: []glDiscussion{thread(open(n(5, reviewer, "fix this")), open(n(6, third, "agree")))}},
		{name: "a third voice with voices: anyone does",
			ds: []glDiscussion{thread(open(n(5, reviewer, "fix this")), open(n(6, third, "agree")))},
			o:  reviewerOpts{Reviewer: reviewer, Voices: crew.VoicesAnyone}, answered: true},
		{name: "allow counts a username",
			ds: []glDiscussion{thread(open(n(5, reviewer, "fix this")), open(n(6, third, "done")))},
			o:  reviewerOpts{Reviewer: reviewer, Voices: crew.VoicesReviewers, Allow: []string{"other-dev"}}, answered: true},
		{name: "a resolved thread does not",
			ds: []glDiscussion{thread(resolved(n(5, reviewer, "fix this")), resolved(n(6, author, "fixed")))}},
		{name: "the reviewer spoke last: nothing to answer",
			ds: []glDiscussion{thread(open(n(5, reviewer, "fix")), open(n(6, author, "done")), open(n(7, reviewer, "ok")))}},
		{name: "a thread the reviewer is not in does not",
			ds: []glDiscussion{thread(open(n(5, third, "fix")), open(n(6, author, "done")))}},
		{name: "notes up to the last one noted do not count",
			ds:       []glDiscussion{thread(open(n(5, reviewer, "fix this")), open(n(6, author, "fixed")))},
			lastSeen: "6"},
		{name: "a mention fires anywhere, resolved too",
			ds: []glDiscussion{thread(resolved(n(5, third, "@reviewer-x can you look?")))}, answered: true},
		{name: "a mention the reviewer's later review has seen does not",
			ds: []glDiscussion{thread(n(5, third, "@reviewer-x see this")), thread(n(6, reviewer, "reviewed again"))}},
		{name: "a longer name is not a mention",
			ds: []glDiscussion{thread(n(5, third, "@reviewer-xy and me@reviewer-x"))}},
		{name: "the reviewer's own mention does not count",
			ds: []glDiscussion{thread(n(5, reviewer, "cc @reviewer-x"))}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := c.o
			if o.Voices == "" {
				o = ropts
			}

			seen := Seen{}
			if c.lastSeen != "" {
				seen["g/r!12:answered"] = c.lastSeen
			}

			ds := append([]glDiscussion{thread(asked)}, c.ds...)
			e, ok := byKey(reviewerEvents(reviewMR(), ds, seen, o))["answered"]

			if !c.answered {
				if ok {
					assert.Equal(t, c.lastSeen, e.Fingerprint, "only the stored fingerprint kept alive")
				}

				return
			}

			require.True(t, ok)
			assert.NotEqual(t, c.lastSeen, e.Fingerprint)
		})
	}
}

func TestReviewerEvents(t *testing.T) {
	m := reviewMR()
	asked := sysNote(1, author, "requested review from @someone and @reviewer-x")

	got := byKey(reviewerEvents(m, []glDiscussion{thread(asked)}, Seen{}, ropts))
	require.Contains(t, got, "requested")
	assert.Equal(t, `g/r!12 — review requested by alice: "Fix the report"`, got["requested"].Text)
	assert.Equal(t, "2:1", got["requested"].Fingerprint, "the reviewer and the request")
	assert.NotContains(t, got, "pushed", "no review yet: a push is not news")

	reviewed := []glDiscussion{
		thread(asked),
		thread(open(n(5, reviewer, "a")), open(n(8, author, "fixed a"))),
		thread(open(n(6, reviewer, "b")), open(n(9, author, "fixed b"))),
		thread(sysNote(7, reviewer, "approved this merge request")),
		thread(sysNote(10, author, "added 2 commits\n\n<ul><li>abc</li></ul>")),
		thread(sysNote(11, author, "added 1 commit\n\n<ul><li>def</li></ul>")),
	}

	es := reviewerEvents(m, reviewed, Seen{}, ropts)
	got = byKey(es)
	assert.NotContains(t, got, "requested", "a note or an approval takes the request up")
	assert.Equal(t, "g/r!12 — author answered 2 threads", got["answered"].Text)
	assert.Equal(t, "9", got["answered"].Fingerprint)
	assert.Equal(t, "g/r!12 — pushed 3 commits since your review", got["pushed"].Text)
	assert.Equal(t, "head1", got["pushed"].Fingerprint)
	assert.Equal(t, t0.Add(11*time.Minute), *got["answered"].At, "news shares the newest moment")
	assert.Equal(t, t0.Add(11*time.Minute), *got["pushed"].At)

	// A re-request after the review is a new request.
	again := append(reviewed, thread(sysNote(12, author, "requested review from @reviewer-x")))
	assert.Equal(t, "2:12", byKey(reviewerEvents(m, again, Seen{}, ropts))["requested"].Fingerprint)

	// Commits before the reviewer's last note are not a push since the review.
	later := append(reviewed, thread(n(13, reviewer, "re-checked")))
	assert.NotContains(t, byKey(reviewerEvents(m, later, Seen{}, ropts)), "pushed")
}

func TestReviewerFoldsOneNotePerMR(t *testing.T) {
	m := reviewMR()
	ds := []glDiscussion{
		thread(open(n(5, reviewer, "a")), open(n(8, author, "fixed"))),
		thread(sysNote(9, author, "added 2 commits")),
	}

	st, q := NewState(), &queue{}
	outs := Apply(st, reviewerEvents(m, ds, Seen{}, ropts), Config{}, t0.Add(time.Hour), q.add)
	assert.Equal(t, []string{"g/r!12:answered=note", "g/r!12:pushed=note"}, fates(outs))
	assert.Equal(t, []string{"g/r!12 — author answered 1 thread, pushed 2 commits since your review " +
		"https://h/g/r/-/merge_requests/12"}, q.lines)
	assert.Equal(t, "8", st.Keys["g/r!12:answered"].Fingerprint)
	assert.Equal(t, "head1", st.Keys["g/r!12:pushed"].Fingerprint)
}

func reviewsPath() string {
	q := url.Values{"scope": {"all"}, "state": {"opened"}, "reviewer_id": {"2"}, "per_page": {"100"}}

	return "/merge_requests?" + q.Encode()
}

func reviewerWatch(t *testing.T) *crew.Watch {
	t.Helper()

	w := &crew.Watch{Name: "reviews", Kind: crew.WatchGitLabMR, Every: "10m", GLConfig: "work", Reviewer: "me"}
	require.NoError(t, w.Check(&crew.Crew{Dir: t.TempDir()}))

	return w
}

func TestGitLabReviewerPushedOncePerSHA(t *testing.T) {
	gl := newFakeGL(t)
	gl.answer("/user", reviewer)

	mr := func(sha, updated string) map[string]any {
		m := mrJSON(sha, updated, false, reviewer)
		m["title"] = "Fix the report"

		return m
	}

	reviewed := []glDiscussion{thread(sysNote(5, reviewer, "approved this merge request"))}
	gl.answer(discPath, reviewed)
	gl.answer(reviewsPath(), []any{mr("aaaa", "u1")})

	w := reviewerWatch(t)
	st, q := NewState(), &queue{}
	c := Config{ForgetAfter: 24 * time.Hour}
	step := func(at time.Duration) []Outcome {
		res := runGL(w, st.Seen(), st.Cache)
		require.NoError(t, res.Err)

		st.Cache = res.Cache

		return Apply(st, res.Events, c, t0.Add(at), q.add)
	}

	assert.Empty(t, step(time.Hour), "reviewed, nothing since")

	gl.answer(discPath, append(reviewed, thread(sysNote(6, author, "added 1 commit"))))
	gl.answer(reviewsPath(), []any{mr("bbbb", "u2")})
	step(2 * time.Hour)
	assert.Equal(t, []string{"g/r!12 — pushed 1 commit since your review https://h/g/r/-/merge_requests/12"}, q.lines)

	gl.answer(reviewsPath(), []any{mr("bbbb", "u3")})
	assert.Equal(t, []string{"g/r!12:pushed=dup"}, fates(step(3*time.Hour)), "one SHA, one note")

	gl.answer(discPath, append(reviewed, thread(sysNote(6, author, "added 1 commit")),
		thread(sysNote(7, author, "added 1 commit"))))
	gl.answer(reviewsPath(), []any{mr("cccc", "u4")})
	step(4 * time.Hour)
	assert.Len(t, q.lines, 2, "a new SHA is news again")
	assert.Equal(t, "g/r!12 — pushed 2 commits since your review https://h/g/r/-/merge_requests/12", q.lines[1])

	step(5 * time.Hour) // the new stored fingerprint reads the threads once more
	gl.reset()
	step(6 * time.Hour)
	assert.NotContains(t, strings.Join(gl.calls(), " "), "discussions", "unchanged updated_at: no discussions call")
}

func TestGitLabReviewerFilters(t *testing.T) {
	gl := newFakeGL(t)
	gl.answer("/user", reviewer)

	draft := mrJSON("a", "u", false, reviewer)
	draft["draft"] = true
	notMine := mrJSON("a", "u", false, third)
	gl.answer(reviewsPath(), []any{draft, notMine})

	res := runGL(reviewerWatch(t), Seen{}, nil)
	require.NoError(t, res.Err)
	assert.Empty(t, res.Events)
	assert.Equal(t, []string{"/user", reviewsPath()}, gl.calls(), "drafts and MRs without the reviewer are never read")
}

func TestReviewerWatchCheck(t *testing.T) {
	c := &crew.Crew{Dir: t.TempDir()}
	w := &crew.Watch{Name: "reviews", Kind: crew.WatchGitLabMR, Every: "10m", GLConfig: "work", Reviewer: "me",
		Author: "me"}
	require.ErrorContains(t, w.Check(c), "not both")

	off, on := "", true
	w = &crew.Watch{Name: "reviews", Kind: crew.WatchGitLabMR, Every: "10m", GLConfig: "work", Reviewer: "me",
		Command: &off, Conflicts: &on}
	require.NoError(t, w.Check(c))
	assert.Empty(t, w.Author, "the reviewer side has no author default")
	assert.Equal(t, []string{"watch reviews: command does not apply with reviewer:, ignored",
		"watch reviews: conflicts does not apply with reviewer:, ignored"}, c.Warnings)
}

func TestGitLabCachedInfoStaysInfo(t *testing.T) {
	gl := newFakeGL(t)
	gl.answer("/user", author)
	gl.answer(mrsPath(), []any{mrJSON("aaaa", "u1", false, reviewer)})
	gl.answer(discPath, []glDiscussion{thread(n(5, reviewer, "no findings"))})

	w := glWatch(t)
	first := runGL(w, Seen{}, nil)
	require.NoError(t, first.Err)
	require.Len(t, first.Events, 1)
	require.True(t, first.Events[0].Info)

	again := runGL(w, Seen{}, first.Cache)
	require.Len(t, again.Events, 1)
	assert.True(t, again.Events[0].Info, "a cached info event is not noted as a trigger")
}
