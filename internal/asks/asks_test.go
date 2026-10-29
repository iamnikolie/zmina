package asks

import (
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
