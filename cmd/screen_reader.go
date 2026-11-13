package cmd

import "github.com/iamnikolie/zmina/internal/screenrules"

// screenReader reads peek's footer and the browser chat's dialogs by one table of screen rules: the crew's own, which its
// shift carries (shift.ScreenRules). It is built where a shift is at hand, never from a table kept for the process, so that
// one process serving several crews reads each crew's panes by that crew's rules.
type screenReader struct{ rules *screenrules.Table }

// table is the rules read by; the embedded ones for a reader with none (what the golden tests build).
func (r screenReader) table() *screenrules.Table {
	if r.rules != nil {
		return r.rules
	}

	return screenrules.Default()
}
