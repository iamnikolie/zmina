package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/shift"
)

// versionNoteEvery is how often the dispatcher hears of the same skew again while it lasts.
const versionNoteEvery = 24 * time.Hour

// codeSkew is one thing that runs the code of an earlier zmina and only a restart brings up to date: the cockpit, or
// a Codex role's bridge. Claude roles are not here: their hooks keep the command lines the earlier zmina wrote, which
// stay an API (TestHookCommandsResolve), so a restart can wait for the owner's pick; doctor lists them.
type codeSkew struct {
	key  string // what runs which version: the same skew has the same key from beat to beat
	text string
}

// codeSkews lists what runs another zmina's code, from the recorded cockpit and the roles up in st.
func codeSkews(c *crew.Crew, st *shift.State) []codeSkew {
	var out []codeSkew

	if r, ok := recordedCockpit(); ok && r.Version != version {
		out = append(out, codeSkew{key: "ui@" + r.Version, text: cockpitSkew(r)})
	}

	codex, _, _ := staleRoles(c, st)
	for _, r := range codex {
		out = append(out, codeSkew{key: "bridge:" + r.name + "@" + r.version, text: fmt.Sprintf(
			"the Codex bridge of %s runs the code of zmina %s, this zmina is %s: zmina restart %s --continue starts it "+
				"with this one", r.name, r.version, version, r.name)})
	}

	return out
}

// versionTold is versions.json next to the crew file: when the dispatcher was last told of each skew that still
// lasts. Its own small file, not a field of the state, so the beat's note needs no more of the state than a note.
type versionTold map[string]time.Time

func versionToldPath(dir string) string { return filepath.Join(dir, "versions.json") }

func readVersionTold(dir string) versionTold {
	told := versionTold{}

	if raw, err := os.ReadFile(versionToldPath(dir)); err == nil {
		_ = json.Unmarshal(raw, &told)
	}

	if told == nil {
		told = versionTold{}
	}

	return told
}

func writeVersionTold(dir string, told versionTold) error {
	raw, err := json.Marshal(told)
	if err != nil {
		return err
	}

	return fsx.WriteFile(versionToldPath(dir), append(raw, '\n'), 0o600)
}

// versionBeat tells the dispatcher what runs the code of an earlier zmina, once a day for each skew while it lasts.
// A skew that is gone is forgotten, so one that comes back after the next install is told at once. It runs after the
// beat's locked phases; the note goes through the health-note queue like the other beats', and the owner reads it from
// the dispatcher. The stamp is written before the note: a beat that cannot record it says nothing, rather than say it
// every minute.
func (a *app) versionBeat(now time.Time) {
	var (
		skews []codeSkew
		dir   string
	)

	if err := a.readState(func(s *shift.Shift) error {
		skews, dir = codeSkews(s.Crew, s.State), s.Crew.StateDir()

		return nil
	}); err != nil {
		return
	}

	told := readVersionTold(dir)
	next := versionTold{}

	var due []codeSkew

	for _, sk := range skews {
		at, was := told[sk.key]
		if !was || now.Sub(at) >= versionNoteEvery || at.After(now) {
			due = append(due, sk)
			next[sk.key] = now

			continue
		}

		next[sk.key] = at
	}

	if maps.EqualFunc(told, next, time.Time.Equal) {
		return
	}

	if err := writeVersionTold(dir, next); err != nil || len(due) == 0 {
		return
	}

	notes := make([]string, 0, len(due))
	for _, sk := range due {
		notes = append(notes, "version: "+sk.text)
	}

	_ = a.withState(func(s *shift.Shift) error {
		for _, n := range notes {
			s.Log("", "version-skew", "", n)
		}

		s.AddNotes(notes)

		return nil
	})

	a.printTick(notes)
}
