package ui

import (
	"encoding/json"
	"os"
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
	node := requireNode(t, "node is not installed: the page's rows are not rendered")

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
	node := requireNode(t, "node is not installed: the page's tabs are not run")

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
		Readout []string          `json:"readout"`
		Anchors map[string]string `json:"anchors"`
		BothRed string            `json:"bothRed"`
		Merged  struct {
			Head string   `json:"head"`
			Rows []string `json:"rows"`
		} `json:"merged"`
		Hand  []string `json:"hand"`
		Head  string   `json:"head"`
		Sheet []string `json:"sheet"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))

	assert.Equal(t, "default/dev", res.SameScope.Crew+"/"+res.SameScope.Role, "the scope it was opened under keeps the conversation")
	assert.Equal(t, "node-b/ops/dispatcher", res.OtherScope.Crew+"/"+res.OtherScope.Role, "another scope talks to its own dispatcher")
	assert.Equal(t, "agents", res.Back.Tab)
	assert.Equal(t, "agents", res.Back.Body)
	assert.Contains(t, res.KbPhone, "keyboard", "a field in the conversation hides the bar on the phone")
	assert.NotContains(t, res.KbDesktop, "keyboard", "never on the desktop")
	assert.NotContains(t, res.KbDesktop, "kb", "nor as the board's own class")

	// D-12 a1, a2 rows in D-19's blocks, one per crew: the readout (a); a crew with a role down first, ties by name; its
	// head says its machines and how its roles stand; inside, down then waiting (a question on screen, then the oldest
	// wait), then working, then the idle ones folded into one line (opened here); a row names no machine while the crew
	// lives on one. A down role that holds nothing, a spare crew and the roles that wait never show twice.
	assert.Equal(t, []string{"5 waiting on you", "2 working", "1 idle"}, res.Readout)
	// One crew has all three here: waiting goes to its block, working to its head, idle to its idle line.
	assert.Equal(t, map[string]string{"ag-wait": "crew:default ags cg", "ag-work": " cg-h", "ag-idle": " cg-idle"}, res.Anchors)
	require.Len(t, res.Agents, 3)
	group := func(i int) []string {
		var out []string
		for _, r := range res.Agents[i].Rows {
			out = append(out, r.Cls+" | "+r.Text)
		}

		return out
	}
	assert.Equal(t, "defaultnode-a1 down · 3 waiting · 2 working · 1 idle", res.Agents[0].Text)
	assert.Equal(t, []string{
		"ag x | devopsDown · holds T-2›",
		"ag n | designerAsks: Which palette?›",
		"ag n | 2reviewerCheck the copy4 d›",
		"ag n | dispatcherShip it?1 h›",
		"ag busy one | tester3 min›",
		"ag busy two | devBuild the login page11 min›",
		"cg-idle | 1 idle · scribe›",
		"ag idle one | scribe40 min›",
	}, group(0))
	assert.Equal(t, "opsnode-b1 down", res.Agents[1].Text)
	assert.Equal(t, []string{"ag x | dispatcherTicker stopped · the crew does not wake›"}, group(1))
	assert.Equal(t, "Yours by hand1 · oldest 8 d ›", res.Agents[2].Text)
	// g1, g2: the same crew on another machine joins its block, and then each row names its machine.
	assert.Equal(t, "defaultnode-a · node-b1 down · 3 waiting · 3 working · 2 idle", res.Merged.Head)
	assert.Equal(t, []string{"devopsnode-aDown · holds T-2›", "designernode-aAsks: Which palette?›", "2reviewernode-aCheck the copy4 d›",
		"dispatchernode-aShip it?1 h›", "buildernode-b2 min›", "testernode-a3 min›", "devnode-aBuild the login page11 min›",
		"2 idle · dispatcher node-b, scribe node-a›", "dispatchernode-b9 min›", "scribenode-a40 min›"}, res.Merged.Rows)
	assert.Equal(t, "Ticker stopped · the crew does not wake · Down · holds T-9", res.BothRed, "a down dispatcher over a stopped ticker says both")

	// l1: Later's by-hand list, and its item as a sheet with one primary and Talk to the role that raised it.
	assert.Equal(t, "Yours by hand", res.Head)
	assert.Equal(t, []string{"Sign in and publishdefault · 8 d›"}, res.Hand)
	assert.Equal(t, []string{"", "Yours by hand · raised by dispatcher · default · 8 d", "Sign in and publish",
		"Sign in and publish the test board", "Done", "Talk to dispatcher"}, res.Sheet)
}

// TestDecidedReadsLikeACheckOnThePage runs static/app.js's question row under Node: a decided item (a role took the
// decision itself) has Done and an answer in words and no No, as a check has, and its opened row says why and what was
// not done; a decision keeps its No.
func TestDecidedReadsLikeACheckOnThePage(t *testing.T) {
	node := requireNode(t, "node is not installed: the page's rows are not rendered")

	raw, err := exec.Command(node, "testdata/app-labels.js", "static/app.js", "decided").Output()
	require.NoError(t, err)

	var res struct {
		Rows map[string]struct {
			Buttons []string `json:"buttons"`
			Panel   []string `json:"panel"`
		} `json:"rows"`
		Acted  []string `json:"acted"`
		Checks []bool   `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))

	assert.Equal(t, []string{"decided done: Reviewed", "check done: Sent to the dispatcher"}, res.Acted,
		"a decided item read is only reviewed; a check is sent")
	assert.Equal(t, []string{"Answer…", "Done", "Later"}, res.Rows["decided"].Buttons)
	assert.Equal(t, res.Rows["check"].Buttons, res.Rows["decided"].Buttons, "as a check")
	assert.Equal(t, []string{"Answer…", "Later", "No"}, res.Rows["decision"].Buttons)
	assert.Equal(t, []string{"links point at it", "Instead of: rename the flag"}, res.Rows["decided"].Panel)
	assert.Equal(t, []bool{true, false}, res.Checks)
}

// TestDecidedItemsAreCountedNowhere draws the whole page of static/app.js under Node: with only decided items the title,
// the favicon and the line of counts say nothing waits, and beside a real ask they are not in its count; they are a group
// of their own, newest first, on the lean page and the cockpit; on the phone they are not among the waiting and not "yours
// by hand", but a list of their own.
func TestDecidedItemsAreCountedNowhere(t *testing.T) {
	node := requireNode(t, "node is not installed: the page is not drawn")

	raw, err := exec.Command(node, "testdata/app-labels.js", "static/app.js", "counts").Output()
	require.NoError(t, err)

	type view struct {
		Title         string   `json:"title"`
		Amber         bool     `json:"amber"`
		Say           string   `json:"say"`
		AskHead       string   `json:"askHead"`
		DecidedHidden bool     `json:"decidedHidden"`
		DecidedHead   string   `json:"decidedHead"`
		DecidedRows   []string `json:"decidedRows"`
		MoveAsks      []string `json:"moveAsks"`
		MoveDecHead   string   `json:"moveDecHead"`
		MoveDec       []string `json:"moveDec"`
		Phone         struct {
			Waiting, Hand, Decided []string
		} `json:"phone"`
	}

	var res map[string]view
	require.NoError(t, json.Unmarshal(raw, &res))

	newestFirst := []string{"default/A-3", "default/A-2"}

	only := res["only"]
	assert.Equal(t, "zmina", only.Title, "no answer to give")
	assert.False(t, only.Amber, "no mark on the favicon")
	assert.Equal(t, "You hold 0 · answer 0 · in progress 0 · parked 0", only.Say)
	assert.Equal(t, "Answer 0", only.AskHead)
	assert.False(t, only.DecidedHidden)
	assert.Equal(t, "Decided, for your review 2", only.DecidedHead)
	assert.Equal(t, newestFirst, only.DecidedRows)
	assert.Empty(t, only.MoveAsks)
	assert.Equal(t, newestFirst, only.MoveDec, "the cockpit has them in a group of their own")
	assert.Equal(t, "Decided, for your review", only.MoveDecHead)
	assert.Empty(t, only.Phone.Waiting)
	assert.Empty(t, only.Phone.Hand)
	assert.Equal(t, []string{"A-3", "A-2"}, only.Phone.Decided)

	mixed := res["mixed"]
	assert.Equal(t, "zmina · answer 2", mixed.Title, "an ask and a check; the decided ones are not counted")
	assert.True(t, mixed.Amber)
	assert.Equal(t, "You hold 0 · answer 2 · in progress 0 · parked 0", mixed.Say)
	assert.Equal(t, newestFirst, mixed.DecidedRows)
	assert.Equal(t, []string{"default/A-9", "default/A-8"}, mixed.MoveAsks, "the decided ones are not among the asks")
	assert.Equal(t, []string{"A-9"}, mixed.Phone.Waiting)
	assert.Equal(t, []string{"A-8"}, mixed.Phone.Hand, "only a check is by hand")
	assert.Equal(t, []string{"A-3", "A-2"}, mixed.Phone.Decided)
}

// requireNode is the node binary; without one the test skips, except in CI, where a skip would read as a pass.
func requireNode(t *testing.T, why string) string {
	t.Helper()

	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}

	if os.Getenv("CI") != "" {
		t.Fatal(why + " (CI must run it)")
	}

	t.Skip(why)

	return ""
}
