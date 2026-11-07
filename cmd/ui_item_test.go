package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getItems(t *testing.T, base, node, crew, ids string) (map[string]itemCard, int) {
	t.Helper()

	resp, err := http.Get(base + "/api/items?" + url.Values{"node": {node}, "crew": {crew}, "ids": {ids}}.Encode())
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}

	var out struct {
		Items map[string]itemCard `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	return out.Items, resp.StatusCode
}

// A task parked and taken again is one piece of work: every id of it opens the same chain, the root's title first,
// the head's state last, and what the owner can do about it.
func TestItemsResolveAChain(t *testing.T) {
	uiHome(t)
	crew := crewPathOf(t, "default")

	for _, args := range [][]string{
		{"task", "new", "dev", "--title", "Login page — rework", "rework the login page"},
		{"task", "park", "T-1", "--note", "left: the form"},
		{"later", "take", "L-1", "dev"},
		{"task", "set", "T-2", "waiting", "--note", "built · left: merge on your word"},
		{"todo", "add", "Merge the login page?", "--title", "Merge login", "--rec", "merge it", "--reason", "green", "--task", "T-2"},
		{"task", "new", "dev", "--title", "Docs", "write the docs"},
		{"task", "done", "T-3"},
		{"later", "add", "an idea for the header"},
		{"todo", "add", "Sign in and publish the board", "--title", "Publish"},
	} {
		_, stderr, err := zm(t, crew, args...)
		require.NoError(t, err, "%v: %s", args, stderr)
	}

	srv, _ := uiServer(t, true, "")
	items, code := getItems(t, srv.URL, "", "default", "T-1,l-1,T-2,A-1,T-3,L-2,A-2,T-8")
	require.Equal(t, http.StatusOK, code)

	ids := func(c itemCard) string {
		var s []string
		for _, st := range c.Chain {
			s = append(s, st.ID)
		}

		return strings.Join(s, " ")
	}

	t1 := items["T-1"]
	require.True(t, t1.Found)
	assert.Equal(t, "T-1 L-1 T-2", ids(t1))
	assert.Equal(t, 0, t1.At)
	assert.Equal(t, "Login page — rework", t1.Chain[0].Title, "the root's title")
	assert.Equal(t, "need", t1.State, "the head waits on the owner")
	require.NotNil(t, t1.Decision)
	assert.Equal(t, itemDecision{ID: "A-1", Action: "yes", Word: "merge it"}, *t1.Decision)
	assert.Equal(t, "dev", t1.Talk)

	l1 := items["L-1"]
	assert.Equal(t, "later", l1.Kind)
	assert.Equal(t, 1, l1.At)
	assert.Equal(t, "T-1 L-1 T-2", ids(l1))

	assert.Equal(t, "ask", items["A-1"].Kind)
	assert.Equal(t, "need", items["A-1"].State)
	assert.Equal(t, "T-2", items["A-1"].Ask.Task)

	assert.Equal(t, "done", items["T-3"].State)
	assert.Nil(t, items["T-3"].Decision)

	l2 := items["L-2"]
	assert.Equal(t, "parked", l2.State)
	assert.Equal(t, "L-2", l2.Back)
	assert.Equal(t, "dispatcher", l2.Talk, "parked from no task: the dispatcher's")

	a2 := items["A-2"]
	require.NotNil(t, a2.Decision)
	assert.Equal(t, itemDecision{ID: "A-2", Action: "done", Word: "Mark checked"}, *a2.Decision)

	assert.False(t, items["T-8"].Found)

	_, code = getItems(t, srv.URL, "", "default", "T-1,rm -rf")
	assert.Equal(t, http.StatusBadGateway, code)
	_, code = getItems(t, srv.URL, "", "nope", "T-1")
	assert.Equal(t, http.StatusBadGateway, code)
}

// An open task's state: queued until its role takes it, then in work.
func TestItemsQueuedThenInWork(t *testing.T) {
	uiHome(t)
	crew := crewPathOf(t, "default")
	_, _, err := zm(t, crew, "task", "new", "dev", "--title", "Fix", "fix it")
	require.NoError(t, err)

	a := &app{out: io.Discard, err: io.Discard}
	data, err := a.localItems("default", "T-1")
	require.NoError(t, err)

	var out struct {
		Items map[string]itemCard `json:"items"`
	}
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Contains(t, []string{"with", "queued"}, out.Items["T-1"].State, "its role is not working on it")
	assert.Equal(t, "dev", out.Items["T-1"].Who)
}

// A peer's ids resolve on its own node, over the owner-signed mesh.
func TestItemsOfAPeer(t *testing.T) {
	homeA := uiHome(t)
	from := crewPathOf(t, "default")
	_, _, err := zm(t, from, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)
	far := farBox(t, homeA, from)
	_, _, err = zm(t, far, "todo", "add", "Ship it?", "--rec", "yes")
	require.NoError(t, err)

	srv, _ := uiServer(t, true, "")
	items, code := getItems(t, srv.URL, "box", "ops", "A-1")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "need", items["A-1"].State)

}
