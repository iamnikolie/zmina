package ui

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type labelledRow struct {
	Words  string   `json:"words"`
	Labels []string `json:"labels"`
}

// TestSameQuestionFromTwoNodesReadsApart renders static/app.js's question rows under Node with a fake DOM
// (testdata/app-labels.js): in All the same question from two nodes carries each node's label, on the cockpit and on
// the lean page; a view filtered to that node or crew drops what the switch already says.
func TestSameQuestionFromTwoNodesReadsApart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's rows are not rendered")
	}

	raw, err := exec.Command(node, "testdata/app-labels.js", "static/app.js").Output()
	require.NoError(t, err)

	var views map[string]map[string][]labelledRow
	require.NoError(t, json.Unmarshal(raw, &views))

	for _, layout := range []string{"cockpit", "lean"} {
		all := views["all"][layout]
		require.Len(t, all, 2, layout)
		assert.Equal(t, []string{"node-a"}, all[0].Labels, layout)
		assert.Equal(t, []string{"node-b"}, all[1].Labels, layout)
		assert.NotEqual(t, all[0].Words, all[1].Words, layout+": the two rows read apart")

		for _, f := range []string{"node:node-b", "node-b/default"} {
			assert.Empty(t, views[f][layout][1].Labels, layout+" "+f+": the switch already names it")
		}
	}
}

// TestPhoneTabs runs static/app.js's phone tabs (D-09) under Node: Talk opens the scoped crew's dispatcher once the
// scope changed, Alarms with no alarm keeps its heading and «Set», and Back to the entry the page opened on without a
// hash returns to the tab it opened on.
func TestPhoneTabs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's tabs are not run")
	}

	raw, err := exec.Command(node, "testdata/app-labels.js", "static/app.js", "phone").Output()
	require.NoError(t, err)

	var res struct {
		SameScope  struct{ Crew, Role string } `json:"sameScope"`
		OtherScope struct{ Crew, Role string } `json:"otherScope"`
		Alarms     string                      `json:"alarms"`
		Back       struct{ Tab, Body string }  `json:"back"`
		KbPhone    string                      `json:"keyboardPhone"`
		KbDesktop  string                      `json:"keyboardDesktop"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))

	assert.Equal(t, "default/dev", res.SameScope.Crew+"/"+res.SameScope.Role, "the scope it was opened under keeps the conversation")
	assert.Equal(t, "node-b/ops/dispatcher", res.OtherScope.Crew+"/"+res.OtherScope.Role, "another scope talks to its own dispatcher")
	assert.Contains(t, res.Alarms, "noal")
	assert.NotContains(t, res.Alarms, "empty", "an empty Alarms is not hidden: its Set is the way to the first one")
	assert.Equal(t, "move", res.Back.Tab)
	assert.Equal(t, "move", res.Back.Body)
	assert.Contains(t, res.KbPhone, "keyboard", "a field in the conversation hides the bar on the phone")
	assert.NotContains(t, res.KbDesktop, "keyboard", "never on the desktop")
	assert.NotContains(t, res.KbDesktop, "kb", "nor as the board's own class")
}
