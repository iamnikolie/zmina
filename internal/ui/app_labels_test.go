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
