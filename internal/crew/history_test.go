package crew

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historyFixture is a crew file in a temp dir and a history on a clock that moves a minute a call.
func historyFixture(t *testing.T) (string, *History) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte("dir: "+dir+"\nroles:\n  dispatcher:\n"), 0o600))

	h := HistoryOf(path)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h.Now = func() time.Time {
		at = at.Add(time.Minute)

		return at
	}

	return path, h
}

func versionNames(t *testing.T, h *History) []string {
	t.Helper()

	vs, err := h.List()
	require.NoError(t, err)

	var out []string
	for _, v := range vs {
		out = append(out, v.How)
	}

	return out
}

// A change by zmina keeps the version it replaced and the one it made; the newest is the file as it is.
func TestCommitKeepsThePreviousVersion(t *testing.T) {
	path, h := historyFixture(t)
	v1, _ := os.ReadFile(path)
	v2 := []byte(string(v1) + "  dev:\n    model: sonnet\n")

	saved, err := h.Commit(path, v2, "dispatcher", "zmina seat add")
	require.NoError(t, err)
	require.NoError(t, saved.HistoryErr)
	require.Len(t, saved.Versions, 2, "the file as it was, then the change")
	assert.True(t, saved.Versions[0].First, "the first look is a start, not a change")
	assert.Equal(t, HowFirst, saved.Versions[0].How)
	assert.Equal(t, "added dev", saved.Versions[1].What)
	assert.Equal(t, "dev", saved.Versions[1].Role)
	assert.Equal(t, "dispatcher", saved.Versions[1].By)

	got, _ := os.ReadFile(path)
	assert.Equal(t, string(v2), string(got))

	vs, err := h.List()
	require.NoError(t, err)
	require.Len(t, vs, 2)
	assert.Equal(t, "zmina seat add", vs[0].How, "newest first")

	old, err := h.Bytes(vs[1])
	require.NoError(t, err)
	assert.Equal(t, string(v1), string(old), "the previous version is the bytes it had")
	assert.True(t, h.Seen(v2))
	assert.False(t, h.Seen(v1))

	// The same change again changes nothing: no version, no event to log.
	saved, err = h.Commit(path, v2, "dispatcher", "zmina seat set")
	require.NoError(t, err)
	assert.Empty(t, saved.Versions)
	assert.Len(t, versionNames(t, h), 2)
}

// An edit made outside zmina is told apart on the next look; a look at a file as it was recorded writes nothing, not
// even a lock file, so a ticker that loads the crew file every minute leaves the history alone.
func TestNoticeSeesAnEditByHandAndOtherwiseWritesNothing(t *testing.T) {
	path, h := historyFixture(t)
	v1, _ := os.ReadFile(path)

	r, err := h.Notice(v1)
	require.NoError(t, err)
	assert.True(t, r.Wrote)
	assert.True(t, r.First)

	before := listDir(t, h.Dir)

	require.NoError(t, os.Chmod(h.Dir, 0o500), "a directory nothing can be written in")
	t.Cleanup(func() { _ = os.Chmod(h.Dir, 0o700) })

	for range 3 {
		r, err = h.Notice(v1)
		require.NoError(t, err, "a file as it was recorded needs no write")
		assert.False(t, r.Wrote)
	}

	require.NoError(t, os.Chmod(h.Dir, 0o700))
	assert.Equal(t, before, listDir(t, h.Dir), "nothing was added or rewritten")

	v2 := []byte(string(v1) + "  reviewer:\n    model: opus\n")
	require.NoError(t, os.WriteFile(path, v2, 0o600))

	r, err = h.Notice(v2)
	require.NoError(t, err)
	assert.True(t, r.Wrote)
	assert.False(t, r.First)
	assert.Equal(t, HowHand, r.How)
	assert.Empty(t, r.By, "who made it is not known")
	assert.Equal(t, "added reviewer", r.What)
	assert.Equal(t, "reviewer", r.Role)
	assert.Equal(t, "added reviewer (edited by hand)", r.Detail())
	assert.Equal(t, []string{HowHand, HowFirst}, versionNames(t, h))

	// Going back to an older content is a change too.
	r, err = h.Notice(v1)
	require.NoError(t, err)
	assert.True(t, r.Wrote)
	assert.Equal(t, "removed reviewer", r.What)
	assert.Len(t, versionNames(t, h), 3)
}

func listDir(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	out := map[string]string{}

	for _, e := range entries {
		fi, err := e.Info()
		require.NoError(t, err)

		out[e.Name()] = fmt.Sprint(fi.Size(), fi.ModTime().UnixNano())
	}

	return out
}

// An edit made by hand before zmina's own is kept as a version of its own, then zmina's change on top of it.
func TestCommitTellsAHandEditApartFromItsOwnChange(t *testing.T) {
	path, h := historyFixture(t)
	v1, _ := os.ReadFile(path)

	_, err := h.Notice(v1)
	require.NoError(t, err)

	byHand := []byte(string(v1) + "  scribe:\n")
	require.NoError(t, os.WriteFile(path, byHand, 0o600))

	saved, err := h.Commit(path, []byte(string(byHand)+"  dev:\n"), "hand", "zmina seat add")
	require.NoError(t, err)
	require.Len(t, saved.Versions, 2)
	assert.Equal(t, HowHand, saved.Versions[0].How)
	assert.Equal(t, "added scribe", saved.Versions[0].What)
	assert.Equal(t, "zmina seat add", saved.Versions[1].How)
	assert.Equal(t, "added dev", saved.Versions[1].What)
}

// The oldest versions go past the keep count; a clock that stands still or runs back still orders them as made.
func TestHistoryKeepsTheNewestN(t *testing.T) {
	path, h := historyFixture(t)
	h.Keep = 3
	h.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	cur, _ := os.ReadFile(path)

	for i := range 6 {
		cur = []byte(string(cur) + fmt.Sprintf("  r%d:\n", i))
		_, err := h.Commit(path, cur, "hand", fmt.Sprintf("change %d", i))
		require.NoError(t, err)
	}

	assert.Equal(t, []string{"change 5", "change 4", "change 3"}, versionNames(t, h))

	entries, err := os.ReadDir(h.Dir)
	require.NoError(t, err)

	yamls := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			yamls++
		}
	}

	assert.Equal(t, 3, yamls, "their meta files go with them")
	assert.Len(t, entries, 3*2+2, "versions and metas, HEAD and the lock")
}

// Two commands that see the same edit at once keep it once.
func TestConcurrentNoticesKeepOneVersion(t *testing.T) {
	path, h := historyFixture(t)
	v1, _ := os.ReadFile(path)

	_, err := h.Notice(v1)
	require.NoError(t, err)

	v2 := []byte(string(v1) + "  dev:\n")

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wrote int
	)

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			r, err := HistoryOf(path).Notice(v2)
			assert.NoError(t, err)

			if r.Wrote {
				mu.Lock()
				wrote++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, 1, wrote)
	assert.Len(t, versionNames(t, h), 2)
}

// A recording that stopped before it wrote HEAD is mended, not repeated.
func TestHistoryMendsHeadWithoutAVersionTwice(t *testing.T) {
	path, h := historyFixture(t)
	v1, _ := os.ReadFile(path)

	_, err := h.Notice(v1)
	require.NoError(t, err)
	require.NoError(t, os.Remove(h.head()))
	assert.False(t, h.Seen(v1))

	r, err := h.Notice(v1)
	require.NoError(t, err)
	assert.False(t, r.Wrote, "the newest version already is these bytes")
	assert.True(t, h.Seen(v1))
	assert.Len(t, versionNames(t, h), 1)
}

// A version whose meta file is gone still lists and restores.
func TestHistoryListsAVersionWithoutItsMeta(t *testing.T) {
	path, h := historyFixture(t)
	v1, _ := os.ReadFile(path)

	r, err := h.Notice(v1)
	require.NoError(t, err)
	require.NoError(t, os.Remove(h.file(r.Stem, ".json")))

	vs, err := h.List()
	require.NoError(t, err)
	require.Len(t, vs, 1)
	assert.Equal(t, "unknown", vs[0].How)
	assert.Equal(t, r.Sum[:8], vs[0].Sum)
	assert.False(t, vs[0].At.IsZero())

	got, err := h.Bytes(vs[0])
	require.NoError(t, err)
	assert.Equal(t, string(v1), string(got))
}

// Restore writes an older version back as a change of its own, and refuses one that no longer loads.
func TestRestorePutsAVersionBackAsAChange(t *testing.T) {
	path, h := historyFixture(t)
	v1, _ := os.ReadFile(path)

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Add("dev", map[string]string{"model": "opus"}))
	e.Note("hand", "zmina seat add")
	require.NoError(t, e.Save())
	require.Len(t, e.Saved.Versions, 2)

	vs, err := h.List()
	require.NoError(t, err)
	require.Len(t, vs, 2)

	old, err := h.Bytes(vs[1])
	require.NoError(t, err)
	assert.Equal(t, string(v1), string(old))

	saved, err := Restore(path, old, "hand", "zmina crew history --restore 1")
	require.NoError(t, err)
	require.Len(t, saved.Versions, 1)
	assert.Equal(t, "removed dev", saved.Versions[0].What)

	got, _ := os.ReadFile(path)
	assert.Equal(t, string(v1), string(got))
	assert.Equal(t, []string{"zmina crew history --restore 1", "zmina seat add", HowFirst}, versionNames(t, h),
		"the restore is a version too, so it can be undone")

	before, _ := os.ReadFile(path)
	_, err = Restore(path, []byte("roles: [not, a, mapping]\n"), "hand", "zmina crew history --restore 9")
	require.Error(t, err)
	assert.ErrorContains(t, err, "would not load")

	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after), "a file that does not load is not written")
	assert.Len(t, versionNames(t, h), 3)
}

// A seat edit goes through the history, and the old single .bak is gone.
func TestSaveKeepsHistoryNotABakFile(t *testing.T) {
	dir := t.TempDir()
	path := write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n")

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Add("dev", map[string]string{"model": "opus"}))
	e.Note("dispatcher", "zmina seat add")
	require.NoError(t, e.Save())

	e, err = Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Set("dev", map[string]string{"model": "sonnet", "about": "code"}))
	e.Note("dispatcher", "zmina seat set")
	require.NoError(t, e.Save())
	require.Len(t, e.Saved.Versions, 1)
	assert.Equal(t, "dev: about, model", e.Saved.Versions[0].What)
	assert.Equal(t, "dev", e.Saved.Versions[0].Role)

	assert.NoFileExists(t, path+".bak")

	vs, err := HistoryOf(path).List()
	require.NoError(t, err)
	assert.Len(t, vs, 3, "the file as first seen, the added seat, the changed one")

	// A change that does not load keeps nothing.
	e, err = Open(path)
	require.NoError(t, err)
	require.NoError(t, e.Set("dev", map[string]string{"keep": "sometimes"}))
	require.Error(t, e.Save())

	vs, _ = HistoryOf(path).List()
	assert.Len(t, vs, 3)
}

func TestSummarize(t *testing.T) {
	base := "dir: /x\nnotify_on: [ask]\nroles:\n  dev:\n    model: opus\n    args: [--a]\n  scribe:\n    about: notes\n"

	for name, tc := range map[string]struct {
		cur, what, role string
	}{
		"one key of one role": {strings.Replace(base, "opus", "sonnet", 1), "dev: model", "dev"},
		"two keys":            {strings.Replace(strings.Replace(base, "opus", "sonnet", 1), "[--a]", "[--b]", 1), "dev: args, model", "dev"},
		"added role":          {base + "  new:\n    keep: always\n", "added new", "new"},
		"removed role":        {strings.Split(base, "  scribe:")[0], "removed scribe", "scribe"},
		"crew key":            {strings.Replace(base, "[ask]", "[ask, blocking]", 1), "crew: notify_on", ""},
		"role and crew key":   {strings.Replace(strings.Replace(base, "opus", "sonnet", 1), "[ask]", "[]", 1), "dev: model; crew: notify_on", ""},
		"two roles":           {strings.Replace(strings.Replace(base, "opus", "sonnet", 1), "notes", "more", 1), "dev: model; scribe: about", ""},
		"comment only":        {"# hello\n" + base, "comments or layout only", ""},
		"five added":          {base + "  a:\n  b:\n  c:\n  d:\n  e:\n", "added a, b, c, d and 1 more", ""},
	} {
		t.Run(name, func(t *testing.T) {
			what, role := summarize([]byte(base), []byte(tc.cur))
			assert.Equal(t, tc.what, what)
			assert.Equal(t, tc.role, role)
		})
	}

	what, role := summarize([]byte(base), []byte("roles: ["))
	assert.Empty(t, what, "a file that does not parse says nothing of its change")
	assert.Empty(t, role)
}

func TestUnifiedDiff(t *testing.T) {
	assert.Empty(t, UnifiedDiff([]byte("a\nb\n"), []byte("a\nb\n"), "x", "y"))

	got := UnifiedDiff([]byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"), []byte("a\nb\nc\nd\nE\nf\ng\nh\ni\nj\n"), "old", "new")
	assert.Equal(t, "--- old\n+++ new\n@@ -2,7 +2,7 @@\n b\n c\n d\n-e\n+E\n f\n g\n h\n", got)

	got = UnifiedDiff([]byte("a\n"), []byte("a\nb\n"), "old", "new")
	assert.Equal(t, "--- old\n+++ new\n@@ -1 +1,2 @@\n a\n+b\n", got)

	got = UnifiedDiff(nil, []byte("a\n"), "old", "new")
	assert.Equal(t, "--- old\n+++ new\n@@ -0,0 +1 @@\n+a\n", got)

	// Two changes far apart are two hunks; near ones share one.
	var a, b []string
	for i := range 30 {
		a = append(a, fmt.Sprint("line", i))
		b = append(b, fmt.Sprint("line", i))
	}

	b[2], b[25] = "changed2", "changed25"
	got = UnifiedDiff([]byte(strings.Join(a, "\n")+"\n"), []byte(strings.Join(b, "\n")+"\n"), "old", "new")
	assert.Equal(t, 2, strings.Count(got, "@@ -"), got)
	assert.Contains(t, got, "-line2\n+changed2\n")
	assert.Contains(t, got, "-line25\n+changed25\n")

	b[2], b[25] = "line2", "line25"
	b[2], b[7] = "changed2", "changed7"
	got = UnifiedDiff([]byte(strings.Join(a, "\n")+"\n"), []byte(strings.Join(b, "\n")+"\n"), "old", "new")
	assert.Equal(t, 1, strings.Count(got, "@@ -"), got)
}
