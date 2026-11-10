package cmd

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skill(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var out, errb bytes.Buffer

	root := NewRoot(&out, &errb)
	root.SetArgs(append([]string{"skill"}, args...))
	err := root.Execute()

	return out.String(), errb.String(), err
}

// A role reads zmina skill at boot as one tool result, cut near 30k characters: what it prints with no topic fits,
// lists every topic and carries the dispatcher's section whole.
func TestSkillFrontFitsOneResultAndListsEveryTopic(t *testing.T) {
	out, _, err := skill(t)
	require.NoError(t, err)

	assert.LessOrEqual(t, utf8.RuneCountInString(out), skillCap)
	assert.Contains(t, out, "## For the dispatcher\n")

	ref := parseSkill(skillDoc)
	for _, topic := range ref.Topics {
		assert.Contains(t, out, "`"+topic.Key+"`", topic.Heading)
	}
}

// Every topic prints alone within one result, and every section a role needs by name is reachable.
func TestSkillEveryTopicPrintsWithinTheCap(t *testing.T) {
	ref := parseSkill(skillDoc)
	seen := map[string]bool{}

	for _, topic := range ref.Topics {
		assert.False(t, seen[topic.Key], "two topics are named %s", topic.Key)
		seen[topic.Key] = true

		out, _, err := skill(t, topic.Key)
		require.NoError(t, err, topic.Key)
		assert.True(t, strings.HasPrefix(out, strings.Repeat("#", topic.Level)+" "+topic.Heading+"\n"), topic.Key)
		assert.LessOrEqual(t, utf8.RuneCountInString(out), skillCap, topic.Key)
	}

	for _, asked := range []string{"tasks", "todo", "for-the-dispatcher", "gotchas", "later"} {
		assert.True(t, seen[asked], "no topic %s", asked)
	}
}

// The topics cover the reference: the introduction and the sections put together are the whole file.
func TestSkillTopicsCoverTheWholeReference(t *testing.T) {
	ref := parseSkill(skillDoc)

	whole := ref.Intro
	for _, topic := range ref.Topics {
		if topic.Level == 2 {
			whole += topic.Text
		}
	}

	assert.Equal(t, skillDoc, whole)

	out, _, err := skill(t, "all")
	require.NoError(t, err)
	assert.Equal(t, skillDoc, out)
}

func TestSkillFindsATopicByAWordOfIt(t *testing.T) {
	out, _, err := skill(t, "task")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "## Tasks"), "a word naming a section and some parts means the section")

	out, _, err = skill(t, "Dispatcher")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "## For the dispatcher"))

	out, _, err = skill(t, "tasks", "todo")
	require.NoError(t, err)
	assert.Contains(t, out, "## Tasks")
	assert.Contains(t, out, "## Todo")

	_, _, err = skill(t, "nosuchtopic")
	require.ErrorContains(t, err, `no topic "nosuchtopic"`)
}

// A section over the cap prints its start and names its parts, each of which prints.
func TestSkillLongSectionNamesItsParts(t *testing.T) {
	doc := "# ref\n\nintro\n\n## Big one\n\nstart\n\n### Part a\n\n" + strings.Repeat("a", skillCap) +
		"\n\n### Part b\n\nb\n\n## Small — after\n\n```\n## not a heading\n```\n"
	ref := parseSkill(doc)
	require.Len(t, ref.Topics, 4)

	big, err := ref.find("big-one")
	require.NoError(t, err)

	out := ref.print(big)
	assert.Contains(t, out, "start")
	assert.Contains(t, out, "`part-a`")
	assert.Contains(t, out, "`part-b`")
	assert.NotContains(t, out, strings.Repeat("a", 100))

	small, err := ref.find("small")
	require.NoError(t, err)
	assert.Contains(t, ref.print(small), "## not a heading", "a heading inside a code fence stays text")

	_, err = ref.find("part")
	require.ErrorContains(t, err, "part-a, part-b")
}
