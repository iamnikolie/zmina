package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clockSessions(t *testing.T) (*uiSessions, *uiClock) {
	t.Helper()

	clock := newUIClock()

	return newUISessions(filepath.Join(t.TempDir(), "ui-sessions.json"), clock.now), clock
}

// TestSessionIsKeptHashedAndPrivate: the file holds the SHA-256 of the id, never the id; only the owner reads it.
func TestSessionIsKeptHashedAndPrivate(t *testing.T) {
	s, _ := clockSessions(t)

	id, err := s.start()
	require.NoError(t, err)
	assert.Len(t, id, 64, "256 bits")

	other, err := s.start()
	require.NoError(t, err)
	assert.NotEqual(t, id, other)

	raw, err := os.ReadFile(s.path)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), id)
	assert.Contains(t, string(raw), hashSessionID(id))

	info, err := os.Stat(s.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	assert.True(t, s.valid(id))
	assert.False(t, s.valid(id+"0"))
	assert.False(t, s.valid(hashSessionID(id)), "the hash in the file is no id")
	assert.False(t, s.valid(""))
}

// TestSessionEndsIdleAfterThirtyDaysAndAtTheLatestAfterNinety: seen again, it slides; never past ninety days.
func TestSessionEndsIdleAfterThirtyDaysAndAtTheLatestAfterNinety(t *testing.T) {
	s, clock := clockSessions(t)

	idle, err := s.start()
	require.NoError(t, err)

	busy, err := s.start()
	require.NoError(t, err)

	for day := 1; day <= 29; day++ {
		clock.add(24 * time.Hour)
		assert.True(t, s.valid(busy), "day %d", day)
	}

	clock.add(24 * time.Hour)
	assert.False(t, s.valid(idle), "thirty days without being seen")
	assert.True(t, s.valid(busy), "seen yesterday")

	// Used every day, it still ends ninety days after it began.
	for day := 31; day < 90; day++ {
		clock.add(24 * time.Hour)
		require.True(t, s.valid(busy), "day %d", day)
	}

	clock.add(24 * time.Hour)
	assert.False(t, s.valid(busy), "the hard cap")

	// An ended session is gone from the file at the next write.
	_, err = s.start()
	require.NoError(t, err)
	list, err := readUISessions(s.path)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// TestSessionSightingIsWrittenOnceAMinute: the file is not rewritten on every request.
func TestSessionSightingIsWrittenOnceAMinute(t *testing.T) {
	s, clock := clockSessions(t)

	id, err := s.start()
	require.NoError(t, err)

	seen := func() time.Time {
		list, err := readUISessions(s.path)
		require.NoError(t, err)
		require.Len(t, list, 1)

		return list[0].LastSeen
	}

	began := seen()

	clock.add(30 * time.Second)
	assert.True(t, s.valid(id))
	assert.Equal(t, began, seen(), "within a minute the file stays")

	clock.add(31 * time.Second)
	assert.True(t, s.valid(id))
	assert.Equal(t, clock.now(), seen())
}

// TestSessionEndedFromAnotherProcessIsNotValid: zmina ui sessions edits the file; the running cockpit sees it.
func TestSessionEndedFromAnotherProcessIsNotValid(t *testing.T) {
	s, clock := clockSessions(t)

	keep, err := s.start()
	require.NoError(t, err)
	end, err := s.start()
	require.NoError(t, err)
	require.True(t, s.valid(end))

	cli := newUISessions(s.path, clock.now)
	require.NoError(t, cli.revoke(hashSessionID(end)[:8]))

	assert.False(t, s.valid(end), "revoked")
	assert.False(t, s.standing(end))
	assert.True(t, s.valid(keep))

	// A sighting due after the revoke does not bring it back.
	clock.add(2 * time.Minute)
	assert.False(t, s.valid(end))
	assert.True(t, s.valid(keep))

	n, err := cli.revokeAll()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, s.valid(keep))
}

func TestRevokeNamesOneSession(t *testing.T) {
	s, clock := clockSessions(t)

	now := clock.now()
	file := uiSessionFile{Sessions: []uiSession{
		{Hash: "aaaa1111" + strings.Repeat("0", 56), Created: now, LastSeen: now, Expires: now.Add(uiSessionIdle)},
		{Hash: "aaaa2222" + strings.Repeat("0", 56), Created: now, LastSeen: now, Expires: now.Add(uiSessionIdle)},
		{Hash: "bbbb3333" + strings.Repeat("0", 56), Created: now, LastSeen: now, Expires: now.Add(uiSessionIdle)},
	}}
	raw, err := json.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(s.path, raw, 0o600))

	err = s.revoke("aaaa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aaaa1111")
	assert.Contains(t, err.Error(), "aaaa2222")

	require.Error(t, s.revoke("cccc"))
	require.Error(t, s.revoke("aa"), "too short to name one")

	require.NoError(t, s.revoke("AAAA1111"))

	list, err := s.all()
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "aaaa2222", list[0].short())
}

func TestSessionsFileIsBounded(t *testing.T) {
	s, clock := clockSessions(t)

	var first string

	for i := range uiSessionsKept + 5 {
		id, err := s.start()
		require.NoError(t, err)

		if i == 0 {
			first = id
		}

		clock.add(time.Second)
	}

	list, err := s.all()
	require.NoError(t, err)
	assert.Len(t, list, uiSessionsKept)
	assert.False(t, s.valid(first), "the one seen longest ago goes")
}

func TestSessionsFileThatDoesNotParseLetsNobodyIn(t *testing.T) {
	s, _ := clockSessions(t)

	require.NoError(t, os.WriteFile(s.path, []byte("{not json"), 0o600))
	assert.False(t, s.valid("anything"))

	id, err := s.start()
	require.NoError(t, err, "a new session replaces the broken file")
	assert.True(t, s.valid(id))
}

// TestUISessionsCommand: zmina ui sessions lists with the short id, ends one or all.
func TestUISessionsCommand(t *testing.T) {
	uiHome(t)

	run := func(args ...string) (string, error) {
		var out bytes.Buffer

		root := NewRoot(&out, &out)
		root.SetArgs(append([]string{"ui", "sessions"}, args...))
		err := root.Execute()

		return out.String(), err
	}

	out, err := run()
	require.NoError(t, err)
	assert.Contains(t, out, "no browser is signed in")

	s := newUISessions(uiSessionsPath(), time.Now)
	first, err := s.start()
	require.NoError(t, err)
	second, err := s.start()
	require.NoError(t, err)

	out, err = run()
	require.NoError(t, err)
	assert.Contains(t, out, "ID")
	assert.Contains(t, out, "LAST SEEN")
	assert.Contains(t, out, hashSessionID(first)[:8])
	assert.Contains(t, out, hashSessionID(second)[:8])
	assert.NotContains(t, out, first, "never an id, only the short hash")

	_, err = run("--revoke", hashSessionID(first)[:8], "--all")
	require.Error(t, err)

	out, err = run("--revoke", hashSessionID(first)[:8])
	require.NoError(t, err)
	assert.Contains(t, out, "ended "+hashSessionID(first)[:8])
	assert.False(t, newUISessions(uiSessionsPath(), time.Now).valid(first))
	assert.True(t, newUISessions(uiSessionsPath(), time.Now).valid(second))

	_, err = run("--revoke", "ffffffff")
	require.Error(t, err)

	out, err = run("--all")
	require.NoError(t, err)
	assert.Contains(t, out, "ended 1 sessions")
	assert.False(t, newUISessions(uiSessionsPath(), time.Now).valid(second))
}
