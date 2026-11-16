package move

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/record"
)

var (
	sumA = strings.Repeat("a", 64)
	sumB = strings.Repeat("b", 64)
	sha1 = strings.Repeat("c", 40)
)

func goodBundle() Bundle {
	return Bundle{Version: Version, Move: "mv-0123456789ab", Role: "dev", From: "alpha", To: "beta", Base: sumA, After: sumB,
		Dir: "~/work", Namespace: "~/ns", Tasks: []BTask{{ID: "T-14", Asked: "fix the form", Title: "Form", Criteria: []string{"it saves"},
			Repo: "~/work/acme", Origin: "git@example.com:acme/app.git", Branch: "zmina/alpha-acme-t-14", Base: "main", Fork: sha1, Tip: sha1}}}
}

func TestAGoodBundleReadsBackAndANewIDIsValid(t *testing.T) {
	raw, err := goodBundle().Marshal()
	require.NoError(t, err)

	b, err := ParseBundle(raw)
	require.NoError(t, err)
	assert.Equal(t, goodBundle(), b)

	for range 20 {
		assert.True(t, ValidID(NewID()))
	}
}

// A bundle comes from another node: each field is checked before the node acts on it, and none may be a path, a command or a ref that
// is no plain branch.
func TestBundleFieldsAreCheckedAsUntrusted(t *testing.T) {
	cases := map[string]func(b *Bundle){
		"a version from the future": func(b *Bundle) { b.Version = 2 },
		"a bad move id":             func(b *Bundle) { b.Move = "mv-../../x" },
		"an id that is a path":      func(b *Bundle) { b.Move = "../../etc/passwd" },
		"a role with a slash":       func(b *Bundle) { b.Role = "a/b" },
		"a copy as the role":        func(b *Bundle) { b.Role = "dev#2" },
		"a node with a space":       func(b *Bundle) { b.From = "al pha" },
		"the same node twice":       func(b *Bundle) { b.To = b.From },
		"a sum that is no hash":     func(b *Bundle) { b.Base = "x" },
		"too many tasks": func(b *Bundle) {
			for i := range taskMax + 1 {
				b.Tasks = append(b.Tasks, BTask{ID: "T-" + strings.Repeat("1", 1) + string(rune('a'+i%20))})
			}
		},
		"a dir that climbs":            func(b *Bundle) { b.Dir = "~/../../etc" },
		"a relative dir":               func(b *Bundle) { b.Dir = "work" },
		"a namespace with a newline":   func(b *Bundle) { b.Namespace = "~/ns\nrm -rf" },
		"a task id with a path":        func(b *Bundle) { b.Tasks[0].ID = "T-1/../x" },
		"a task id that is no id":      func(b *Bundle) { b.Tasks[0].ID = "t-14" },
		"a repo that is relative":      func(b *Bundle) { b.Tasks[0].Repo = "acme" },
		"a repo that climbs":           func(b *Bundle) { b.Tasks[0].Repo = "/srv/../etc" },
		"a repo with a control char":   func(b *Bundle) { b.Tasks[0].Repo = "~/a\x00b" },
		"no origin for a repo":         func(b *Bundle) { b.Tasks[0].Origin = "" },
		"an origin with a newline":     func(b *Bundle) { b.Tasks[0].Origin = "a\nb" },
		"a branch that is an option":   func(b *Bundle) { b.Tasks[0].Branch = "--upload-pack=touch /tmp/x" },
		"a branch with a command":      func(b *Bundle) { b.Tasks[0].Branch = "zmina/$(id)" },
		"a branch with two dots":       func(b *Bundle) { b.Tasks[0].Branch = "zmina/a..b" },
		"a tip that is a branch":       func(b *Bundle) { b.Tasks[0].Tip = "main" },
		"a short tip":                  func(b *Bundle) { b.Tasks[0].Tip = "abc123" },
		"a base that is an option":     func(b *Bundle) { b.Tasks[0].Base = "--exec=x" },
		"a fork that is a path":        func(b *Bundle) { b.Tasks[0].Fork = "../x" },
		"a branch with no repository":  func(b *Bundle) { b.Tasks[0].Repo, b.Tasks[0].Origin = "", "" },
		"a tip with no branch":         func(b *Bundle) { b.Tasks[0].Branch = "" },
		"a criterion of two lines":     func(b *Bundle) { b.Tasks[0].Criteria = []string{"a\nb"} },
		"a wip that is no commit":      func(b *Bundle) { b.Tasks[0].WIP = "abc" },
		"a wip that is a ref":          func(b *Bundle) { b.Tasks[0].WIP = "refs/heads/main" },
		"a negative budget":            func(b *Bundle) { b.Tasks[0].Budget = -1 },
		"the same task twice":          func(b *Bundle) { b.Tasks = append(b.Tasks, b.Tasks[0]) },
		"a checkpoint of another kind": func(b *Bundle) { b.Tasks[0].Checkpoint = &record.Document{Kind: "result"} },
		"a record that breaks its shape": func(b *Bundle) {
			b.Tasks[0].Checkpoint = &record.Document{Kind: "checkpoint", Shape: crew.HandoffShape{Sections: []crew.Section{{Name: "summary"}}},
				Values: map[string]string{"other": "x"}}
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := goodBundle()
			mutate(&b)
			assert.Error(t, b.Validate())
		})
	}
}

func TestBundleSizeIsCheckedBeforeAnythingIsParsed(t *testing.T) {
	_, err := ParseBundle(nil)
	require.Error(t, err)

	_, err = ParseBundle([]byte(strings.Repeat("x", BundleMax+1)))
	require.ErrorContains(t, err, "1 to")

	// A bundle that would be over the cap is not made.
	b := goodBundle()
	b.Tasks[0].Note = strings.Repeat("n", textMax)
	b.Tasks = append(b.Tasks, BTask{ID: "T-5", Note: strings.Repeat("n", textMax), Asked: strings.Repeat("a", textMax)}, BTask{ID: "T-6", Note: strings.Repeat("n", textMax), Asked: strings.Repeat("a", textMax)})
	_, err = b.Marshal()
	require.ErrorContains(t, err, "a move carries at most")

	// Unknown fields from a newer zmina are ignored, never acted on.
	raw, err := json.Marshal(map[string]any{"version": 1, "move": "mv-0123456789ab", "role": "dev", "from": "alpha", "to": "beta",
		"base": sumA, "after": sumB, "dir": "~/w", "namespace": "~/n", "exec": "rm -rf /",
		"tasks": []map[string]any{{"id": "T-1", "asked": "x", "run": "touch /tmp/x"}}})
	require.NoError(t, err)

	b2, err := ParseBundle(raw)
	require.NoError(t, err)
	assert.Equal(t, "T-1", b2.Tasks[0].ID)
}

func TestJournalAndReceiptsRoundTrip(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}

	_, ok, err := Open(l, "dev")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, OpenID(l, "dev"))

	j := Journal{ID: "mv-0123456789ab", Role: "dev", From: "alpha", To: "beta", Phase: PhaseAsked, Started: time.Now().UTC(), Base: sumA, After: sumB,
		Tasks: []Task{{ID: "T-14", Item: "L-1"}}}
	require.NoError(t, Save(l, j))

	got, ok, err := Open(l, "dev")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, j.ID, got.ID)
	assert.Equal(t, PhaseAsked, got.Phase)
	assert.Equal(t, "mv-0123456789ab", OpenID(l, "dev"))

	// A journal that does not read keeps the role refused.
	require.NoError(t, os.WriteFile(l.Move("dev"), []byte("{"), 0o600))
	assert.Equal(t, "unreadable", OpenID(l, "dev"))

	require.NoError(t, Close(l, "dev"))
	require.NoError(t, Close(l, "dev"), "closing twice is no error")
	assert.Empty(t, OpenID(l, "dev"))

	_, ok, err = Receipt(l, "mv-0123456789ab")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, SaveReceipt(l, TakeAnswer{Move: "mv-0123456789ab", Node: "beta", Role: "dev", Tasks: []Taken{{From: "T-14", To: "T-9"}}, Sum: sumB}))

	a, ok, err := Receipt(l, "mv-0123456789ab")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "T-9", a.Tasks[0].To)

	_, _, err = Receipt(l, "../../x")
	require.Error(t, err, "a receipt is named by a move id, never a path")
	require.Error(t, SaveReceipt(l, TakeAnswer{Move: "../x"}))

	require.NoError(t, SaveDrive(l, Drive{ID: "mv-0123456789ab", Role: "dev", From: "alpha", To: "beta"}))
	assert.Len(t, Drives(l), 1)
	require.NoError(t, CloseDrive(l, "dev"))
	assert.Empty(t, Drives(l))
}

// Two nodes whose users have different home directories hold the same place when it is under home.
func TestPortablePathsAreHomeRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Equal(t, "~/work/acme", Portable(filepath.Join(home, "work", "acme")))
	assert.Equal(t, "~", Portable(home))
	assert.Equal(t, "/srv/acme", Portable("/srv/acme"))
	assert.Equal(t, "", Portable(""))
	assert.Equal(t, "/homework", Portable("/homework"), "a sibling that starts with the same letters is no part of home")
	assert.Equal(t, filepath.Join(home, "x"), Expand("~/x"))
}

// Every text field of a bundle that is shown or stored loses its control characters: the escape sequences, the bell, the format
// characters that reorder text. A text of several lines keeps its line breaks, a line of one does not.
func TestParseBundleStripsControlCharactersFromEveryTextField(t *testing.T) {
	b := goodBundle()
	b.Tasks[0].ID = "T-9"
	esc := "\x1b[2J\x1b]0;pwned\x07"

	b.Tasks[0].Asked = "fix" + esc + "\nthe form\u202e"
	b.Tasks[0].Note = "note" + esc + "\n\tindented"
	b.Tasks[0].Title = "Form" + esc + "\nsecond line"
	b.Tasks[0].Project = "acme" + esc
	b.Tasks[0].URL = "https://example.com/" + esc
	b.Tasks[0].Criteria = []string{"it saves" + esc + "\r\nand more"}

	shape := crew.HandoffShape{Sections: []crew.Section{{Name: "summary", Guidance: "what" + esc}}}
	doc := &record.Document{Kind: "checkpoint", Task: "T-9", Role: "dev", By: "dev" + esc, Shape: shape,
		Values: map[string]string{"summary": "section" + esc + "\nline two"}}
	b.Tasks[0].Checkpoint = doc

	raw, err := json.Marshal(b)
	require.NoError(t, err)

	got, err := ParseBundle(raw)
	require.NoError(t, err)

	task := got.Tasks[0]
	assert.Equal(t, "fix[2J]0;pwned\nthe form", task.Asked, "a line break stays in a text of lines; the escape, the bell and the reordering mark go")
	assert.Equal(t, "note[2J]0;pwned\n\tindented", task.Note)
	assert.Equal(t, "Form[2J]0;pwned second line", task.Title, "a line break is a space in a line")
	assert.Equal(t, "acme[2J]0;pwned", task.Project)
	assert.Equal(t, "https://example.com/[2J]0;pwned", task.URL)
	assert.Equal(t, []string{"it saves[2J]0;pwned  and more"}, task.Criteria)

	require.NotNil(t, task.Checkpoint)
	assert.Equal(t, "dev[2J]0;pwned", task.Checkpoint.By)
	assert.Equal(t, "section[2J]0;pwned\nline two", task.Checkpoint.Values["summary"])
	assert.Equal(t, "what[2J]0;pwned", task.Checkpoint.Shape.Sections[0].Guidance)

	for _, v := range append([]string{task.Asked, task.Note, task.Title, task.Project, task.URL, task.Checkpoint.By}, task.Criteria...) {
		assert.NotContains(t, v, "\x1b")
		assert.NotContains(t, v, "\x07")
		assert.NotContains(t, v, "\u202e")
	}
}

// One step of a role's move runs at a node at a time: a second one is told it is busy, or waits for the first, and the lock is free again
// when it is given back.
func TestLockExcludesAndYieldsWhenGivenBack(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}

	release, err := Lock(l, "dev", 0)
	require.NoError(t, err)

	_, err = Lock(l, "dev", 0)
	require.ErrorIs(t, err, ErrBusy)

	start := time.Now()
	_, err = Lock(l, "dev", 150*time.Millisecond)
	require.ErrorIs(t, err, ErrBusy)
	assert.GreaterOrEqual(t, time.Since(start), 140*time.Millisecond, "it waited")

	other, err := Lock(l, "ops", 0)
	require.NoError(t, err, "another role's move is another lock")
	other()

	go func() {
		time.Sleep(80 * time.Millisecond)
		release()
	}()

	again, err := Lock(l, "dev", 5*time.Second)
	require.NoError(t, err, "a waiting step gets the lock when the first gives it back")
	again()

	third, err := Lock(l, "dev", 0)
	require.NoError(t, err)
	third()
}

func TestTakingMarkerAndTombstone(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}

	_, ok, err := OpenTaking(l, "dev")
	require.NoError(t, err)
	assert.False(t, ok)

	want := Taking{ID: "mv-0123456789ab", Role: "dev", From: "alpha", Started: time.Now().UTC().Truncate(time.Second), Origins: []string{"alpha/T-2"},
		Repos: []string{"/home/alex/work/acme"}}
	require.NoError(t, SaveTaking(l, want))

	got, ok, err := OpenTaking(l, "dev")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, want, got)

	require.NoError(t, CloseTaking(l, "dev"))
	require.NoError(t, CloseTaking(l, "dev"), "closed twice is closed")

	require.NoError(t, os.MkdirAll(l.Moves(), 0o700))
	require.NoError(t, os.WriteFile(l.MoveTaking("dev"), []byte("{"), 0o600))

	_, _, err = OpenTaking(l, "dev")
	require.ErrorContains(t, err, "does not read")

	gone, err := Aborted(l, "mv-0123456789ab")
	require.NoError(t, err)
	assert.False(t, gone)

	require.NoError(t, SaveAborted(l, Tombstone{ID: "mv-0123456789ab", Role: "dev", At: time.Now()}))

	gone, err = Aborted(l, "mv-0123456789ab")
	require.NoError(t, err)
	assert.True(t, gone)

	gone, err = Aborted(l, "mv-aaaaaaaaaaaa")
	require.NoError(t, err)
	assert.False(t, gone, "by id")

	_, err = Aborted(l, "../x")
	require.Error(t, err)
	require.Error(t, SaveAborted(l, Tombstone{ID: "../../x"}))
}
