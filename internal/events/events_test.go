package events

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLog(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "events.jsonl")}

	evs, err := l.Read(Filter{})
	require.NoError(t, err)
	assert.Empty(t, evs, "no file, no events")

	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	for i, role := range []string{"dev", "designer", "dev"} {
		require.NoError(t, l.Add(Event{TS: t0.Add(time.Duration(i) * time.Hour), Role: role, Event: "up", Task: "T-1"}))
	}

	f, _ := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("not json\n")
	_ = f.Close()

	evs, _ = l.Read(Filter{Role: "dev"})
	assert.Len(t, evs, 2)

	evs, _ = l.Read(Filter{Since: t0.Add(90 * time.Minute)})
	assert.Len(t, evs, 1)

	evs, _ = l.Read(Filter{Last: 2})
	require.Len(t, evs, 2)
	assert.Equal(t, "designer", evs[0].Role)
}

func TestLogRotates(t *testing.T) {
	old := MaxSize
	MaxSize = 300

	t.Cleanup(func() { MaxSize = old })

	l := &Log{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)

	for i := range 10 {
		require.NoError(t, l.Add(Event{TS: t0.Add(time.Duration(i) * time.Minute), Role: "dev", Event: "up"}))
	}

	_, err := os.Stat(l.Path + ".1")
	require.NoError(t, err, "rotated")

	evs, err := l.Read(Filter{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(evs), 3, "both files are read")
	assert.True(t, evs[len(evs)-1].TS.Equal(t0.Add(9*time.Minute)), "newest last")

	for i := 1; i < len(evs); i++ {
		assert.True(t, evs[i].TS.After(evs[i-1].TS), "oldest first across the two files")
	}
}
