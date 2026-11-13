package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An ask may offer answers to pick from (--option, repeated) and a recommendation (--rec, also --recommend); they are stored on
// the item and shown by todo ls --full and --json.
func TestTodoAddOptionsAndRecommend(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	out, _, err := zm(t, file, "todo", "add", "Which name for the site?", "--recommend", "acme", "--reason", "short",
		"--option", "acme", "--option", "  acme-lab ", "--option", "", "--title", "Site name")
	require.NoError(t, err)
	assert.Equal(t, "A-1\n", out)

	ask, err := askStore(mustCrew(t, file)).Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, "acme", ask.Rec, "--recommend is --rec")
	assert.Equal(t, []string{"acme", "acme-lab"}, ask.Options, "trimmed, none empty")
	assert.Equal(t, "decision", string(ask.Kind))

	// Options alone make a decision too: it is a question with answers, not a thing to do.
	_, _, err = zm(t, file, "todo", "add", "Ship it?", "--option", "yes", "--option", "no")
	require.NoError(t, err)

	ask, _ = askStore(mustCrew(t, file)).Get("A-2")
	assert.Equal(t, "decision", string(ask.Kind))
	assert.Empty(t, ask.Rec)

	// Neither: a check, as before; an explicit kind is kept.
	_, _, err = zm(t, file, "todo", "add", "Sign in")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "add", "Pick one", "--option", "a", "--kind", "check")
	require.NoError(t, err)

	ask, _ = askStore(mustCrew(t, file)).Get("A-3")
	assert.Equal(t, "check", string(ask.Kind))
	ask, _ = askStore(mustCrew(t, file)).Get("A-4")
	assert.Equal(t, "check", string(ask.Kind))

	// ls --full says them; the short line does not.
	out, _, err = zm(t, file, "todo", "ls", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "recommended: acme (short) · options: 1) acme  2) acme-lab")

	out, _, err = zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "options:")

	var list struct {
		Items []todoItem `json:"items"`
	}

	out, _, err = zm(t, file, "todo", "ls", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &list))

	for _, it := range list.Items {
		if it.ID == "A-1" {
			assert.Equal(t, []string{"acme", "acme-lab"}, it.Options)
		}
	}

	// An answer by option is an answer in the owner's words: the option's text.
	_, _, err = zm(t, file, "todo", "answer", "A-1", "acme-lab")
	require.NoError(t, err)
	ask, _ = askStore(mustCrew(t, file)).Get("A-1")
	assert.Equal(t, "acme-lab", ask.Answer)

	// No more than a phone can show.
	args := []string{"todo", "add", "Too many?"}
	for i := range 13 {
		args = append(args, "--option", strings.Repeat("o", i+1))
	}

	_, _, err = zm(t, file, args...)
	require.ErrorContains(t, err, "at most 12 options")
}

// todo edit replaces or clears the options; an ask with options stays a decision without a recommendation.
func TestTodoEditOptions(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "todo", "add", "Which name?", "--rec", "acme", "--option", "acme")
	require.NoError(t, err)

	out, _, err := zm(t, file, "todo", "edit", "A-1", "--option", "x", "--option", "y")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · options\n", out)

	ask, _ := askStore(mustCrew(t, file)).Get("A-1")
	assert.Equal(t, []string{"x", "y"}, ask.Options)

	_, _, err = zm(t, file, "todo", "edit", "A-1", "--recommend", "-")
	require.NoError(t, err)
	ask, _ = askStore(mustCrew(t, file)).Get("A-1")
	assert.Empty(t, ask.Rec)
	assert.Equal(t, "decision", string(ask.Kind), "it still offers answers")

	out, _, err = zm(t, file, "todo", "edit", "A-1", "--option", "-")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · options cleared\n", out)

	ask, _ = askStore(mustCrew(t, file)).Get("A-1")
	assert.Empty(t, ask.Options)
	assert.Equal(t, "check", string(ask.Kind), "neither a recommendation nor options: a check")

	_, _, err = zm(t, file, "todo", "edit", "A-1")
	require.ErrorContains(t, err, "nothing to change: give --title, --text, --rec, --option")
}

// An ask file written before options and closed_by were kept reads as it did, can be answered, and is written back with
// neither field when it has nothing to say of them.
func TestOldAskFilesStayReadable(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	dir := filepath.Join(filepath.Dir(file), "asks")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	old := `{"id": "A-1", "kind": "decision", "text": "Rename the flag?", "rec": "keep", "reason": "links", "role": "dev",
  "state": "open", "created": "2026-11-01T10:00:00Z", "updated": "2026-11-01T10:00:00Z"}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "A-1.json"), []byte(old), 0o600))

	out, _, err := zm(t, file, "todo", "ls", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "Rename the flag? · recommended: keep (links)")
	assert.NotContains(t, out, "options:")

	_, _, err = zm(t, file, "todo", "yes", "A-1")
	require.NoError(t, err)

	raw := string(mustRead(t, filepath.Join(dir, "A-1.json")))
	assert.NotContains(t, raw, `"options"`)
	assert.Contains(t, raw, `"closed_by": "owner"`)
}
