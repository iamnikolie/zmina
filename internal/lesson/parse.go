package lesson

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// headingRe is the line that opens a lesson: ### and the rule. Four #, or none after them, are not it.
	headingRe = regexp.MustCompile(`^ {0,3}###(?:[ \t]+(.*))?$`)
	// keyRe is a key line: Why, Applies to (Applies alone is read too) or In a file already, then a colon.
	keyRe = regexp.MustCompile(`(?i)^\s*(?:(why)|(applies(?:\s+to)?)|(in\s+a\s+file\s+already))\s*:\s*(.*)$`)
	// breakRe is a horizontal rule, the line writers put between blocks; it ends the Why above it.
	breakRe = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
)

// ParseMarkdown reads the lessons of a text, the format the roles write:
//
//	### <the rule, one line>
//	Why: <the reason; it goes on over the following lines until the next key or ###>
//	Applies to: every seat | this role | one task | not sure
//	In a file already: <path> | no
//
// Text before the first ### is left out. Keys may be in any case. The rule, Why and Applies to are required; In a
// file already may be missing, and no, none, - or nothing mean no file. A Why keeps its line breaks. An error names
// the line of the block's ### and what is wrong, and then no lesson is returned: all or nothing, so a text with one
// bad block is written again whole, never half-recorded.
func ParseMarkdown(text string) ([]Lesson, error) {
	var (
		out []Lesson
		cur *block
	)

	flush := func() error {
		if cur == nil {
			return nil
		}

		l, err := cur.lesson()
		if err != nil {
			return err
		}

		out = append(out, l)
		cur = nil

		return nil
	}

	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")

		if m := headingRe.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}

			cur = &block{line: i + 1, rule: strings.TrimSpace(m[1])}

			continue
		}

		if cur == nil {
			continue
		}

		if err := cur.take(line); err != nil {
			return nil, err
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}

	return out, nil
}

// block is a lesson while it is being read.
type block struct {
	line    int
	rule    string
	why     []string
	applies string
	inFile  string

	hasWhy, hasApplies, hasInFile bool
	// inWhy: the lines that follow are still the reason.
	inWhy bool
}

// take reads one line of the block: a key, a break, or more of the Why. Anything else is left out.
func (b *block) take(line string) error {
	if breakRe.MatchString(line) {
		b.inWhy = false

		return nil
	}

	m := keyRe.FindStringSubmatch(line)
	if m == nil {
		if b.inWhy {
			b.why = append(b.why, strings.TrimRight(line, " \t"))
		}

		return nil
	}

	b.inWhy = false
	value := strings.TrimSpace(m[4])

	switch {
	case m[1] != "":
		if b.hasWhy {
			return fmt.Errorf("line %d: Why twice", b.line)
		}

		b.hasWhy, b.inWhy = true, true
		b.why = append(b.why, value)
	case m[2] != "":
		if b.hasApplies {
			return fmt.Errorf("line %d: Applies to twice", b.line)
		}

		b.hasApplies, b.applies = true, value
	default:
		if b.hasInFile {
			return fmt.Errorf("line %d: In a file already twice", b.line)
		}

		b.hasInFile, b.inFile = true, value
	}

	return nil
}

// lesson checks the block and makes the lesson of it.
func (b *block) lesson() (Lesson, error) {
	if b.rule == "" {
		return Lesson{}, fmt.Errorf("line %d: no rule after ###", b.line)
	}

	why := strings.TrimSpace(strings.Join(b.why, "\n"))
	if why == "" {
		return Lesson{}, fmt.Errorf("line %d: no Why", b.line)
	}

	if b.applies == "" {
		return Lesson{}, fmt.Errorf("line %d: no Applies to", b.line)
	}

	applies, err := ParseApplies(b.applies)
	if err != nil {
		return Lesson{}, fmt.Errorf("line %d: %w", b.line, err)
	}

	return Lesson{Rule: b.rule, Why: why, Applies: applies, InFile: filePath(b.inFile)}, nil
}

// filePath is the path a writer gave, or "" when it says there is none; backticks around it are dropped.
func filePath(s string) string {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "`"))

	switch strings.ToLower(s) {
	case "", "no", "none", "-":
		return ""
	}

	return s
}
