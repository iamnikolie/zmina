package cmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
)

func question(items ...chat.QuestionItem) *chat.Question { return &chat.Question{ID: "toolu_1", Items: items} }

func item(multi bool, n int) chat.QuestionItem {
	it := chat.QuestionItem{Question: "q", MultiSelect: multi}
	for range n {
		it.Options = append(it.Options, chat.QuestionOption{Label: "o"})
	}
	return it
}

func answer(t *testing.T, raw string) questionAnswer {
	t.Helper()
	var a questionAnswer
	require.NoError(t, json.Unmarshal([]byte(raw), &a))
	return a
}

// The keys were read off Claude Code's own dialog: a digit picks and moves on, digits tick and Tab moves on, the
// digit after the options is «Type something», and a question of several parts ends on a review whose 1 submits.
func TestAnswerStepsFollowTheDialog(t *testing.T) {
	q := question(item(false, 2), item(true, 3))
	steps, err := answerSteps(q, answer(t, `{"id":"toolu_1","items":[{"choices":[2]},{"choices":[1,3]}]}`))
	require.NoError(t, err)
	o2, o3 := []string{"o", "o"}, []string{"o", "o", "o"}
	assert.Equal(t, []answerStep{{Keys: []string{"2"}, Expect: "q", Options: o2}, {Want: []int{1, 3}, Keys: []string{"Tab"}, Expect: "q", Options: o3},
		{Keys: []string{"1"}, Expect: "Review your answers", Options: []string{"Submit answers", "Cancel"}}}, steps, "each part is checked on the screen first")

	steps, err = answerSteps(question(item(false, 2)), answer(t, `{"id":"toolu_1","items":[{"other":"my own words"}]}`))
	require.NoError(t, err)
	assert.Equal(t, []answerStep{{Keys: []string{"3"}, Expect: "q", Options: []string{"o", "o"}}, {Text: "my own words", Expect: "q"}}, steps,
		"one part: no review screen; the words are typed only while the question is still up")
}

func TestAnswerStepsRefuseWhatTheDialogCannotTake(t *testing.T) {
	cases := map[string]struct {
		q   *chat.Question
		raw string
	}{
		"another question":        {question(item(false, 2)), `{"id":"toolu_2","items":[{"choices":[1]}]}`},
		"parts missing":           {question(item(false, 2), item(false, 2)), `{"id":"toolu_1","items":[{"choices":[1]}]}`},
		"no such option":          {question(item(false, 2)), `{"id":"toolu_1","items":[{"choices":[3]}]}`},
		"two picks on one choice": {question(item(false, 2)), `{"id":"toolu_1","items":[{"choices":[1,2]}]}`},
		"nothing picked":          {question(item(false, 2)), `{"id":"toolu_1","items":[{}]}`},
		"words with ticks":        {question(item(true, 2), item(false, 2)), `{"id":"toolu_1","items":[{"choices":[1],"other":"x"},{"choices":[1]}]}`},
		"a lone ticks question":   {question(item(true, 2)), `{"id":"toolu_1","items":[{"choices":[1]}]}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := answerSteps(c.q, answer(t, c.raw))
			assert.Error(t, err)
		})
	}
}

func TestOnScreenReadsThroughWrapping(t *testing.T) {
	screen := "←  ☐ A  ☐ B  ✔ Submit  →\nzmina на телефоне: с какого экрана\n  открываться и какие вкладки?\n❯ 1. Agents"
	assert.True(t, onScreen(screen, "zmina на телефоне: с какого экрана открываться и какие вкладки?"))
	assert.False(t, onScreen("❯ ordinary prompt", "zmina на телефоне: с какого экрана"))
	assert.False(t, onScreen(screen, ""))
	assert.False(t, onScreen(screen, "zmina на телефоне: с какого экрана открываться и какие вкладки оставить?"), "the whole question, not its start")
	assert.True(t, onScreen(screen, "zmina на телефоне: с какого экрана открываться и какие вкладки?", "Agents"))
	assert.False(t, onScreen(screen, "zmina на телефоне: с какого экрана открываться и какие вкладки?", "Agents", "Move"), "every option")
}

// The dialog read off a live session: the question, then numbered rows; ticks show as [✔].
const liveDialog = `old text: Which tabs? 1. Talk 2. Later
←  ☒ A  ☐ B  ✔ Submit  →
Which tabs?
❯ 1. [✔] Talk
         the chat
  2. [ ] Later
  3. [ ] Type something
     Submit
  4. Chat about this`

func TestActiveDialogReadsTheBottomQuestion(t *testing.T) {
	rows, ok := activeDialog(liveDialog, "Which tabs?")
	require.True(t, ok)
	require.GreaterOrEqual(t, len(rows), 2)
	assert.Equal(t, dialogOption{N: 1, Label: "Talk", Ticked: true, Current: true}, rows[0])
	assert.False(t, rows[1].Ticked)
	assert.True(t, dialogMatches(rows, []string{"Talk", "Later"}))
	assert.False(t, dialogMatches(rows, []string{"Later", "Talk"}), "options in another order are another question")

	_, ok = activeDialog(liveDialog, "Which screen?")
	assert.False(t, ok)
}
