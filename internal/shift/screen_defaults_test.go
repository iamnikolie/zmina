package shift

import (
	"time"

	"github.com/iamnikolie/zmina/internal/screenrules"
)

// The readers as the golden tests call them: by the embedded table, as a shift with no crew rules of its own reads. Production
// code never calls these; it reads by its shift's table (Shift.ScreenRules).

func NowLine(screen string) string { return nowLine(screenrules.Default(), screen) }

func ParseDialog(screen string) *Dialog { return parseDialog(screenrules.Default(), screen) }

func WaitGone(t Tmux, names []string, timeout time.Duration) {
	waitGone(screenrules.Default(), t, names, timeout)
}

func answerExit(t Tmux, name string) bool { return pressExit(screenrules.Default(), t, name) }

// screenrulesWith is a table of the rows given, with none of the protections of a crew's override file.
func screenrulesWith(rows ...screenrules.Rule) (*screenrules.Table, error) {
	return screenrules.NewTable(rows)
}
