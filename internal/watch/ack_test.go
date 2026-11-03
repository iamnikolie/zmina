package watch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var renotify = Config{ForgetAfter: 14 * 24 * time.Hour, Renotify: 2 * time.Hour}

func TestRenotifyWhileUnacked(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, renotify, t0, q.add)
	require.True(t, st.Keys["a"].Waiting(), "a noted key waits on an ack")
	assert.Equal(t, t0, st.Keys["a"].Since)

	outs := Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(time.Hour), q.add)
	assert.Equal(t, []string{"a=dup"}, fates(outs), "inside renotify it is a dup")

	outs = Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(2*time.Hour), q.add)
	assert.Equal(t, []string{"a=renote"}, fates(outs), "past renotify it is said again, not dropped")
	assert.Equal(t, t0, outs[0].Event.Since)
	assert.Equal(t, []string{"a at 1", "a at 1"}, q.lines)
	assert.Equal(t, 1, st.Keys["a"].Renotes)
	assert.Equal(t, t0, st.Keys["a"].Since, "still pending since the first note")
	assert.Equal(t, t0.Add(2*time.Hour), st.Keys["a"].NotedAt)

	outs = Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(3*time.Hour), q.add)
	assert.Equal(t, []string{"a=dup"}, fates(outs), "renotify counts from the last time it was said")

	outs = Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(4*time.Hour), q.add)
	assert.Equal(t, []string{"a=renote"}, fates(outs))
	assert.Equal(t, 2, st.Keys["a"].Renotes)
}

func TestRenotifyOff(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, cfg, t0, q.add)
	outs := Apply(st, []Event{ev("a", "1")}, cfg, t0.Add(48*time.Hour), q.add)
	assert.Equal(t, []string{"a=dup"}, fates(outs), "without renotify a dup stays a dup")
}

func TestAckStopsRenotify(t *testing.T) {
	st, q := NewState(), &queue{}
	a, b, c := ev("g/r!1:review", "1"), ev("g/r!1:pushed", "1"), ev("g/r!2:review", "1")
	a.URL, b.URL, c.URL = "https://example.com/g/r/-/merge_requests/1", "https://example.com/g/r/-/merge_requests/1",
		"https://example.com/g/r/-/merge_requests/2"

	Apply(st, []Event{a, b, c}, renotify, t0, q.add)
	assert.Equal(t, []string{"g/r!1:pushed", "g/r!1:review", "g/r!2:review"}, st.Waiting())

	assert.Equal(t, []string{"g/r!1:pushed", "g/r!1:review"}, st.Ack(a.URL, "dispatcher", t0.Add(time.Minute)),
		"a url acks every key its note carried")
	assert.Equal(t, []string{"g/r!2:review"}, st.Ack("g/r!2:review", "hand", t0.Add(time.Minute)))
	assert.Empty(t, st.Ack("g/r!2:review", "hand", t0.Add(time.Minute)), "an acked key no longer waits")
	assert.Equal(t, "dispatcher", st.Keys["g/r!1:review"].AckedBy)

	outs := Apply(st, []Event{a, b, c}, renotify, t0.Add(5*time.Hour), q.add)
	assert.Equal(t, []string{"g/r!1:review=dup", "g/r!1:pushed=dup", "g/r!2:review=dup"}, fates(outs))
	assert.Empty(t, st.Waiting())

	c.Fingerprint = "2"
	Apply(st, []Event{a, b, c}, renotify, t0.Add(6*time.Hour), q.add)
	assert.Equal(t, []string{"g/r!2:review"}, st.Waiting(), "a new fingerprint is news and waits again")
	assert.Equal(t, t0.Add(6*time.Hour), st.Keys["g/r!2:review"].Since)
}

func TestKeyNoLongerReportedClearsAndComesBackWaiting(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, renotify, t0, q.add)
	Apply(st, nil, renotify, t0.Add(time.Hour), q.add)
	assert.Empty(t, st.Waiting(), "a key the source stops reporting does not wait while it is gone")
	assert.Empty(t, st.Match("a"), "nor can it be acked")

	// One empty run (a glitch) does not end the wait: back with the same fingerprint it waits from the same Since.
	outs := Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(5*time.Hour), q.add)
	assert.Equal(t, []string{"a=renote"}, fates(outs))
	assert.Equal(t, []string{"a"}, st.Waiting())
	assert.Equal(t, t0, st.Keys["a"].Since)

	Apply(st, nil, renotify, t0.Add(6*time.Hour), q.add)
	Apply(st, nil, renotify, t0.Add(6*time.Hour+renotify.ForgetAfter), q.add)
	assert.NotContains(t, st.Keys, "a", "gone past forget_after: forgotten")
}

func TestRenotifyOffDoesNotWait(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, cfg, t0, q.add)
	assert.Empty(t, st.Waiting(), "renotify off: a noted key does not wait")
	assert.True(t, st.Keys["a"].Since.IsZero())
}

func TestKeyKeepsTheNoteAsQueued(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, renotify, t0, q.add)
	assert.Equal(t, "w: a at 1", st.Keys["a"].Queued)

	Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(3*time.Hour), func(e Event) (string, error) {
		return "w: again", nil
	})
	assert.Equal(t, "w: again", st.Keys["a"].Queued, "a renote keeps its own queued text")
}

func TestMatchAndAck(t *testing.T) {
	st, q := NewState(), &queue{}
	a := ev("a", "1")
	a.URL = "https://example.com/1"

	Apply(st, []Event{a, ev("b", "1")}, renotify, t0, q.add)
	assert.Equal(t, []string{"a"}, st.Match("https://example.com/1"))
	assert.Empty(t, st.Match("https://example.com/12"))
	assert.Equal(t, []string{"a", "b"}, st.Waiting(), "Match changes nothing")
}

func TestRenoteStillQueuedIsADup(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, renotify, t0, q.add)

	outs := Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(3*time.Hour), func(e Event) (string, error) {
		return "", ErrStillQueued
	})
	assert.Equal(t, []string{"a=dup"}, fates(outs), "an earlier note not typed yet is not said twice")
	assert.Equal(t, t0, st.Keys["a"].NotedAt)
	assert.Zero(t, st.Keys["a"].Renotes)

	q.full = true
	outs = Apply(st, []Event{ev("a", "1")}, renotify, t0.Add(3*time.Hour), q.add)
	assert.Equal(t, []string{"a=queue-full"}, fates(outs))
	assert.True(t, st.Keys["a"].Waiting(), "a full queue keeps it waiting for the next run")
}

func TestBaselineAndOldKeysDoNotWait(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, Config{ForgetAfter: time.Hour, Baseline: true, Renotify: time.Hour}, t0, q.add)
	assert.Empty(t, st.Waiting(), "a baseline told nobody")

	// A key noted by a zmina before acks has no Since: it is never said again.
	st.Keys["old"] = &Key{Fingerprint: "1", NotedAt: t0, FirstSeen: t0}
	outs := Apply(st, []Event{ev("a", "1"), ev("old", "1")}, renotify, t0.Add(5*time.Hour), q.add)
	assert.Equal(t, []string{"a=dup", "old=dup"}, fates(outs))
	assert.Empty(t, q.lines)
}

func TestRenotesFoldApartFromNews(t *testing.T) {
	st, q := NewState(), &queue{}
	fold := func(key, fp, text string) Event {
		return Event{Key: key, Fingerprint: fp, Text: "g/r!1 — " + text, Fold: "g/r!1", URL: "https://example.com/1"}
	}

	Apply(st, []Event{fold("g/r!1:answered", "1", "author answered 1 thread"),
		fold("g/r!1:pushed", "1", "pushed 1 commit")}, renotify, t0, q.add)
	assert.Equal(t, []string{"g/r!1 — author answered 1 thread, pushed 1 commit https://example.com/1"}, q.lines)

	outs := Apply(st, []Event{fold("g/r!1:answered", "1", "author answered 1 thread"),
		fold("g/r!1:pushed", "1", "pushed 1 commit"), fold("g/r!1:requested", "1", "review requested")},
		renotify, t0.Add(2*time.Hour), q.add)
	assert.Equal(t, []string{"g/r!1:answered=renote", "g/r!1:pushed=renote", "g/r!1:requested=note"}, fates(outs))
	assert.Equal(t, []string{"g/r!1 — author answered 1 thread, pushed 1 commit https://example.com/1",
		"g/r!1 — author answered 1 thread, pushed 1 commit https://example.com/1",
		"g/r!1 — review requested https://example.com/1"}, q.lines[:3])
	assert.Equal(t, "g/r!1 — author answered 1 thread, pushed 1 commit", st.Keys["g/r!1:pushed"].Text,
		"a key keeps the note it went out in")
}
