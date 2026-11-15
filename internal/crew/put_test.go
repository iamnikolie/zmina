package crew

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pushedFile = `name: acme
namespace: /ns
home: alpha
nodes:
  alpha: {prefix: al-}
  beta: {prefix: be-}
roles:
  dispatcher: {}
  dev: {place: beta}
`

// putDir is a crew's directory with a file in it, and the path of the file.
func putDir(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path := filepath.Join(t.TempDir(), "acme", "crew.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))

	if body != "" {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}

	return path
}

func opts(node, base string) PutOptions {
	return PutOptions{Node: node, Base: base, By: "mesh alpha", How: "crew push"}
}

func noTemp(t *testing.T, path string) {
	t.Helper()

	left, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".crew-push-*"))
	require.NoError(t, err)
	assert.Empty(t, left, "no temp file of a push is left")
}

// A push replaces the file whose hash is the base, keeps what it replaced, and records who sent it and how.
func TestPutReplacesTheFileItWasBasedOn(t *testing.T) {
	old := "name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n"
	path := putDir(t, old)

	res, err := Put(path, []byte(pushedFile), opts("beta", SumOf([]byte(old))))
	require.NoError(t, err)
	assert.True(t, res.Wrote)
	assert.Equal(t, SumOf([]byte(pushedFile)), res.Sum)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, pushedFile, string(got))

	vs, err := HistoryOf(path).List()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(vs), 2)
	assert.Equal(t, "mesh alpha", vs[0].By)
	assert.Equal(t, "crew push", vs[0].How)

	prev, err := HistoryOf(path).Bytes(vs[1])
	require.NoError(t, err)
	assert.Equal(t, old, string(prev), "the file it replaced is kept")

	noTemp(t, path)

	// And it loads for the node that took it: beta's overlay is merged.
	c, err := LoadFor(path, "beta")
	require.NoError(t, err)
	assert.Equal(t, "be-", c.Prefix)
}

// A file that is not the one the push was based on is not replaced, and the refusal carries what is there.
func TestPutRefusesAFileThatIsNotTheBase(t *testing.T) {
	there := "name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n  ops: {}\n"
	path := putDir(t, there)

	for name, base := range map[string]string{"another file": SumOf([]byte("something else")), "no base": ""} {
		_, err := Put(path, []byte(pushedFile), opts("beta", base))

		var be *BaseError
		require.ErrorAs(t, err, &be, name)
		assert.Equal(t, SumOf([]byte(there)), be.Have, name)
		assert.Equal(t, there, string(be.Current), name)

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, there, string(got), name+": the file there is as it was")
	}

	noTemp(t, path)
}

func TestPutOfTheFileThatIsThereWritesNothing(t *testing.T) {
	path := putDir(t, pushedFile)

	before, _ := HistoryOf(path).List()

	// The base is not even the file's: the file is this one, which is what the owner wants there.
	res, err := Put(path, []byte(pushedFile), opts("beta", SumOf([]byte("whatever"))))
	require.NoError(t, err)
	assert.False(t, res.Wrote)
	assert.Equal(t, SumOf([]byte(pushedFile)), res.Sum)

	after, _ := HistoryOf(path).List()
	assert.Len(t, after, len(before), "no version made")
}

// A crew that is not there is made only when the owner said so; a refusal leaves nothing behind.
func TestPutMakesACrewOnlyWhenAskedTo(t *testing.T) {
	path := putDir(t, "")
	require.NoError(t, os.Remove(filepath.Dir(path)))

	_, err := Put(path, []byte(pushedFile), opts("beta", ""))
	require.ErrorIs(t, err, ErrNoCrew)
	assert.NoDirExists(t, filepath.Dir(path))

	o := opts("beta", "")
	o.Create = true

	res, err := Put(path, []byte(pushedFile), o)
	require.NoError(t, err)
	assert.True(t, res.Wrote)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, pushedFile, string(got))

	vs, err := HistoryOf(path).List()
	require.NoError(t, err)
	require.NotEmpty(t, vs)
	assert.Equal(t, "mesh alpha", vs[0].By)

	// A base on a crew that is not there is a conflict, and the directory it made is gone again.
	gone := putDir(t, "")
	require.NoError(t, os.Remove(filepath.Dir(gone)))

	o.Base = SumOf([]byte("x"))
	_, err = Put(gone, []byte(pushedFile), o)

	var be *BaseError
	require.ErrorAs(t, err, &be)
	assert.Empty(t, be.Have)
	assert.NoDirExists(t, filepath.Dir(gone))
}

// A file that does not load on this node is refused before it replaces anything.
func TestPutRefusesAFileThatDoesNotLoad(t *testing.T) {
	there := "name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n"
	path := putDir(t, there)
	base := SumOf([]byte(there))

	for name, c := range map[string]struct{ body, node, says string }{
		"not yaml":          {"roles: [a\n  b: {", "beta", "does not load"},
		"no roles":          {"home: alpha\nnodes: {alpha: {}}\n", "alpha", "no roles"},
		"a bad overlay key": {pushedFile + "x-ok: 1\n", "beta", ""},
		"an overlay that fails only for this node":      {strings.Replace(pushedFile, "beta: {prefix: be-}", "beta: {notify_on: 3}", 1), "beta", "nodes.beta"},
		"a bad key under a node":                        {strings.Replace(pushedFile, "beta: {prefix: be-}", "beta: {roles: {}}", 1), "beta", "is not set per node"},
		"no nodes declared":                             {"name: acme\nroles:\n  dispatcher: {}\n", "beta", "declares no nodes:"},
		"a key that is not per node under another node": {strings.Replace(pushedFile, "alpha: {prefix: al-}", "alpha: {roles: {}}", 1), "beta", "would not load on alpha"},
		"binary": {"\x00\x01\x02", "beta", "does not load"},
	} {
		if c.says == "" {
			continue // a file that loads: covered by the push that succeeds
		}

		_, err := Put(path, []byte(c.body), opts(c.node, base))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), c.says, name)

		got, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		assert.Equal(t, there, string(got), name+": the old file stays")
	}

	noTemp(t, path)

	// The same file loads for alpha and not for beta: it is read for the node it is for.
	bad := strings.Replace(pushedFile, "beta: {prefix: be-}", "beta: {notify_on: 3}", 1)

	_, err := Put(path, []byte(bad), opts("beta", base))
	require.Error(t, err)

	res, err := Put(path, []byte(bad), opts("alpha", base))
	require.NoError(t, err)
	assert.True(t, res.Wrote)
}

// Two pushes of one base: one takes the file and the other is told it moved.
func TestPutIsCompareAndSwapUnderConcurrency(t *testing.T) {
	there := "name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n"
	path := putDir(t, there)
	base := SumOf([]byte(there))

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wrote   int
		refused int
	)

	for i := range 6 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			body := pushedFile + "x-n: " + string(rune('a'+i)) + "\n"
			res, err := Put(path, []byte(body), opts("beta", base))

			mu.Lock()
			defer mu.Unlock()

			var be *BaseError

			switch {
			case err == nil && res.Wrote:
				wrote++
			case errors.As(err, &be):
				refused++
			default:
				t.Errorf("unexpected: %v %v", res, err)
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, 1, wrote)
	assert.Equal(t, 5, refused)
	noTemp(t, path)
}

func TestNewCrewKey(t *testing.T) {
	for key, want := range map[string]bool{
		"acme": true, "acme-ops": true, "default": true, "a1": true,
		"": false, "..": false, "../x": false, "a/b": false, "/etc": false, ".hidden": false, "Acme": false, "1a": false, "a b": false,
		"mesh": false, "tasks": false, "later": false, "crew.d": false, "worktrees": false, "plugins": false, "agents": false,
		strings.Repeat("a", 32): false,
	} {
		assert.Equal(t, want, NewCrewKey(key), "%q", key)
	}
}
