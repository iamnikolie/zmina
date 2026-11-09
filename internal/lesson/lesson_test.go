package lesson

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var lessonAt = time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)

func TestAddNumbersAboveEveryFile(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "lessons")}

	a, err := s.Add(Lesson{Rule: "run make test", Why: "CI is strict", Applies: Every}, lessonAt)
	require.NoError(t, err)
	assert.Equal(t, "LS-1", a.ID)
	assert.Equal(t, New, a.State)
	assert.True(t, a.At.Equal(lessonAt))
	assert.True(t, a.Updated.Equal(lessonAt))

	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "LS-7.json"), []byte("{broken"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "notes.txt"), []byte("not a lesson"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "LS-30.json.tmp"), []byte("half"), 0o600))

	b, err := s.Add(Lesson{Rule: "quote zsh globs", Why: "no matches found", Applies: Role}, lessonAt.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, "LS-8", b.ID, "numbered above every file, readable or not; other names do not count")
}

func TestAddKeepsTheWritersFieldsAndMakesTheState(t *testing.T) {
	s := &Store{Dir: t.TempDir()}

	got, err := s.Add(Lesson{
		Rule: "r", Why: "w", Applies: Task, InFile: "AGENTS.md", Role: "dev", Task: "T-1",
		State: Dropped, Verdict: "stale", MR: "!9", ID: "LS-99",
	}, lessonAt)
	require.NoError(t, err)
	assert.Equal(t, "LS-1", got.ID, "the id is the store's, not the caller's")
	assert.Equal(t, New, got.State)

	back, err := s.Get("1")
	require.NoError(t, err)
	assert.Equal(t, got, back)
	assert.Equal(t, "AGENTS.md", back.InFile)
	assert.Equal(t, "dev", back.Role)
	assert.Equal(t, "T-1", back.Task)
}

func TestAddNeverReplaces(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	keep := Lesson{ID: "LS-1", Rule: "keep me", Why: "w", Applies: Every, State: Merged}
	require.NoError(t, s.Put(keep))

	// The numbering would never pick LS-1 again; the write itself must still refuse the name.
	err := s.write(Lesson{ID: "LS-1", Rule: "intruder"}, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrExist)

	got, err := s.Get("LS-1")
	require.NoError(t, err)
	assert.Equal(t, "keep me", got.Rule)
	assert.Equal(t, Merged, got.State)

	empty := filepath.Join(s.Dir, "LS-2.json")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))

	next, err := s.Add(Lesson{Rule: "next"}, lessonAt)
	require.NoError(t, err)
	assert.Equal(t, "LS-3", next.ID, "an empty file holds its number too")
}

func TestStoreModes(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "made", "lessons")}

	l, err := s.Add(Lesson{Rule: "r", Why: "w", Applies: Every}, lessonAt)
	require.NoError(t, err)

	info, err := os.Stat(s.Dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the directory is made private")

	info, err = os.Stat(filepath.Join(s.Dir, "LS-1.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	l.Why = "changed"
	require.NoError(t, s.Put(l))

	info, err = os.Stat(filepath.Join(s.Dir, "LS-1.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a replaced file stays private")

	left, err := filepath.Glob(filepath.Join(s.Dir, "*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, left, "no temporary file is left behind")
}

func TestPutGetRoundTrip(t *testing.T) {
	s := &Store{Dir: t.TempDir()}

	l, err := s.Add(Lesson{Rule: "r", Why: "line one\nline two", Applies: Unsure}, lessonAt)
	require.NoError(t, err)

	l.State, l.Verdict, l.Reason, l.MR = InMR, "edit AGENTS.md", "it keeps coming up", "!12"
	l.Updated = lessonAt.Add(48 * time.Hour)
	require.NoError(t, s.Put(l))

	for _, id := range []string{"LS-1", "ls-1", "1", " 1 "} {
		got, err := s.Get(id)
		require.NoError(t, err, id)
		assert.Equal(t, l.Rule, got.Rule)
		assert.Equal(t, "line one\nline two", got.Why)
		assert.Equal(t, Unsure, got.Applies)
		assert.Equal(t, InMR, got.State)
		assert.Equal(t, "edit AGENTS.md", got.Verdict)
		assert.Equal(t, "it keeps coming up", got.Reason)
		assert.Equal(t, "!12", got.MR)
		assert.True(t, got.At.Equal(lessonAt), "At stays when the lesson is put back")
		assert.True(t, got.Updated.Equal(lessonAt.Add(48*time.Hour)))
	}
}

func TestPutKeepsAnUnknownState(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	require.NoError(t, s.Put(Lesson{ID: "LS-4", Rule: "r", State: "parked-by-a-newer-zmina"}))

	got, err := s.Get("4")
	require.NoError(t, err)
	assert.Equal(t, State("parked-by-a-newer-zmina"), got.State)
}

func TestGetErrors(t *testing.T) {
	s := &Store{Dir: t.TempDir()}

	_, err := s.Get("5")
	assert.EqualError(t, err, "no lesson LS-5")

	_, err = s.Get("lesson five")
	assert.ErrorContains(t, err, "no lesson")

	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "LS-3.json"), []byte("{broken"), 0o600))

	_, err = s.Get("3")
	assert.ErrorContains(t, err, "lesson LS-3", "a damaged file is named")

	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "LS-6.json"), []byte(`{"id":"LS-9"}`), 0o600))

	_, err = s.Get("6")
	assert.ErrorContains(t, err, "the file says it is")
}

func TestPutRefusesABadID(t *testing.T) {
	s := &Store{Dir: t.TempDir()}

	for _, id := range []string{"", "7", "ls-7", "LS-", "LS-7/../x", "A-7"} {
		assert.ErrorContains(t, s.Put(Lesson{ID: id}), "bad id", id)
	}

	entries, err := os.ReadDir(s.Dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestAllInOrderOfNumbers(t *testing.T) {
	s := &Store{Dir: t.TempDir()}

	for _, id := range []string{"LS-10", "LS-2", "LS-9", "LS-100", "LS-1"} {
		require.NoError(t, s.Put(Lesson{ID: id, Rule: id}))
	}

	all, err := s.All()
	require.NoError(t, err)

	var ids []string
	for _, l := range all {
		ids = append(ids, l.ID)
	}

	assert.Equal(t, []string{"LS-1", "LS-2", "LS-9", "LS-10", "LS-100"}, ids, "by number, not by name")
}

func TestAllSkipsWhatItCannotRead(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	require.NoError(t, s.Put(Lesson{ID: "LS-1", Rule: "one"}))
	require.NoError(t, s.Put(Lesson{ID: "LS-3", Rule: "three"}))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "LS-2.json"), []byte("{broken"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "LS-4.json"), []byte(`{"id":"LS-8"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "README.md"), []byte("hi"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(s.Dir, "LS-5.json"), 0o700))

	all, err := s.All()
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "one", all[0].Rule)
	assert.Equal(t, "three", all[1].Rule)
}

func TestAllWithoutADirectory(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "none")}

	all, err := s.All()
	require.NoError(t, err)
	assert.Empty(t, all)
}

func TestAllWhenTheDirIsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lessons")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	_, err := (&Store{Dir: path}).All()
	assert.Error(t, err)

	_, err = (&Store{Dir: path}).Add(Lesson{}, lessonAt)
	assert.Error(t, err)
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"7":       "LS-7",
		"ls-7":    "LS-7",
		"LS-7":    "LS-7",
		" 7 ":     "LS-7",
		" Ls-12 ": "LS-12",
		"":        "",
		"A-7":     "A-7",
		"-7":      "-7",
		"7x":      "7X",
	} {
		assert.Equal(t, want, Normalize(in), "%q", in)
	}
}

func TestParseApplies(t *testing.T) {
	for in, want := range map[string]Applies{
		"every seat":   Every,
		"Every Seat":   Every,
		"every":        Every,
		"all":          Every,
		"ALL":          Every,
		"this role":    Role,
		"role":         Role,
		"one role":     Role,
		"one task":     Task,
		"task":         Task,
		"not sure":     Unsure,
		"unsure":       Unsure,
		"  Not   Sure": Unsure,
		"\tthis role ": Role,
	} {
		got, err := ParseApplies(in)
		require.NoError(t, err, "%q", in)
		assert.Equal(t, want, got, "%q", in)
	}

	for _, in := range []string{"", "everyone", "every seats", "this seat", "maybe"} {
		got, err := ParseApplies(in)
		require.Error(t, err, "%q", in)
		assert.Empty(t, got)
		assert.ErrorContains(t, err, "every seat, this role, one task or not sure")
	}
}
