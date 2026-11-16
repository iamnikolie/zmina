package crew

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/fsx"
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

	// The same file does not load for beta, so it is taken by no node: it goes to every node it lists in the end.
	bad := strings.Replace(pushedFile, "beta: {prefix: be-}", "beta: {notify_on: 3}", 1)

	for _, node := range []string{"beta", "alpha"} {
		_, err := Put(path, []byte(bad), opts(node, base))
		require.Error(t, err, node)
		assert.Contains(t, err.Error(), "nodes.beta", node)
	}
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

// A file that loads for the node it is pushed to and not for another node it lists is refused: a push goes to every node in the end,
// and a node that took what a third node cannot load is a split crew (a hand copy would stop that node).
func TestPutRefusesAFileThatDoesNotLoadForAnotherListedNode(t *testing.T) {
	there := "name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n"
	path := putDir(t, there)
	base := SumOf([]byte(there))

	three := strings.Replace(pushedFile, "  beta: {prefix: be-}\n", "  beta: {prefix: be-}\n  gamma: {GAMMA}\n", 1)

	for name, gamma := range map[string]string{
		"a quiet_hours that is no hours": "quiet_hours: nonsense",
		"a time zone that is none":       "timezone: Mars/Olympus",
		"a map under notify":             "notify: {a: b}",
		"a prefix that is no prefix":     "prefix: 'a b'",
	} {
		body := strings.Replace(three, "GAMMA", gamma, 1)

		// It loads for beta, the node it is pushed to, as it is.
		_, err := LoadFor(writeTemp(t, body), "beta")
		require.NoError(t, err, name+": loads for beta")

		_, err = LoadFor(writeTemp(t, body), "gamma")
		require.Error(t, err, name+": and not for gamma")

		_, err = Put(path, []byte(body), opts("beta", base))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "would not load on gamma", name)

		got, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		assert.Equal(t, there, string(got), name+": the old file stays")
	}

	noTemp(t, path)

	// A file that loads for every node it lists is taken.
	good := strings.Replace(three, "GAMMA", "prefix: ga-", 1)
	res, err := Put(path, []byte(good), opts("beta", base))
	require.NoError(t, err)
	assert.True(t, res.Wrote)
}

// writeTemp writes body to a crew file of its own and returns the path.
func writeTemp(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// A push that fails after the crew's directory was made leaves nothing behind, history lock included, so the name is free again.
func TestPutThatFailsLeavesNoDirectoryItMade(t *testing.T) {
	path := putDir(t, "")
	dir := filepath.Dir(path)
	require.NoError(t, os.Remove(dir))

	// The write of the file fails after the history's lock was made: the staged file is gone when it should take its name.
	fsx.BeforeCommit = func(tmp, p string) {
		if filepath.Base(p) == "crew.yaml" {
			_ = os.Remove(tmp)
		}
	}

	t.Cleanup(func() { fsx.BeforeCommit = nil })

	o := opts("beta", "")
	o.Create = true

	_, err := Put(path, []byte(pushedFile), o)
	require.Error(t, err)
	assert.NoDirExists(t, dir, "the directory the push made, and what it put in it, are gone")

	// The name is free: the same push, nothing in the way, makes the crew.
	fsx.BeforeCommit = nil

	res, err := Put(path, []byte(pushedFile), o)
	require.NoError(t, err)
	assert.True(t, res.Wrote)
}

// A push that fails in a directory that was there already takes away what it made in it (the history) and no more.
func TestPutThatFailsKeepsADirectoryThatWasThere(t *testing.T) {
	path := putDir(t, "")
	dir := filepath.Dir(path)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("mine"), 0o600))

	fsx.BeforeCommit = func(tmp, p string) {
		if filepath.Base(p) == "crew.yaml" {
			_ = os.Remove(tmp)
		}
	}

	t.Cleanup(func() { fsx.BeforeCommit = nil })

	o := opts("beta", "")
	o.Create = true

	_, err := Put(path, []byte(pushedFile), o)
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(dir, "keep.txt"))
	assert.NoDirExists(t, filepath.Join(dir, "crew.d"))
}

// A push does not write through a link that leaves the zmina home: not a crew directory that is one, nor a crew file, nor a history.
func TestPutRefusesToWriteThroughALinkOutOfTheHome(t *testing.T) {
	there := "name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n"
	base := SumOf([]byte(there))

	setup := func(t *testing.T) (home, outside string) {
		t.Helper()
		t.Setenv("CLAUDE_CONFIG_DIR", "")

		return t.TempDir(), t.TempDir()
	}

	withHome := func(home string) PutOptions {
		o := opts("beta", base)
		o.Home = home

		return o
	}

	t.Run("a crew directory that is a link", func(t *testing.T) {
		home, outside := setup(t)
		require.NoError(t, os.WriteFile(filepath.Join(outside, "crew.yaml"), []byte(there), 0o600))
		require.NoError(t, os.Symlink(outside, filepath.Join(home, "acme")))

		_, err := Put(filepath.Join(home, "acme", "crew.yaml"), []byte(pushedFile), withHome(home))
		require.ErrorContains(t, err, "leaves")

		got, rerr := os.ReadFile(filepath.Join(outside, "crew.yaml"))
		require.NoError(t, rerr)
		assert.Equal(t, there, string(got), "the file the link leads to is as it was")
		assert.NoFileExists(t, filepath.Join(outside, ".crew-push-1.yaml"))
		assert.NoDirExists(t, filepath.Join(outside, "crew.d"), "nothing was made there")
	})

	t.Run("a crew file that is a link", func(t *testing.T) {
		home, outside := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(home, "acme"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(outside, "dotfiles-crew.yaml"), []byte(there), 0o600))
		require.NoError(t, os.Symlink(filepath.Join(outside, "dotfiles-crew.yaml"), filepath.Join(home, "acme", "crew.yaml")))

		_, err := Put(filepath.Join(home, "acme", "crew.yaml"), []byte(pushedFile), withHome(home))
		require.ErrorContains(t, err, "leaves")

		got, rerr := os.ReadFile(filepath.Join(outside, "dotfiles-crew.yaml"))
		require.NoError(t, rerr)
		assert.Equal(t, there, string(got))
	})

	t.Run("a history that is a link", func(t *testing.T) {
		home, outside := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(home, "acme", "crew.d"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, "acme", "crew.yaml"), []byte(there), 0o600))
		require.NoError(t, os.Symlink(outside, filepath.Join(home, "acme", "crew.d", "history")))

		_, err := Put(filepath.Join(home, "acme", "crew.yaml"), []byte(pushedFile), withHome(home))
		require.ErrorContains(t, err, "leaves")

		entries, rerr := os.ReadDir(outside)
		require.NoError(t, rerr)
		assert.Empty(t, entries, "nothing was written where the link leads")
	})

	t.Run("a link that stays inside the home is a file there like another", func(t *testing.T) {
		home, _ := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(home, "shared"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, "shared", "base.yaml"), []byte(there), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(home, "acme"), 0o700))
		require.NoError(t, os.Symlink(filepath.Join(home, "shared", "base.yaml"), filepath.Join(home, "acme", "crew.yaml")))

		res, err := Put(filepath.Join(home, "acme", "crew.yaml"), []byte(pushedFile), withHome(home))
		require.NoError(t, err)
		assert.True(t, res.Wrote)
	})

	t.Run("a home that is itself a link, as it is on a machine that keeps it elsewhere", func(t *testing.T) {
		real, _ := setup(t)
		home := filepath.Join(t.TempDir(), "zmina-home")
		require.NoError(t, os.Symlink(real, home))
		require.NoError(t, os.MkdirAll(filepath.Join(real, "acme"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(real, "acme", "crew.yaml"), []byte(there), 0o600))

		res, err := Put(filepath.Join(home, "acme", "crew.yaml"), []byte(pushedFile), withHome(home))
		require.NoError(t, err)
		assert.True(t, res.Wrote)
	})

	t.Run("a crew made under a link", func(t *testing.T) {
		home, outside := setup(t)
		require.NoError(t, os.Symlink(outside, filepath.Join(home, "acme")))

		o := withHome(home)
		o.Base, o.Create = "", true

		_, err := Put(filepath.Join(home, "acme", "crew.yaml"), []byte(pushedFile), o)
		require.ErrorContains(t, err, "leaves")

		entries, rerr := os.ReadDir(outside)
		require.NoError(t, rerr)
		assert.Empty(t, entries)
	})
}

// Every name zmina keeps under a crew's directory is one a crew made by a push may not take, for the default crew keeps its own files in the
// directory where the other crews' directories are. The names are walked from Layout, so a path added to it is one more.
func TestReservedKeysCoverEveryNameOfTheLayout(t *testing.T) {
	root := filepath.Join("root", "x")
	l := Layout{Dir: root}
	v := reflect.ValueOf(l)

	var first []string

	for i := range v.NumMethod() {
		m := v.Method(i)
		if m.Type().NumOut() != 1 || m.Type().Out(0).Kind() != reflect.String {
			continue
		}

		args := make([]reflect.Value, m.Type().NumIn())
		for j := range args {
			args[j] = reflect.ValueOf("arg")
		}

		p := m.Call(args)[0].String()
		rel, err := filepath.Rel(root, p)
		require.NoError(t, err, v.Type().Method(i).Name)

		first = append(first, strings.Split(filepath.ToSlash(rel), "/")[0])
	}

	require.Greater(t, len(first), 25, "the walk found the Layout's paths")

	for _, name := range first {
		assert.False(t, NewCrewKey(name), "%q is a name zmina keeps in a crew directory: it is no crew's key", name)
	}

	// What zmina keeps there that no Layout method names: stores that live next to the layout, under the crew's directory.
	for _, name := range []string{"codex", "messages", "tells", "chat-files", "lessons", "pruned", "context", "results", "checkpoints",
		"handoffs", "releases", "restarts", "archive", "agents", "mesh", "bin", "mesh-ui", "lock", "watch"} {
		assert.False(t, NewCrewKey(name), "%q", name)
	}

	// A crew's key still may be what it was: a word of lower case.
	for _, name := range []string{"acme", "acme-ops", "default", "ops2"} {
		assert.True(t, NewCrewKey(name), "%q", name)
	}
}

const editable = "name: acme\nnamespace: /ns\nhome: alpha\nnodes:\n  alpha: {}\n  beta: {}\nroles:\n  dispatcher: {}\n  dev: {}\n"

// A seat edit that read the file before a push replaced it does not write over the push: it fails, saying to run it again, and the file
// is the pushed one. The same for an edit made by hand meanwhile.
func TestAnEditThatReadTheFileBeforeItChangedFailsInsteadOfWinning(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, path string){
		"a push": func(t *testing.T, path string) {
			pushed := editable + "x-pushed: yes\n"
			res, err := Put(path, []byte(pushed), opts("beta", SumOf([]byte(editable))))
			require.NoError(t, err)
			require.True(t, res.Wrote)
		},
		"a hand edit": func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte(editable+"x-hand: yes\n"), 0o600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := putDir(t, editable)
			require.NoError(t, os.MkdirAll(filepath.Join(filepath.Dir(path), "dev"), 0o700))

			e, err := Open(path)
			require.NoError(t, err)
			require.NoError(t, e.Add("ops", map[string]string{"about": "runs things"}))

			change(t, path)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			err = e.Save()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the crew file changed meanwhile, run it again")

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "the file is the one that was written meanwhile, whole")
			assert.NotContains(t, string(after), "ops:")
			assert.Empty(t, e.Saved.Versions, "nothing was kept: nothing was written")

			// Run again: the edit reads the new file and saves.
			e, err = Open(path)
			require.NoError(t, err)
			require.NoError(t, e.Add("ops", map[string]string{"about": "runs things"}))
			require.NoError(t, e.Save())

			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(raw), "ops:")
			assert.Contains(t, string(raw), "x-")
		})
	}
}

// An edit of a crew that nothing else touches saves as it always did (a crew with no nodes: included): no refusal, one version.
func TestAnEditThatNothingRacedSavesAsBefore(t *testing.T) {
	path := putDir(t, "name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n")

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Add("dev", map[string]string{"about": "code"}))
	require.NoError(t, e.Save())

	e, err = Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Set("dev", map[string]string{"model": "opus"}))
	require.NoError(t, e.Save())

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"dispatcher", "dev"}, c.Names())
}

// Edits and pushes at once: each is either written whole or told to run again, so none is lost.
func TestEditsAndPushesAtOnceLoseNothing(t *testing.T) {
	path := putDir(t, editable)

	const workers = 8

	var wg sync.WaitGroup

	for i := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 200 {
				if i%2 == 0 {
					e, err := Open(path)
					if err != nil {
						t.Errorf("open: %v", err)

						return
					}

					if err = e.Add("r"+string(rune('a'+i)), map[string]string{"about": "x"}); err != nil {
						t.Errorf("add: %v", err)

						return
					}

					if err = e.Save(); err == nil {
						return
					} else if !strings.Contains(err.Error(), "run it again") {
						t.Errorf("save: %v", err)

						return
					}

					continue
				}

				// A push that adds a key of its own to what it finds, based on what it read.
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("read: %v", err)

					return
				}

				body := string(raw) + "x-k" + string(rune('a'+i)) + ": 1\n"

				if _, err := Put(path, []byte(body), opts("beta", SumOf(raw))); err == nil {
					return
				} else if !errors.As(err, new(*BaseError)) {
					t.Errorf("put: %v", err)

					return
				}
			}

			t.Errorf("worker %d never got through", i)
		}()
	}

	wg.Wait()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	for i := range workers {
		if i%2 == 0 {
			assert.Contains(t, string(raw), "r"+string(rune('a'+i))+":", "the role of worker %d", i)
		} else {
			assert.Contains(t, string(raw), "x-k"+string(rune('a'+i))+": 1", "the key of worker %d", i)
		}
	}
}
