package lean

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lineCount(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}

	require.NoError(t, err)

	n := 0

	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}

	return n
}

// The lines from before the cutoff go to the archive file, appended; the newer ones and the lines that do not parse
// stay; a second run moves nothing and rewrites nothing.
func TestArchiveOlderMovesOnlyOldRecords(t *testing.T) {
	dir := t.TempDir()
	meter, archive := filepath.Join(dir, "reads.jsonl"), filepath.Join(dir, "archive", "reads.jsonl")
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)

	for _, at := range []time.Time{now.Add(-90 * 24 * time.Hour), now.Add(-40 * 24 * time.Hour), now.Add(-2 * 24 * time.Hour), now} {
		Append(meter, Record{At: at, Role: "dev", Kind: "read", Path: "a.go"})
	}

	f, err := os.OpenFile(meter, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("not json\n{\"kind\":\"read\"}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	older, total, err := CountOlder(meter, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 2, older)
	assert.Equal(t, 6, total)

	moved, err := ArchiveOlder(meter, archive, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 2, moved)
	assert.Equal(t, 2, lineCount(t, archive))
	assert.Equal(t, 4, lineCount(t, meter), "two recent records and the two lines that are no record of a time")

	raw, err := os.ReadFile(meter)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "not json", "what does not parse is kept")

	before, err := os.Stat(meter)
	require.NoError(t, err)

	moved, err = ArchiveOlder(meter, archive, cutoff)
	require.NoError(t, err)
	assert.Zero(t, moved)

	after, err := os.Stat(meter)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "nothing old: the file is not rewritten")

	moved, err = ArchiveOlder(filepath.Join(dir, "none.jsonl"), archive, cutoff)
	require.NoError(t, err)
	assert.Zero(t, moved, "no meter file is no lines")

	// The later run appends to the archive: the first batch is still there.
	Append(meter, Record{At: now.Add(-50 * 24 * time.Hour), Kind: "read"})

	moved, err = ArchiveOlder(meter, archive, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, moved)
	assert.Equal(t, 3, lineCount(t, archive))
}

// Calls metered while old lines are archived are not lost: the rewrite and the appends take turns.
func TestMeteredCallsSurviveTheArchiving(t *testing.T) {
	dir := t.TempDir()
	meter, archive := filepath.Join(dir, "reads.jsonl"), filepath.Join(dir, "archive", "reads.jsonl")
	now := time.Now().UTC()

	for range 50 {
		Append(meter, Record{At: now.Add(-90 * 24 * time.Hour), Kind: "read"})
	}

	const writers, each = 4, 100

	var wg sync.WaitGroup

	for range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range each {
				Append(meter, Record{At: now, Kind: "read", Path: "b.go"})
			}
		}()
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}

		_, err := ArchiveOlder(meter, archive, now.Add(-30*24*time.Hour))
		require.NoError(t, err)
	}

	assert.Equal(t, 50, lineCount(t, archive))
	assert.Equal(t, writers*each, lineCount(t, meter), "every metered call is still there")

	raw, err := os.ReadFile(meter)
	require.NoError(t, err)

	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r Record
		require.NoError(t, json.Unmarshal([]byte(l), &r), "no half lines: %s", l)
	}
}
