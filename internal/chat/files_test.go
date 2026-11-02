package chat

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesAreImmutableRoleScopedAndKeepOriginalBytes(t *testing.T) {
	store := FileStore{Dir: t.TempDir(), Role: "dev"}
	file := Upload{ID: "abcdefab-1234-1234-1234-123456789abc", Name: "example.pdf", Data: []byte("%PDF-1.7\x00\xff\n")}
	first, err := store.Put(file)
	require.NoError(t, err)
	repeated, err := store.Put(file)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	meta, path, err := store.Get(file.ID)
	require.NoError(t, err)
	require.Equal(t, first, meta)
	bytes, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, file.Data, bytes)
	file.Data = []byte("changed")
	_, err = store.Put(file)
	require.ErrorContains(t, err, "different content")
	_, _, err = (FileStore{Dir: store.Dir, Role: "other"}).Get(file.ID)
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "outside"), path))
	_, _, err = store.Get(file.ID)
	require.Error(t, err)
}

func TestFilesRefuseTraversalControlCharactersAndOversize(t *testing.T) {
	store := FileStore{Dir: t.TempDir(), Role: "dev"}
	for _, name := range []string{"../secret", "/secret", "..", "a\\b", "a\nb", strings.Repeat("x", 241)} {
		_, err := store.Put(Upload{ID: "abcdefab-1234-1234-1234-123456789abc", Name: name})
		require.Error(t, err, name)
	}
	_, err := store.Put(Upload{ID: "../../escape", Name: "plain.txt"})
	require.Error(t, err)
	_, err = store.Put(Upload{ID: "abcdefab-1234-1234-1234-123456789abc", Name: "big.bin", Data: make([]byte, MaxFileBytes+1)})
	require.Error(t, err)
}
