package shift

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/screenrules"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// Dialog is a question Claude Code shows instead of the input line (AskUserQuestion, a permission prompt): its words and
// its numbered choices, read from the screen. Cursor is the choice the ❯ stands on.
type Dialog struct {
	Question string         `json:"question"`
	Options  []DialogOption `json:"options"`
	Cursor   int            `json:"cursor"`
	// ID is the dialog's fingerprint (its question and every choice): an answer names it, so it is never taken by
	// another question that happens to have the same choices. For a permission prompt whose tool call its hook recorded, the
	// fingerprint covers the call too: two commands never share one.
	ID string `json:"id"`
	// Tool and Subject are, for a permission prompt, the tool and what it was asked to do, from the hook's record; empty when
	// there is none, and then nothing says what is being approved (View.WaitingFor says it is a permission prompt).
	Tool    string `json:"tool,omitempty"`
	Subject string `json:"subject,omitempty"`
	// SubjectCut: the subject is not all of what is being approved (see AskedPermission.Cut).
	SubjectCut bool `json:"subject_cut,omitempty"`
	// Multi: several choices can be ticked, which Enter on one does not settle: it needs the terminal.
	Multi bool `json:"multi,omitempty"`
}

// DialogOption is one numbered choice.
type DialogOption struct {
	N     int    `json:"n"`
	Label string `json:"label"`
	// Detail is the description under the choice, the lines Claude Code draws indented beneath it.
	Detail string `json:"detail,omitempty"`
}

// optionOf reads a screen row as a numbered choice: the row's match with its groups (1 the cursor, 2 the number, 3 the
// words), nil when it is none. What a choice looks like is the Claude rules' (claude.dialog.option).
func optionOf(rules *screenrules.Table, line string) []string {
	hit, ok := rules.Test(screenrules.Claude, screenrules.DialogOption, line)
	if !ok {
		return nil
	}

	return hit.Sub
}

// ParseDialog reads, by this crew's rules, the last numbered list on a screen and the words above it; nil when there is none of two or more
// choices numbered from 1. The words that mark a choice, the footer that closes a live dialog, the choices that ask for
// words and the title mark are the Claude rules' (claude.dialog.*); the geometry is this function's
// (claude.dialog.parse).
func (s *Shift) ParseDialog(screen string) *Dialog { return parseDialog(s.ScreenRules(), screen) }

func parseDialog(rules *screenrules.Table, screen string) *Dialog {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")

	first, last := -1, -1
	var opts []DialogOption
	cursor := 0

	for i := len(lines) - 1; i >= 0; i-- {
		m := optionOf(rules, lines[i])
		if m == nil {
			if first >= 0 && strings.TrimSpace(lines[i]) != "" && !strings.HasPrefix(lines[i], "     ") {
				break // the words above the list
			}

			continue
		}

		n, _ := strconv.Atoi(m[2])
		if len(opts) > 0 && n != opts[0].N-1 {
			break
		}

		opts = append([]DialogOption{{N: n, Label: m[3]}}, opts...)
		if m[1] != "" {
			cursor = n
		}

		first = i
		if last < 0 {
			last = i
		}

		if n == 1 {
			break
		}
	}

	// A live dialog, not a numbered list in the conversation: the ❯ stands on a choice, and the screen ends with the
	// dialog's own footer (nothing but choice descriptions and blank lines between).
	if len(opts) < 2 || opts[0].N != 1 || cursor == 0 {
		return nil
	}

	// The footer is found by where it stands, never by words a description may also hold: the screen's last
	// non-blank line, not indented as a description is. Between the last choice and it, only descriptions and blanks.
	end := len(lines) - 1
	for end > last && strings.TrimSpace(lines[end]) == "" {
		end--
	}

	if end <= last || isDetail(lines[end]) {
		return nil
	}

	if _, footerWords := rules.Test(screenrules.Claude, screenrules.DialogFooter, lines[end]); !footerWords {
		return nil
	}

	for _, l := range lines[last+1 : end] {
		if strings.TrimSpace(l) != "" && !isDetail(l) {
			return nil
		}
	}

	// Each choice's description: the indented lines under it, up to the next choice or the footer, whatever they say.
	at := -1
	for _, l := range lines[first:end] {
		if m := optionOf(rules, l); m != nil {
			n, _ := strconv.Atoi(m[2])
			at = n - 1
			continue
		}
		if t := strings.TrimSpace(l); t != "" && isDetail(l) && at >= 0 && at < len(opts) {
			opts[at].Detail = strings.TrimSpace(opts[at].Detail + " " + t)
		}
	}

	d := &Dialog{Cursor: cursor}
	for _, o := range opts {
		if _, free := rules.Test(screenrules.Claude, screenrules.DialogFree, o.Label); !free {
			d.Options = append(d.Options, o)
		}
	}

	// The question: the lines above the list, up to a blank line or the dialog's title (☐), without its frame.
	var q []string
	for i := first - 1; i >= 0 && i >= first-12; i-- {
		l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "│|"))
		if l == "" {
			if len(q) > 0 {
				break
			}

			continue
		}

		if title, ok := rules.Test(screenrules.Claude, screenrules.DialogTitle, l); ok {
			q = append([]string{strings.TrimSpace(title.Rest)}, q...)

			break
		}

		q = append([]string{l}, q...)
	}

	d.Question = strings.Join(q, " ")

	if len(d.Options) == 0 {
		return nil
	}

	d.ID = dialogID(d.Question, opts)

	return d
}

// dialogOf is the dialog on a role's screen as the role's session and hook records complete it: what it waits on, and for a permission
// prompt whose call the hook recorded, the tool and its subject, folded into the fingerprint. nil when the screen shows none.
func (s *Shift) dialogOf(name string, sess *registry.Session, screen string) *Dialog {
	d := s.ParseDialog(screen)
	if d == nil {
		return nil
	}

	if p := s.permissionLook(name, sess); p != nil && permissionOnScreen(p, screen) {
		d.Tool, d.Subject, d.SubjectCut = p.Tool, p.Input, p.Cut
		d.ID = permissionDialogID(p, d.ID)
	}

	d.Multi = boxedChoices(d.Options)

	return d
}

// DialogOn reads the dialog on a screen just captured for a role, as its view does: the same fingerprint, so that a press can
// be held to the dialog the owner was shown. nil when the screen shows none, or the role is not up.
func (s *Shift) DialogOn(name, screen string) *Dialog {
	p, err := s.probe(name)
	if err != nil || p.State != tmux.Up {
		return nil
	}

	sess, _ := s.registryFor(name).ByPID(p.PID)

	return s.dialogOf(name, sess, screen)
}

// permissionDialogID is the fingerprint of a permission prompt: the screen's, with the tool and a sum of its whole input.
func permissionDialogID(p *AskedPermission, screen string) string {
	sum := sha256.Sum256([]byte("permission\x00" + p.Tool + "\x00" + p.InputSum + "\x00" + screen))

	return hex.EncodeToString(sum[:])[:16]
}

// boxedChoices reports whether every choice is drawn with a box before it: a question whose choices are ticked, not taken.
func boxedChoices(opts []DialogOption) bool {
	if len(opts) == 0 {
		return false
	}

	for _, o := range opts {
		if !boxed(o.Label) {
			return false
		}
	}

	return true
}

func boxed(label string) bool {
	for _, b := range []string{"☐", "☑", "☒", "◻", "◼", "▢", "[ ]", "[x]", "[X]"} {
		if strings.HasPrefix(label, b) {
			return true
		}
	}

	return false
}

// dialogID is the fingerprint of a question and every choice with its description.
func dialogID(question string, opts []DialogOption) string {
	sum := sha256.New()
	sum.Write([]byte(question))

	for _, o := range opts {
		fmt.Fprintf(sum, "\x00%d\x00%s\x00%s", o.N, o.Label, o.Detail)
	}

	return hex.EncodeToString(sum.Sum(nil))[:16]
}

// ChooseKeys are the keys that move the dialog's cursor to choice n and take it.
func (d *Dialog) ChooseKeys(n int) []string {
	from := d.Cursor
	if from == 0 {
		from = 1
	}

	var keys []string

	for ; from < n; from++ {
		keys = append(keys, "Down")
	}

	for ; from > n; from-- {
		keys = append(keys, "Up")
	}

	return append(keys, "Enter")
}

// isDetail reports a line drawn as a choice's description: indented past the choices' own numbers.
func isDetail(l string) bool { return strings.HasPrefix(l, "     ") }
