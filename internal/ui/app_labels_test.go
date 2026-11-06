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

// TestPhoneTabs runs static/app.js's phone tabs (D-09, D-12) under Node: Talk opens the scoped crew's dispatcher once
// the scope changed, Back to the entry the page opened on without a hash returns to the tab it opened on, Agents draws one row per role in its groups, and a by-hand item opens as a sheet.
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
		Back       struct{ Tab, Body string }  `json:"back"`
		KbPhone    string                      `json:"keyboardPhone"`
		KbDesktop  string                      `json:"keyboardDesktop"`
		Agents     []struct {
			Cls, Text string
			Rows      []struct{ Cls, Text string }
		} `json:"agents"`
		Readout []string `json:"readout"`
		Hand    []string `json:"hand"`
		Head    string   `json:"head"`
		Sheet   []string `json:"sheet"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))

	assert.Equal(t, "default/dev", res.SameScope.Crew+"/"+res.SameScope.Role, "the scope it was opened under keeps the conversation")
	assert.Equal(t, "node-b/ops/dispatcher", res.OtherScope.Crew+"/"+res.OtherScope.Role, "another scope talks to its own dispatcher")
	assert.Equal(t, "agents", res.Back.Tab)
	assert.Equal(t, "agents", res.Back.Body)
	assert.Contains(t, res.KbPhone, "keyboard", "a field in the conversation hides the bar on the phone")
	assert.NotContains(t, res.KbDesktop, "keyboard", "never on the desktop")
	assert.NotContains(t, res.KbDesktop, "kb", "nor as the board's own class")

	// D-12 a1–a3: the readout (a); red first (a role down with a task, a peer crew's stopped ticker), then a question on
	// screen, then the oldest wait; a role with two items is one row with its count; a busy role with its task on a
	// second line, one without on one line and no words (b); the idle one; by hand a line. A down role that holds
	// nothing, a spare crew and the roles that wait never show twice.
	assert.Equal(t, []string{"5 waiting on you", "2 working", "1 idle"}, res.Readout)
	require.Len(t, res.Agents, 4)
	group := func(i int) []string {
		var out []string
		for _, r := range res.Agents[i].Rows {
			out = append(out, r.Cls+" | "+r.Text)
		}

		return out
	}
	assert.Equal(t, "Waiting on you", res.Agents[0].Text)
	assert.Equal(t, []string{
		"ag x | devopsdefaultDown · holds T-2›",
		"ag x | dispatcherops · node-bTicker stopped · the crew does not wake›",
		"ag n | designerdefaultAsks: Which palette?›",
		"ag n | 2reviewerdefaultCheck the copy4 d›",
		"ag n | dispatcherdefaultShip it?1 h›",
	}, group(0))
	assert.Equal(t, "Working", res.Agents[1].Text)
	assert.Equal(t, []string{"ag busy one | testerdefault3 min›", "ag busy two | devdefaultBuild the login page11 min›"}, group(1))
	assert.Equal(t, []string{"ag idle one | scribedefault40 min›"}, group(2))
	assert.Equal(t, "Yours by hand1 · oldest 8 d ›", res.Agents[3].Text)

	// l1: Later's by-hand list, and its item as a sheet with one primary and Talk to the role that raised it.
	assert.Equal(t, "Yours by hand", res.Head)
	assert.Equal(t, []string{"Sign in and publishdefault · 8 d›"}, res.Hand)
	assert.Equal(t, []string{"", "Yours by hand · raised by dispatcher · default · 8 d", "Sign in and publish",
		"Sign in and publish the test board", "Done", "Talk to dispatcher"}, res.Sheet)
}
