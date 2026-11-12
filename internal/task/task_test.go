package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/record"
)

func TestStore(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	a, err := s.Add(Task{Role: "dev", Asked: "fix the login"}, now)
	require.NoError(t, err)
	assert.Equal(t, "T-1", a.ID)
	assert.Equal(t, Open, a.State)

	for range 9 {
		_, err = s.Add(Task{Role: "designer", Asked: "x"}, now)
		require.NoError(t, err)
	}

	all, err := s.All()
	require.NoError(t, err)
	require.Len(t, all, 10)
	assert.Equal(t, "T-10", all[9].ID, "ordered by number, not by name")

	got, err := s.Get("t-1")
	require.NoError(t, err)
	assert.Equal(t, "fix the login", got.Asked)

	got.State = Done
	require.NoError(t, s.Put(got))
	assert.Len(t, s.Active(), 9)

	_, err = s.Get("99")
	assert.Error(t, err)

	_, err = ParseState("finished")
	assert.Error(t, err)
}

func TestLegacyWaitingIsWaiting(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	a, err := s.Add(Task{Role: "dev", Asked: "x"}, now)
	require.NoError(t, err)

	a.State = "waiting-alex"
	require.NoError(t, s.Put(a))

	got, err := s.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, Waiting, got.State, "a file written under the old name reads as waiting")
	assert.Len(t, s.Active(), 1)

	st, err := ParseState("waiting-alex")
	require.NoError(t, err)
	assert.Equal(t, Waiting, st)
}

func TestDamagedFileIsNeverWrittenOver(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	for range 2 {
		_, err := s.Add(Task{Role: "dev", Asked: "x"}, now)
		require.NoError(t, err)
	}

	broken := filepath.Join(s.Dir, "T-3.json")
	require.NoError(t, os.WriteFile(broken, []byte(`{"id": "T-3", "state": `), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-4.json"), []byte(`{"id": "T-9", "state": "open"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-5.json"), []byte(`{"id": "T-5", "state": "paused"}`), 0o600))

	next, err := s.Add(Task{Role: "dev", Asked: "y"}, now)
	require.NoError(t, err)
	assert.Equal(t, "T-6", next.ID, "numbered above every file, readable or not")

	raw, _ := os.ReadFile(broken)
	assert.Equal(t, `{"id": "T-3", "state": `, string(raw), "untouched")

	tasks, problems, err := s.Scan()
	require.NoError(t, err)
	require.Len(t, problems, 3)
	assert.Contains(t, problems[0], "T-3")
	assert.Contains(t, problems[1], `the file says it is "T-9"`)
	assert.Contains(t, problems[2], `unknown state "paused"`)

	var ids []string
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}

	assert.Equal(t, []string{"T-1", "T-2", "T-5", "T-6"}, ids)
	assert.Len(t, s.Active(), 4, "an unknown state holds its role")

	require.ErrorIs(t, s.write(Task{ID: "T-6"}, true), ErrExists)
}

func TestHeadlineAndWhere(t *testing.T) {
	tk := Task{Asked: strings.Repeat("rework the landing page ", 10), Repo: "/src/kreida"}
	assert.Len(t, []rune(tk.Headline()), titleMax, "the asked text, cut")
	assert.True(t, strings.HasSuffix(tk.Headline(), "…"))
	assert.Equal(t, "kreida", tk.Where(), "the repo's directory name")

	tk.Title, tk.Project = "Лендинг kreida — переделка", "kreida-cloud"
	assert.Equal(t, "Лендинг kreida — переделка", tk.Headline())
	assert.Equal(t, "kreida-cloud", tk.Where())
	assert.Empty(t, Task{}.Where())
}

func TestRouteIsOptionalInTheRecord(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	plain, err := s.Add(Task{Role: "dev", Asked: "a plain task"}, now)
	require.NoError(t, err)

	routed, err := s.Add(Task{Role: "dev", Asked: "a routed task", Route: &Route{Rule: 0, Name: "other crew", Why: "repo under acme-lg"}}, now)
	require.NoError(t, err)

	byHand, err := s.Add(Task{Role: "dev3", Asked: "a hand pick", Route: &Route{Rule: 6, Name: "pool by limits", Why: "w", Said: "dev2", Override: "alex named it"}}, now)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(s.Dir, plain.ID+".json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "route", "no routes, no key")

	raw, err = os.ReadFile(filepath.Join(s.Dir, routed.ID+".json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"rule": 0`, "rule 0 is a rule")
	assert.NotContains(t, string(raw), "said")
	assert.NotContains(t, string(raw), "override")

	got, err := s.Get(byHand.ID)
	require.NoError(t, err)
	assert.Equal(t, &Route{Rule: 6, Name: "pool by limits", Why: "w", Said: "dev2", Override: "alex named it"}, got.Route)

	got, err = s.Get(plain.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Route)

	// A task file written before the field existed reads, and keeps no route when it is written again.
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-9.json"),
		[]byte(`{"id":"T-9","role":"dev","asked":"old","state":"open","created":"2026-10-01T10:00:00Z","updated":"2026-10-01T10:00:00Z"}`), 0o600))

	old, err := s.Get("T-9")
	require.NoError(t, err)
	assert.Nil(t, old.Route)
	require.NoError(t, s.Put(old))

	raw, err = os.ReadFile(filepath.Join(s.Dir, "T-9.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "route")
}

// A task file written before criteria existed reads as it did; one with criteria keeps them in order, and a task
// without any leaves the key out of its file.
func TestCriteriaRoundTripAndOldFiles(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	old := `{"id":"T-1","role":"dev","asked":"fix the login","state":"open","created":"2026-10-26T12:00:00Z","updated":"2026-10-26T12:00:00Z"}`
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-1.json"), []byte(old), 0o600))

	got, err := s.Get("T-1")
	require.NoError(t, err)
	assert.Empty(t, got.Criteria)
	assert.Equal(t, "fix the login", got.Asked)

	require.NoError(t, s.Put(got))
	raw, err := os.ReadFile(filepath.Join(s.Dir, "T-1.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "criteria", "a task without criteria writes no key")

	added, err := s.Add(Task{Role: "dev", Asked: "x", Criteria: []string{"tests pass", "the login form shows an error, not a blank page"}}, now)
	require.NoError(t, err)

	raw, err = os.ReadFile(filepath.Join(s.Dir, added.ID+".json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"criteria": [`)

	got, err = s.Get(added.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"tests pass", "the login form shows an error, not a blank page"}, got.Criteria)
}

func TestCleanCriteria(t *testing.T) {
	got, err := CleanCriteria([]string{"  one  ", "two"})
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two"}, got)

	got, err = CleanCriteria(nil)
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = CleanCriteria([]string{"one", " "})
	assert.ErrorContains(t, err, "not empty")

	_, err = CleanCriteria([]string{"first line\nsecond line"})
	assert.ErrorContains(t, err, "one line")
}

// A task file written before checks existed reads as it did, and writes back without a check; one with a check and a
// closure that records it reads back whole.
func TestCheckFieldsAreOptionalInTheFile(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	require.NoError(t, os.MkdirAll(s.Dir, 0o700))

	old := `{"id": "T-1", "role": "dev", "asked": "x", "state": "done", "created": "2026-10-26T12:00:00Z",
  "updated": "2026-10-26T12:00:00Z", "closed": {"by": "dev", "at": "2026-10-26T12:00:00Z", "state": "done"}}`
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-1.json"), []byte(old), 0o600))

	got, err := s.Get("T-1")
	require.NoError(t, err)
	assert.Nil(t, got.Check)
	assert.Nil(t, got.Closed.Check)
	assert.Nil(t, got.Closed.Unchecked)

	require.NoError(t, s.Put(got))

	raw, err := os.ReadFile(filepath.Join(s.Dir, "T-1.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"check"`)
	assert.NotContains(t, string(raw), `"unchecked"`)

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	got.Check = &donecheck.Spec{Run: "make test", Timeout: "5m", Commit: true}
	got.Closed.Check = &donecheck.Result{Run: "make test", Source: donecheck.FromTask, Commit: "abc123", At: now,
		DurationMS: 1500, Output: "ok"}
	got.Closed.Unchecked = &Unchecked{By: "hand", Run: "make test"}
	require.NoError(t, s.Put(got))

	again, err := s.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

// A task file written before hand-outs were counted reads with generation 0 and writes back without the keys; one with a
// generation, a closure under it and late reports reads back whole.
func TestGenerationFieldsAreOptionalInTheFile(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	require.NoError(t, os.MkdirAll(s.Dir, 0o700))

	old := `{"id": "T-1", "role": "dev", "asked": "x", "state": "done", "created": "2026-10-26T12:00:00Z",
  "updated": "2026-10-26T12:00:00Z", "result": {"revision": "ab", "at": "2026-10-26T12:00:00Z"},
  "delivery": {"id": "pkt", "role": "dev", "runtime": "claude", "home": "/h", "line": "l", "task_updated": "2026-10-26T12:00:00Z",
  "queued_at": "2026-10-26T12:00:00Z"}, "closed": {"by": "dev", "at": "2026-10-26T12:00:00Z", "state": "done"}}`
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-1.json"), []byte(old), 0o600))

	got, err := s.Get("T-1")
	require.NoError(t, err)
	assert.Zero(t, got.Delivery.Gen)
	assert.Zero(t, got.Result.Gen)
	assert.Zero(t, got.Closed.Gen)
	assert.Empty(t, got.Late)

	require.NoError(t, s.Put(got))

	raw, err := os.ReadFile(filepath.Join(s.Dir, "T-1.json"))
	require.NoError(t, err)

	for _, key := range []string{`"gen"`, `"unbound"`, `"late"`} {
		assert.NotContains(t, string(raw), key)
	}

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	got.Delivery.Gen, got.Result.Gen, got.Closed.Gen, got.Closed.Unbound = 3, 3, 3, true
	got.Late = []Late{{At: now, What: "result", Revision: "cd", By: "dev", Session: "sess-a", Gen: 3}}
	require.NoError(t, s.Put(got))

	again, err := s.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

// The next hand-out is one over the last delivery, and over any record or closing a delivery that was put back had
// made: a number is not given to two sessions.
func TestNextGenIsNeverAGivenOne(t *testing.T) {
	assert.Equal(t, 1, Task{}.NextGen(), "a task never handed out")
	assert.Equal(t, 4, Task{Delivery: &Delivery{Gen: 3}}.NextGen())
	assert.Equal(t, 6, Task{Delivery: &Delivery{Gen: 3}, Result: &record.Ref{Gen: 5}}.NextGen())
	assert.Equal(t, 7, Task{Delivery: &Delivery{Gen: 3}, Checkpoint: &record.Ref{Gen: 6}}.NextGen())
	assert.Equal(t, 8, Task{Closed: &Closure{Gen: 7}}.NextGen())
}

func TestLateReportsAreTheSameOnlyForTheSameCallerOnTheSameHandOut(t *testing.T) {
	l := Late{What: "result", Revision: "ab", By: "dev", Session: "sess-a", Gen: 2}

	assert.True(t, l.Same(l))

	same := l
	same.At = time.Now()
	assert.True(t, l.Same(same), "the time is no part of it")

	for _, o := range []Late{
		{What: "checkpoint", Revision: "ab", By: "dev", Session: "sess-a", Gen: 2},
		{What: "result", Revision: "cd", By: "dev", Session: "sess-a", Gen: 2},
		{What: "result", Revision: "ab", By: "review", Session: "sess-a", Gen: 2},
		{What: "result", Revision: "ab", By: "dev", Session: "sess-b", Gen: 2},
		{What: "result", Revision: "ab", By: "dev", Session: "sess-a", Gen: 3},
	} {
		assert.False(t, l.Same(o), "%+v", o)
	}
}
