package cmd

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

func question(items ...chat.QuestionItem) *chat.Question {
	return &chat.Question{ID: "toolu_1", Items: items}
}

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
	assert.Equal(t, []answerStep{{Keys: []string{"3"}, Expect: "q", Options: []string{"o", "o"}}, {Text: "my own words", Expect: "q", Options: []string{"o", "o"}, Current: 3}}, steps,
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

	// An old question's text above another dialog: the rows that follow restart at 1, so it is not the active one.
	stale := "Which tabs?\n  1. Talk\n  2. Later\nDeploy now?\n❯ 1. Yes\n  2. No"
	_, ok = activeDialog(stale, "Which tabs?")
	assert.False(t, ok, "old text above another dialog")
	_, ok = activeDialog("Which tabs?\n  1. Talk\n  2. Later", "Which tabs?")
	assert.False(t, ok, "rows without a cursor are not a live dialog")
}

// A long question as Claude Code 2.1.319 draws it: a rule down the left of its wrapped text, the rows under it, the
// last one past a rule of its own. Read off a live session with the words changed.
const barredDialog = `❯ Ask me which day suits the release. It is a real question: the reviewer is out of quota until Tuesday,
  10 November, 13:15, and the review goes to the second seat meanwhile.
────────────────────────────────────────────────────────────────────────────────────────────────────────────
 ☐ Release
│ It is a real question: the reviewer is out of quota until Tuesday, 10 November, 13:15, and the review goes to
│ the second seat meanwhile. Wait for Tuesday or buy more credits?
❯ 1. Wait for Tuesday (Recommended)
     The second seat reviews until Tuesday, no extra spend
  2. Buy credits
     You buy them on the provider's usage page, then the reviewer comes back
  3. Type something.
────────────────────────────────────────────────────────────────────────────────────────────────────────────
  4. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel`

func TestActiveDialogReadsThroughTheQuestionsRule(t *testing.T) {
	q := "It is a real question: the reviewer is out of quota until Tuesday, 10 November, 13:15, and the review goes to the second seat meanwhile. Wait for Tuesday or buy more credits?"
	rows, ok := activeDialog(barredDialog, q)
	require.True(t, ok, "the rule is the dialog's frame, not words of the question")
	assert.True(t, dialogMatches(rows, []string{"Wait for Tuesday (Recommended)", "Buy credits"}))
	assert.True(t, rows[0].Current)
	assert.True(t, onScreen(barredDialog, q, "Buy credits"))
	assert.Equal(t, []string{"a │ b"}, dialogLines("│ a │ b │"), "a bar inside the words stays")

	// The rule is the question's frame only: rows inside a box are another dialog's, as they were before.
	boxed := "⏺ " + q + "\n╭──────────╮\n│ Do you want to proceed? │\n│ ❯ 1. Wait for Tuesday (Recommended) │\n│   2. Buy credits │\n╰──────────╯"
	_, ok = activeDialog(boxed, q)
	assert.False(t, ok, "a boxed dialog under the question's text is not the question's")
}

// dialogScreen is a role's tmux with a question dialog on its screen: no input line, the screen given, keys kept.
type dialogScreen struct {
	shift.Tmux
	screen string
	keys   []string
}

func (d *dialogScreen) Probe(string) (tmux.Pane, error)     { return tmux.Pane{State: tmux.Up}, nil }
func (d *dialogScreen) Prompt(string) (string, bool, error) { return "", false, nil }
func (d *dialogScreen) Capture(string, int) (string, error) { return d.screen, nil }
func (d *dialogScreen) Keys(_ string, keys ...string) error {
	d.keys = append(d.keys, keys...)
	return nil
}

// An answer stopped before its first key is no answer: the owner may send it again once the screen reads right.
func TestAnswerStoppedBeforeItsFirstKeyMayBeSentAgain(t *testing.T) {
	uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	c, err := a.crewApp("default")
	require.NoError(t, err)
	q := "It is a real question: the reviewer is out of quota until Tuesday, 10 November, 13:15, and the review goes to the second seat meanwhile. Wait for Tuesday or buy more credits?"
	snapshot := chat.Snapshot{NeedsInput: true, Question: &chat.Question{ID: "toolu_1", Items: []chat.QuestionItem{{Question: q,
		Options: []chat.QuestionOption{{Label: "Wait for Tuesday (Recommended)"}, {Label: "Buy credits"}}}}}}
	raw := `{"id":"toolu_1","items":[{"choices":[1],"other":""}]}`
	screen := &dialogScreen{screen: "Another question?\n❯ 1. Yes\n  2. No"}
	require.NoError(t, c.withState(func(s *shift.Shift) error {
		s.Tmux = screen
		require.ErrorContains(t, answerQuestion(s, "dev", raw, snapshot), "not the one answered")
		require.Empty(t, screen.keys)
		require.Empty(t, s.State.Role("dev").AnsweredQuestions, "nothing reached the dialog")

		screen.screen = barredDialog
		require.NoError(t, answerQuestion(s, "dev", raw, snapshot))
		require.Equal(t, []string{"1"}, screen.keys)
		require.ErrorContains(t, answerQuestion(s, "dev", raw, snapshot), "answered from Chat already", "never twice")
		return nil
	}))
}
