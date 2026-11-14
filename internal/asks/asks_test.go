package asks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsks(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "asks")}
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)

	a, err := s.Add(Ask{Kind: Decision, Text: "rename the flag?", Rec: "keep it", Blocks: "dev"}, now)
	require.NoError(t, err)
	assert.Equal(t, "A-1", a.ID)
	assert.Equal(t, Open, a.State)

	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "A-7.json"), []byte("{broken"), 0o600))

	b, err := s.Add(Ask{Kind: Check, Text: "sign in"}, now)
	require.NoError(t, err)
	assert.Equal(t, "A-8", b.ID, "numbered above every file, readable or not")

	got, err := s.Close("1", Answered, "rename it to --strict", now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, Answered, got.State)
	assert.Equal(t, "rename it to --strict", got.Answer)

	_, err = s.Close("a-1", Yes, "", now)
	assert.ErrorContains(t, err, "already answered")

	pending := s.Pending()
	require.Len(t, pending, 1)
	assert.Equal(t, "A-8", pending[0].ID)

	_, problems, err := s.Scan()
	require.NoError(t, err)
	assert.Len(t, problems, 1, "the damaged file is named, the rest read")
}

func TestParseKind(t *testing.T) {
	k, err := ParseKind("", "yes")
	require.NoError(t, err)
	assert.Equal(t, Decision, k)

	k, err = ParseKind("", "")
	require.NoError(t, err)
	assert.Equal(t, Check, k)

	_, err = ParseKind("vote", "")
	assert.Error(t, err)
}

// Decided is named, never read from a recommendation; its reason and what was not done are kept in the ask's file, and a
// file written before the field reads as it did.
func TestDecidedKind(t *testing.T) {
	k, err := ParseKind("decided", "")
	require.NoError(t, err)
	assert.Equal(t, Decided, k)

	k, err = ParseKind("decided", "yes")
	require.NoError(t, err)
	assert.Equal(t, Decided, k, "named, so it stays what it is named")

	k, err = ParseKind("", "yes")
	require.NoError(t, err)
	assert.Equal(t, Decision, k, "a recommendation makes a decision, never a decided item")

	k, err = ParseKind("", "")
	require.NoError(t, err)
	assert.Equal(t, Check, k)

	_, err = ParseKind("Decided", "")
	assert.Error(t, err)

	s := &Store{Dir: filepath.Join(t.TempDir(), "asks")}
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)

	a, err := s.Add(Ask{Kind: Decided, Text: "keep the old flag name", Reason: "links point at it", Instead: "rename it",
		Role: "dev"}, now)
	require.NoError(t, err)

	got, err := s.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, Decided, got.Kind)
	assert.Equal(t, "links point at it", got.Reason)
	assert.Equal(t, "rename it", got.Instead)
	assert.True(t, got.Open())

	raw, err := os.ReadFile(filepath.Join(s.Dir, "A-1.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"instead": "rename it"`)

	old := `{"id":"A-2","kind":"decision","text":"rename?","rec":"no","state":"open","created":"2026-10-01T10:00:00Z","updated":"2026-10-01T10:00:00Z"}`
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "A-2.json"), []byte(old), 0o600))

	got, err = s.Get("A-2")
	require.NoError(t, err)
	assert.Empty(t, got.Instead)
	assert.Equal(t, "no", got.Rec)

	raw, err = json.Marshal(Ask{ID: "A-3", Kind: Check})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "instead", "omitted when empty")
}
