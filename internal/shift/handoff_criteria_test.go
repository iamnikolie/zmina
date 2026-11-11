package shift

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
)

// A task without criteria gets the packet instructions it always had, word for word; one with criteria gets the
// same text with what done means between the shape's request and the report rule, and carries them in its task.
func TestTaskPacketCarriesCriteria(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	path := shellWord(f.s.Crew.Path)

	r, err := f.s.Role("designer")
	require.NoError(t, err)

	request := f.s.Crew.HandoffFor(r).Request("checkpoint or result")
	require.Contains(t, request, "- summary: ")

	head := fmt.Sprintf("Read repository instructions. Run zmina --crew %s task show T-1 and confirm this task is still assigned to "+
		"designer; use the latest note before working.\n", path)
	tail := fmt.Sprintf("When done, save it with zmina --crew %s task result T-1 --file <json>: that tells the dispatcher, "+
		"with the gist and the command that shows it. Send the dispatcher no message of your own about the result: "+
		"it would hear it twice.\n", path)

	plain, err := f.s.TaskPacket("designer", task.Task{ID: "T-1", Role: "designer", Asked: "x"})
	require.NoError(t, err)
	assert.Equal(t, head+request+tail, plain.Instructions, "no criteria: the packet text is unchanged")
	assert.Empty(t, plain.Task.Criteria)

	with, err := f.s.TaskPacket("designer", task.Task{ID: "T-1", Role: "designer", Asked: "x",
		Criteria: []string{"tests pass", "the flag is in skill.md"}})
	require.NoError(t, err)
	assert.Equal(t, head+request+
		"Done means every criterion below holds; in your result say for each whether it holds and how you checked.\n"+
		"1. tests pass\n"+
		"2. the flag is in skill.md\n"+tail, with.Instructions)
	assert.Equal(t, []string{"tests pass", "the flag is in skill.md"}, with.Task.Criteria)
}
