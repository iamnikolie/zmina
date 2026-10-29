package watch

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

type queue struct {
	lines []string
	full  bool
}

func (q *queue) add(e Event) error {
	if q.full {
		return errors.New("20 notes already wait")
	}

	q.lines = append(q.lines, e.Line())

	return nil
}

func ev(key, fp string) Event { return Event{Key: key, Fingerprint: fp, Text: key + " at " + fp} }

func fates(outs []Outcome) []string {
	var out []string
	for _, o := range outs {
		out = append(out, o.Event.Key+"="+o.String())
	}

	return out
}

var cfg = Config{ForgetAfter: 14 * 24 * time.Hour}

func TestDedupe(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, cfg, t0, q.add)
	assert.Equal(t, []string{"a at 1"}, q.lines)

	outs := Apply(st, []Event{ev("a", "1")}, cfg, t0.Add(time.Minute), q.add)
	assert.Equal(t, []string{"a=dup"}, fates(outs), "same fingerprint is not news")

	Apply(st, []Event{ev("a", "2")}, cfg, t0.Add(2*time.Minute), q.add)
	assert.Equal(t, []string{"a at 1", "a at 2"}, q.lines, "a changed fingerprint is")
}

func TestGoneKeyComesBack(t *testing.T) {
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, cfg, t0, q.add)
	Apply(st, nil, cfg, t0.Add(time.Hour), q.add)
	require.False(t, st.Keys["a"].GoneAt.IsZero(), "a successful run without the key marks it gone")

	outs := Apply(st, []Event{ev("a", "1")}, cfg, t0.Add(48*time.Hour), q.add)
	assert.Equal(t, []string{"a=dup"}, fates(outs), "back with the same fingerprint inside forget_after")
	assert.True(t, st.Keys["a"].GoneAt.IsZero())

	Apply(st, nil, cfg, t0.Add(49*time.Hour), q.add)
	Apply(st, nil, cfg, t0.Add(49*time.Hour+cfg.ForgetAfter), q.add)
	assert.NotContains(t, st.Keys, "a", "forgotten forget_after after it went")

	Apply(st, []Event{ev("a", "1")}, cfg, t0.Add(30*24*time.Hour), q.add)
	assert.Len(t, q.lines, 2, "once forgotten it is news again")
}

func TestQuiet(t *testing.T) {
	c := cfg
	c.Quiet = 10 * time.Minute
	st, q := NewState(), &queue{}

	at := t0.Add(-3 * time.Minute)
	e := ev("a", "1")
	e.At = &at

	outs := Apply(st, []Event{e}, c, t0, q.add)
	assert.Equal(t, []string{"a=quiet 7m"}, fates(outs))
	assert.Empty(t, q.lines)
	assert.NotContains(t, st.Keys, "a", "nothing stored for a held event")

	outs = Apply(st, []Event{e}, c, t0.Add(7*time.Minute), q.add)
	assert.Equal(t, []string{"a=note"}, fates(outs), "noted once the window passed")
	assert.Empty(t, st.Pending)
}

func TestQuietGoneBeforeWindow(t *testing.T) {
	c := cfg
	c.Quiet = 10 * time.Minute
	st, q := NewState(), &queue{}

	at := t0
	e := ev("a", "1")
	e.At = &at

	Apply(st, []Event{e}, c, t0.Add(time.Minute), q.add)
	outs := Apply(st, nil, c, t0.Add(5*time.Minute), q.add)

	require.Len(t, outs, 1)
	assert.Equal(t, Dropped, outs[0].Fate)
	assert.Equal(t, GoneBeforeQuiet, outs[0].Reason)
	assert.Empty(t, q.lines)
	assert.Empty(t, st.Pending)
}

func TestQuietSettledBackToStored(t *testing.T) {
	c := cfg
	c.Quiet = 10 * time.Minute
	st, q := NewState(), &queue{}

	Apply(st, []Event{ev("a", "1")}, c, t0, q.add)

	at := t0.Add(time.Minute)
	moved := ev("a", "2")
	moved.At = &at
	Apply(st, []Event{moved}, c, t0.Add(2*time.Minute), q.add)

	outs := Apply(st, []Event{ev("a", "1")}, c, t0.Add(4*time.Minute), q.add)
	assert.Equal(t, []string{"a=dup", "a=dropped (gone-before-quiet)"}, fates(outs))
	assert.Len(t, q.lines, 1)
}

func TestBaseline(t *testing.T) {
	c := cfg
	c.Baseline = true
	st, q := NewState(), &queue{}

	outs := Apply(st, []Event{ev("a", "1"), ev("b", "1")}, c, t0, q.add)
	assert.Equal(t, []string{"a=baseline", "b=baseline"}, fates(outs))
	assert.Empty(t, q.lines, "a fresh watch does not flood the dispatcher")

	Apply(st, []Event{ev("a", "1"), ev("b", "2")}, c, t0.Add(time.Minute), q.add)
	assert.Equal(t, []string{"b at 2"}, q.lines)

	st, q = NewState(), &queue{}
	c.Baseline = false
	Apply(st, []Event{ev("a", "1")}, c, t0, q.add)
	assert.Equal(t, []string{"a at 1"}, q.lines, "baseline: false notes on the first run")
}

func TestQueueFullRetries(t *testing.T) {
	st, q := NewState(), &queue{full: true}

	outs := Apply(st, []Event{ev("a", "1")}, cfg, t0, q.add)
	assert.Equal(t, QueueFull, outs[0].Fate)
	assert.NotContains(t, st.Keys, "a", "not stored while the queue is full")

	q.full = false
	Apply(st, []Event{ev("a", "1")}, cfg, t0.Add(time.Minute), q.add)
	assert.Equal(t, []string{"a at 1"}, q.lines, "retried on the next run")
}

func TestInfoOff(t *testing.T) {
	st, q := NewState(), &queue{}
	e := ev("a", "1")
	e.Info = true

	outs := Apply(st, []Event{e}, cfg, t0, q.add)
	assert.Equal(t, []string{"a=info-off"}, fates(outs))
	assert.NotContains(t, st.Keys, "a")

	c := cfg
	c.Info = true
	Apply(st, []Event{e}, c, t0, q.add)
	assert.Len(t, q.lines, 1)
}

func TestForgetAndClone(t *testing.T) {
	st, q := NewState(), &queue{}
	Apply(st, []Event{ev("a", "1"), ev("b", "1")}, cfg, t0, q.add)

	cp := st.Clone()
	Apply(cp, []Event{ev("a", "2")}, cfg, t0, q.add)
	assert.Equal(t, "1", st.Keys["a"].Fingerprint, "a clone leaves the state alone")

	assert.Equal(t, 1, st.Forget("a"))
	assert.Equal(t, Seen{"b": "1"}, st.Seen())
	assert.Equal(t, 1, st.Forget(""))
	assert.Empty(t, st.Keys)
}

func TestURLAppended(t *testing.T) {
	e := Event{Text: "g/r!12 — 4 new review threads (x)", URL: "https://h/g/r/-/merge_requests/12"}
	assert.Equal(t, "g/r!12 — 4 new review threads (x) https://h/g/r/-/merge_requests/12", e.Line())
}
