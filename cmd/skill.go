package cmd

import (
	_ "embed"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

//go:embed skill.md
var skillDoc string

// skillCap is the most characters one print of the reference may have: a tool result is cut near 30k characters
// (Claude Code's default), and a role that reads a cut reference does not know what it missed.
const skillCap = 25000

// skillFront is the topic printed whole after the index, the one the dispatcher needs at every boot.
const skillFront = "for-the-dispatcher"

func (a *app) skillCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "skill [topic...]",
		Short: "Print the agent reference: its topics, one topic, or all of it",
		Long: `Print the agent reference for this CLI. With no topic: the introduction, the list of topics and the
dispatcher's section. With topics: those sections (a topic is matched by its name or a word of it, "task" finds
"tasks"). "all" prints the whole reference, over 200k characters: pipe it (zmina skill all | grep -n word),
a tool result is cut near 30k.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			doc := parseSkill(skillDoc)
			if len(args) == 0 {
				fmt.Fprint(a.out, doc.front())

				return nil
			}

			if len(args) == 1 && strings.EqualFold(args[0], "all") {
				fmt.Fprint(a.out, skillDoc)

				return nil
			}

			var out strings.Builder

			for _, arg := range args {
				t, err := doc.find(arg)
				if err != nil {
					return err
				}

				out.WriteString(doc.print(t))
			}

			if n := utf8.RuneCountInString(out.String()); n > skillCap {
				fmt.Fprintf(a.err, "zmina: %dk characters; a tool result is cut near 30k: ask for fewer topics at once\n", n/1000)
			}

			_, err := io.WriteString(a.out, out.String())

			return err
		},
	}
}

// skillTopic is one `##` or `###` section of the reference.
type skillTopic struct {
	Key, Heading string
	Level        int
	// Text is the heading line through the line before the next heading of the same or a higher level.
	Text string
	// Own is the heading line through the line before its first subsection (all of Text when it has none).
	Own  string
	Subs []int
}

type skillRef struct {
	Intro  string
	Topics []skillTopic
}

var skillKeyCut = regexp.MustCompile(`[^a-z0-9]+`)

// skillKey names a heading: the words before " — " or ":", lower case, without a leading "the", joined by dashes.
func skillKey(heading string) string {
	h := heading
	if i := strings.Index(h, " — "); i >= 0 {
		h = h[:i]
	}

	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}

	h = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), "the ")

	return strings.Trim(skillKeyCut.ReplaceAllString(h, "-"), "-")
}

// parseSkill splits the reference at its `##` and `###` headings. Lines inside code fences are never headings.
func parseSkill(doc string) skillRef {
	type mark struct {
		at, level int
		heading   string
	}

	var marks []mark

	fenced, at := false, 0

	for _, line := range strings.SplitAfter(doc, "\n") {
		trimmed := strings.TrimRight(line, "\n")

		switch {
		case strings.HasPrefix(trimmed, "```"):
			fenced = !fenced
		case !fenced && strings.HasPrefix(trimmed, "### "):
			marks = append(marks, mark{at, 3, strings.TrimPrefix(trimmed, "### ")})
		case !fenced && strings.HasPrefix(trimmed, "## "):
			marks = append(marks, mark{at, 2, strings.TrimPrefix(trimmed, "## ")})
		}

		at += len(line)
	}

	ref := skillRef{Intro: doc}
	if len(marks) > 0 {
		ref.Intro = doc[:marks[0].at]
	}

	parent := -1

	for i, m := range marks {
		end, own := len(doc), len(doc)

		for _, n := range marks[i+1:] {
			if own == len(doc) {
				own = n.at
			}

			if n.level <= m.level {
				end = n.at

				break
			}
		}

		own = min(own, end)
		t := skillTopic{Key: skillKey(m.heading), Heading: m.heading, Level: m.level, Text: doc[m.at:end], Own: doc[m.at:own]}

		if m.level == 2 {
			parent = len(ref.Topics)
		} else if parent >= 0 {
			ref.Topics[parent].Subs = append(ref.Topics[parent].Subs, len(ref.Topics))
		}

		ref.Topics = append(ref.Topics, t)
	}

	return ref
}

// find matches a topic by its key, else by a word of its key that starts with what was asked.
func (r skillRef) find(asked string) (skillTopic, error) {
	want := skillKey(asked)

	var hits []skillTopic

	for _, t := range r.Topics {
		if t.Key == want {
			return t, nil
		}

		for _, w := range strings.Split(t.Key, "-") {
			if want != "" && !strings.Contains(want, "-") && strings.HasPrefix(w, want) {
				hits = append(hits, t)

				break
			}
		}
	}

	if prefix := r.keyPrefix(want); len(hits) == 0 && prefix != nil {
		hits = prefix
	}

	// A word that names one whole section and some of its parts means the section: "task" is tasks.
	if len(hits) > 1 {
		var top []skillTopic

		for _, t := range hits {
			if t.Level == 2 {
				top = append(top, t)
			}
		}

		if len(top) == 1 {
			hits = top
		}
	}

	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return skillTopic{}, fmt.Errorf("no topic %q in the reference: zmina skill lists them", asked)
	default:
		keys := make([]string, len(hits))
		for i, t := range hits {
			keys[i] = t.Key
		}

		return skillTopic{}, fmt.Errorf("topic %q matches %s: name one", asked, strings.Join(keys, ", "))
	}
}

// keyPrefix is every topic whose key starts with want, for a multi-word ask such as "crew-f".
func (r skillRef) keyPrefix(want string) []skillTopic {
	if want == "" {
		return nil
	}

	var hits []skillTopic

	for _, t := range r.Topics {
		if strings.HasPrefix(t.Key, want) {
			hits = append(hits, t)
		}
	}

	return hits
}

// print is a topic whole, or, when that is over skillCap, its own text and the list of its subsections.
func (r skillRef) print(t skillTopic) string {
	if utf8.RuneCountInString(t.Text) <= skillCap || len(t.Subs) == 0 {
		return t.Text
	}

	var b strings.Builder

	b.WriteString(t.Own)
	fmt.Fprintf(&b, "This section goes on in %d parts, each printed with zmina skill <part>:\n\n", len(t.Subs))

	for _, i := range t.Subs {
		fmt.Fprintf(&b, "- `%s`: %s\n", r.Topics[i].Key, r.Topics[i].Heading)
	}

	b.WriteString("\n")

	return b.String()
}

// skillSize is a topic's length as the index shows it, in thousands of characters.
func skillSize(text string) string {
	n := utf8.RuneCountInString(text)
	if n < 1000 {
		return "<1k"
	}

	return fmt.Sprintf("%dk", (n+500)/1000)
}

// front is what zmina skill prints with no topic: the introduction, the index of topics and the dispatcher's
// section whole.
func (r skillRef) front() string {
	var b strings.Builder

	b.WriteString(r.Intro)
	b.WriteString("## Topics\n\n")
	b.WriteString("This is the start of the reference, not all of it. Print a topic with `zmina skill <topic>` (several at\n" +
		"once: `zmina skill tasks todo`; a word of a name finds it). `zmina skill all` prints everything, over 200k\n" +
		"characters, more than one tool result holds: pipe it (`zmina skill all | grep -n <word>`).\n\n")

	for _, t := range r.Topics {
		indent := ""
		if t.Level == 3 {
			indent = "  "
		}

		fmt.Fprintf(&b, "%s- `%s` (%s): %s\n", indent, t.Key, skillSize(t.Text), t.Heading)
	}

	b.WriteString("\n")

	for _, t := range r.Topics {
		if t.Key == skillFront {
			b.WriteString(r.print(t))
		}
	}

	return b.String()
}
