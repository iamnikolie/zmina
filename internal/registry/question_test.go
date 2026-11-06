package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const askLine = `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"Two questions."},` +
	`{"type":"tool_use","id":"toolu_1","name":"AskUserQuestion","input":{"questions":[` +
	`{"question":"Which screen first?","header":"Screen","multiSelect":false,"options":[` +
	`{"label":"Agents (Recommended)","description":"who waits on you"},{"label":"Move","description":"as today"}]},` +
	`{"question":"Which tabs?","header":"Tabs","multiSelect":true,"options":[{"label":"Talk"},{"label":"Later"}]}]}}]}}`

const answerLine = `{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Agents"}]}}`

func TestPendingQuestionIsTheLastUnanswered(t *testing.T) {
	q := pendingQuestion([]byte(strings.Join([]string{`{"type":"user","uuid":"u0","message":{"content":"hi"}}`, askLine}, "\n")))
	require.NotNil(t, q)
	assert.Equal(t, "toolu_1", q.ID)
	require.Len(t, q.Items, 2)
	assert.Equal(t, "Which screen first?", q.Items[0].Question)
	assert.Equal(t, "Screen", q.Items[0].Header)
	assert.Equal(t, "Agents (Recommended)", q.Items[0].Options[0].Label)
	assert.Equal(t, "who waits on you", q.Items[0].Options[0].Description)
	assert.True(t, q.Items[1].MultiSelect)
}

func TestPendingQuestionAnsweredIsGone(t *testing.T) {
	assert.Nil(t, pendingQuestion([]byte(askLine+"\n"+answerLine)))
	assert.Nil(t, pendingQuestion([]byte(`{"type":"user","message":{"content":"no question"}}`)))
}

func TestPendingQuestionInASubagentIsNotTheRoles(t *testing.T) {
	side := strings.Replace(askLine, `"type":"assistant",`, `"type":"assistant","isSidechain":true,`, 1)
	assert.Nil(t, pendingQuestion([]byte(side)))
}
