package watch

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

const (
	me  = 1
	rev = 2
	bot = 3
)

func u(id int64, name string) glUser { return glUser{ID: id, Username: name} }

func n(id int64, who glUser, body string) glNote {
	return glNote{ID: id, Author: who, Body: body, CreatedAt: t0.Add(time.Duration(id) * time.Minute)}
}

func open(n glNote) glNote     { n.Resolvable = true; return n }
func resolved(n glNote) glNote { n.Resolvable, n.Resolved = true, true; return n }

func thread(notes ...glNote) glDiscussion { return glDiscussion{ID: "d", Notes: notes} }

var (
	author   = u(me, "alice")
	reviewer = u(rev, "reviewer-x")
	passer   = u(bot, "ci-bot")
	opts     = reviewOpts{AuthorID: me, Voices: crew.VoicesReviewers, Command: "/agent"}
)

func TestDecideRules(t *testing.T) {
	sys := n(9, reviewer, "added 1 commit")
	sys.System = true

	cases := []struct {
		name     string
		ds       []glDiscussion
		lastSeen int64
		o        reviewOpts
		review   int
		command  int
		counted  int
	}{
		{name: "system notes are ignored", ds: []glDiscussion{thread(sys)}},
		{name: "an unresolved thread is a trigger", ds: []glDiscussion{thread(open(n(5, reviewer, "fix this")))},
			review: 1, counted: 1},
		{name: "a non-voice last note is invisible: the author's answer still stands",
			ds: []glDiscussion{thread(open(n(5, reviewer, "fix this")), open(n(6, author, "done")), open(n(7, passer, "lgtm")))}},
		{name: "a non-voice note alone counts nothing", ds: []glDiscussion{thread(open(n(7, passer, "bump")))}},
		{name: "the author as last voice: answered, skipped",
			ds: []glDiscussion{thread(open(n(5, reviewer, "fix this")), open(n(6, author, "fixed")))}},
		{name: "a command as last voice triggers even in a resolved thread",
			ds:      []glDiscussion{thread(resolved(n(5, reviewer, "nit")), resolved(n(6, author, "  /AGENT take option B")))},
			command: 1, counted: 2},
		{name: "a top-level command triggers", ds: []glDiscussion{thread(n(6, author, "/agent rebase please"))},
			command: 1, counted: 1},
		{name: "the prefix must end in whitespace", ds: []glDiscussion{thread(n(6, author, "/agents are nice"))}},
		{name: "a resolved thread is info", ds: []glDiscussion{thread(resolved(n(5, reviewer, "looks good")))}, counted: 1},
		{name: "a top-level note is info", ds: []glDiscussion{thread(n(5, reviewer, "no findings"))}, counted: 1},
		{name: "notes up to last_seen do not count",
			ds: []glDiscussion{thread(open(n(5, reviewer, "a"))), thread(open(n(8, reviewer, "b")))}, lastSeen: 5,
			review: 1, counted: 1},
		{name: "voices: anyone counts a passer-by", ds: []glDiscussion{thread(open(n(7, passer, "fix")))},
			o: reviewOpts{AuthorID: me, Voices: crew.VoicesAnyone, Command: "/agent"}, review: 1, counted: 1},
		{name: "allow counts a username", ds: []glDiscussion{thread(open(n(7, passer, "fix")))},
			o:      reviewOpts{AuthorID: me, Voices: crew.VoicesReviewers, Allow: []string{"ci-bot"}, Command: "/agent"},
			review: 1, counted: 1},
		{name: "matching is by id: a renamed reviewer still counts",
			ds: []glDiscussion{thread(open(n(5, u(rev, "renamed"), "fix")))}, review: 1, counted: 1},
		{name: "command off", ds: []glDiscussion{thread(n(6, author, "/agent go"))},
			o: reviewOpts{AuthorID: me, Voices: crew.VoicesReviewers}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := c.o
			if o.Voices == "" {
				o = opts
			}

			r := decide(c.ds, []glUser{reviewer}, c.lastSeen, o)
			assert.Equal(t, c.review, r.ReviewThreads, "review threads")
			assert.Equal(t, c.command, r.CommandThreads, "command threads")
			assert.Equal(t, c.counted, r.Counted, "counted notes")
		})
	}
}

func TestReviewEventText(t *testing.T) {
	m := glMR{IID: 12, WebURL: "https://h/g/r/-/merge_requests/12", Reviewers: []glUser{reviewer}}
	m.References.Full = "g/r!12"

	ds := []glDiscussion{
		thread(open(n(5, reviewer, "a"))), thread(open(n(6, reviewer, "b"))),
		thread(resolved(n(7, reviewer, "c")), resolved(n(8, author, "/agent take option B,\nkeep the old name"))),
	}

	e := reviewEvent(m, ds, "", opts)
	require.NotNil(t, e)
	assert.Equal(t, "g/r!12:review", e.Key)
	assert.Equal(t, "8", e.Fingerprint, "the newest counted note")
	assert.Equal(t, t0.Add(8*time.Minute), *e.At, "quiet is measured from the newest counted note")
	assert.Equal(t, `g/r!12 — 2 new review threads (reviewer-x); author instruction: "/agent take option B, keep the old name"`, e.Text)
	assert.False(t, e.Info)

	info := reviewEvent(m, []glDiscussion{thread(n(5, reviewer, "no findings"))}, "", opts)
	assert.True(t, info.Info)

	keep := reviewEvent(m, ds, "8", opts)
	assert.Equal(t, "8", keep.Fingerprint, "nothing new keeps the key alive at its stored fingerprint")
	assert.Nil(t, reviewEvent(m, nil, "", opts))
}

// fakeGL puts a gl on PATH that answers each API path from a fixture file and records the calls.
type fakeGL struct {
	t   *testing.T
	dir string
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

func newFakeGL(t *testing.T) *fakeGL {
	t.Helper()

	dir := t.TempDir()
	script := `#!/bin/sh
while [ "$1" != GET ]; do shift; done
p=$(printf %s "$2" | tr -c 'A-Za-z0-9' '_')
echo "$2" >> "` + dir + `/calls"
if [ -f "` + dir + `/$p.json" ]; then cat "` + dir + `/$p.json"; else echo "404 Not Found: $2" >&2; exit 1; fi
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gl"), []byte(script), 0o700))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	return &fakeGL{t: t, dir: dir}
}

func (f *fakeGL) answer(path string, v any) {
	raw, err := json.Marshal(v)
	require.NoError(f.t, err)
	require.NoError(f.t, os.WriteFile(filepath.Join(f.dir, unsafe.ReplaceAllString(path, "_")+".json"), raw, 0o600))
}

func (f *fakeGL) calls() []string {
	raw, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	s := strings.TrimSpace(string(raw))

	if s == "" {
		return nil
	}

	return strings.Split(s, "\n")
}

func (f *fakeGL) reset() { _ = os.Remove(filepath.Join(f.dir, "calls")) }

func mrsPath() string {
	q := url.Values{"scope": {"all"}, "state": {"opened"}, "author_id": {"1"}, "per_page": {"100"},
		"with_merge_status_recheck": {"true"}}

	return "/merge_requests?" + q.Encode()
}

func mrJSON(sha, updated string, conflict bool, reviewers ...glUser) map[string]any {
	return map[string]any{"iid": 12, "project_id": 7, "web_url": "https://h/g/r/-/merge_requests/12", "sha": sha,
		"target_branch": "main", "has_conflicts": conflict, "updated_at": updated, "author": author,
		"reviewers": reviewers, "references": map[string]string{"full": "g/r!12"}}
}

const discPath = "/projects/7/merge_requests/12/discussions?per_page=100"

func glWatch(t *testing.T) *crew.Watch {
	t.Helper()

	w := &crew.Watch{Name: "mrs", Kind: crew.WatchGitLabMR, Every: "10m", GLConfig: "work"}
	require.NoError(t, w.Check(&crew.Crew{Dir: t.TempDir()}))

	return w
}

func runGL(w *crew.Watch, seen Seen, cache json.RawMessage) Result {
	return Run(context.Background(), w, Env{Seen: seen, Cache: cache})
}

func TestGitLabConflict(t *testing.T) {
	gl := newFakeGL(t)
	gl.answer("/user", author)
	gl.answer(discPath, []glDiscussion{})
	gl.answer("/projects/7/repository/branches/main", map[string]any{"commit": map[string]string{"id": "5e6f7a8b99"}})

	w := glWatch(t)
	st, q := NewState(), &queue{}
	c := Config{Quiet: 10 * time.Minute, ForgetAfter: time.Hour}

	gl.answer(mrsPath(), []any{mrJSON("1a2b3c4d55", "u1", true, reviewer)})
	res := runGL(w, st.Seen(), st.Cache)
	require.NoError(t, res.Err)
	Apply(st, res.Events, c, t0, q.add)
	st.Cache = res.Cache
	assert.Equal(t, []string{"g/r!12 — conflicts with main (source 1a2b3c4d, target 5e6f7a8b) https://h/g/r/-/merge_requests/12"}, q.lines)

	res = runGL(w, st.Seen(), st.Cache)
	Apply(st, res.Events, c, t0.Add(10*time.Minute), q.add)
	assert.Len(t, q.lines, 1, "once per (source, target) pair")

	gl.answer("/projects/7/repository/branches/main", map[string]any{"commit": map[string]string{"id": "9999aaaa00"}})
	res = runGL(w, st.Seen(), st.Cache)
	Apply(st, res.Events, c, t0.Add(20*time.Minute), q.add)
	assert.Len(t, q.lines, 2, "the target moved while the MR did not: news again")

	gl.answer(mrsPath(), []any{mrJSON("1a2b3c4d55", "u1", true)})
	res = runGL(w, Seen{}, nil)
	assert.Empty(t, res.Events, "no reviewer, no conflict note")
}

func TestGitLabConflictWaitsForPendingReview(t *testing.T) {
	gl := newFakeGL(t)
	gl.answer("/user", author)
	gl.answer(mrsPath(), []any{mrJSON("aaaa", "u1", true, reviewer)})
	gl.answer(discPath, []glDiscussion{thread(open(n(5, reviewer, "fix")))})
	gl.answer("/projects/7/repository/branches/main", map[string]any{"commit": map[string]string{"id": "bbbb"}})

	res := runGL(glWatch(t), Seen{}, nil)
	require.NoError(t, res.Err)
	require.Len(t, res.Events, 2)
	assert.Equal(t, res.Events[0].At, res.Events[1].At, "the conflict shares the pending review's moment")

	st, q := NewState(), &queue{}
	outs := Apply(st, res.Events, Config{Quiet: 10 * time.Minute}, t0.Add(6*time.Minute), q.add)
	assert.Equal(t, []string{"g/r!12:review=quiet 9m", "g/r!12:conflict=quiet 9m"}, fates(outs))
}

func TestGitLabCheapPath(t *testing.T) {
	gl := newFakeGL(t)
	gl.answer("/user", author)
	gl.answer(mrsPath(), []any{mrJSON("aaaa", "u1", true, reviewer)})
	gl.answer(discPath, []glDiscussion{thread(open(n(5, reviewer, "fix")))})
	gl.answer("/projects/7/repository/branches/main", map[string]any{"commit": map[string]string{"id": "bbbb"}})

	w := glWatch(t)
	first := runGL(w, Seen{}, nil)
	require.NoError(t, first.Err)

	gl.reset()

	again := runGL(w, Seen{}, first.Cache)
	require.NoError(t, again.Err)
	assert.Equal(t, first.Events, again.Events)
	assert.NotContains(t, strings.Join(gl.calls(), " "), "discussions", "unchanged updated_at: no discussions call")
	assert.Contains(t, gl.calls(), "/projects/7/repository/branches/main", "the conflict is still checked")

	gl.reset()
	runGL(w, Seen{"g/r!12:review": "5"}, first.Cache)
	assert.Contains(t, strings.Join(gl.calls(), " "), "discussions", "a new last_seen reads the threads again")
}

func TestGitLabFiltersAndFailures(t *testing.T) {
	gl := newFakeGL(t)
	gl.answer("/user", author)

	draft := mrJSON("a", "u", false, reviewer)
	draft["draft"] = true
	other := mrJSON("a", "u", false, reviewer)
	other["references"] = map[string]string{"full": "elsewhere/r!3"}
	gl.answer(mrsPath(), []any{draft, other})

	w := glWatch(t)
	w.Projects = []string{"g/"}
	res := runGL(w, Seen{}, nil)
	require.NoError(t, res.Err)
	assert.Empty(t, res.Events)
	assert.Equal(t, []string{"/user", mrsPath()}, gl.calls(), "drafts and other projects are never read")

	gl.reset()
	_ = os.Remove(filepath.Join(gl.dir, unsafe.ReplaceAllString("/user", "_")+".json"))
	res = runGL(w, Seen{}, nil)
	assert.ErrorContains(t, res.Err, "404 Not Found")
}
