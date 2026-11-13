package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// withLimitWording puts a screen-rules.yaml in the rig's crew directory that reads a wording as a usage-limit notice, under
// the id given.
func (r *explainRig) withLimitWording(id, wording string) {
	r.t.Helper()

	body := "rules:\n  - id: " + id + "\n    engine: claude\n    sets: usage_limit\n    means: a wording this crew's CLI uses\n" +
		"    match: {contains: \"" + wording + "\"}\n"
	require.NoError(r.t, os.WriteFile(filepath.Join(filepath.Dir(r.crewFile), "screen-rules.yaml"), []byte(body), 0o600))
}

// shiftOf is the shift a command of this rig would build for its crew, as zmina ui builds one for each crew it serves.
func (r *explainRig) shiftOf() *shift.Shift {
	r.t.Helper()

	a := &app{out: os.Stderr, err: os.Stderr, crew: r.crewFile, shiftSeams: r.seams()}

	s, err := a.newShift()
	require.NoError(r.t, err)

	return s
}

// limitKind is the id of the rule that reads the rig's screen as a usage-limit notice, "" when none does.
func limitKind(s *shift.Shift) string {
	v, why, ok := s.Look("dev")
	if !ok {
		return "!"
	}

	if l := s.Explain(v, why).Screen.Limit; l != nil {
		return l.Kind
	}

	return ""
}

// zmina ui serves several crews in one process: the screen rules of one crew are never applied to another crew's panes. Two
// shifts built one after the other for crews with different screen-rules.yaml each read with their own.
func TestTwoCrewsInOneProcessReadTheirOwnScreenRules(t *testing.T) {
	alpha, beta := newExplainRig(t), newExplainRig(t)
	alpha.withLimitWording("acme.alpha", "alpha exhausted")
	beta.withLimitWording("acme.beta", "beta exhausted")

	alpha.session("idle", "")
	beta.session("idle", "")

	sa, sb := alpha.shiftOf(), beta.shiftOf()

	alpha.tm.screen = "⎿ alpha exhausted until 9pm"
	beta.tm.screen = "⎿ beta exhausted until 9pm"
	assert.Equal(t, "acme.alpha", limitKind(sa), "the first crew reads its own wording after the second crew's shift was built")
	assert.Equal(t, "acme.beta", limitKind(sb))

	// Each crew's wording is no notice in the other's panes.
	alpha.tm.screen = "⎿ beta exhausted until 9pm"
	beta.tm.screen = "⎿ alpha exhausted until 9pm"
	assert.Empty(t, limitKind(sa))
	assert.Empty(t, limitKind(sb))
}

// The same, read concurrently (run with -race): the table is the shift's own and nothing is shared between two crews.
func TestTwoCrewsReadTheirOwnScreenRulesConcurrently(t *testing.T) {
	alpha, beta := newExplainRig(t), newExplainRig(t)
	alpha.withLimitWording("acme.alpha", "alpha exhausted")
	beta.withLimitWording("acme.beta", "beta exhausted")

	alpha.session("idle", "")
	beta.session("idle", "")

	alpha.tm.screen = "⎿ alpha exhausted until 9pm"
	beta.tm.screen = "⎿ beta exhausted until 9pm"

	var wg sync.WaitGroup

	for _, c := range []struct {
		rig  *explainRig
		want string
	}{{alpha, "acme.alpha"}, {beta, "acme.beta"}} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 100 {
				// Every pass builds a shift for its crew, as one page call does.
				assert.Equal(t, c.want, limitKind(c.rig.shiftOf()))
			}
		}()
	}

	wg.Wait()
}

// No code reads a screen by a table kept for the process: the embedded table is named only where a crew's own is built from it,
// and by the one accessor each shift and reader has. (The tables are the shift's, so that a process serving several crews
// never reads one crew's panes by another's rows.)
func TestNoReaderUsesATableKeptForTheProcess(t *testing.T) {
	allowed := map[string]int{
		"../internal/shift/shift.go": 1, // Shift.ScreenRules: the embedded table when the shift has no crew rules
		"screen_reader.go":           1, // screenReader.table: the same, for a reader built with none
		"root.go":                    1, // newShift: the base the crew's file is read over
		"screen_rules.go":            1, // doctor: the base the crew's file is read over
	}

	var found = map[string]int{}

	for _, dir := range []string{".", "../internal/shift"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)

		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}

			raw, err := os.ReadFile(f)
			require.NoError(t, err)

			src := string(raw)
			assert.NotContains(t, src, "screenrules.Active", f)
			assert.NotContains(t, src, "screenrules.Use(", f)

			if n := strings.Count(src, "screenrules.Default()"); n > 0 {
				found[filepath.ToSlash(f)] = n
			}
		}
	}

	assert.Equal(t, allowed, found)
}
