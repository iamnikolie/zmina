package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueDropExplainsRetainedInbox(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "send", "dev", "--when-idle", "Review the change")
	require.NoError(t, err)
	out, _, err := zm(t, file, "queue", "drop", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "0 lines dropped")
	assert.Contains(t, out, "1 inbox lines remain")
	assert.Contains(t, out, "zmina --crew "+quoteCommandArg(file)+" queue drop 'dev' --all")
	out, _, err = zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "Review the change")
	out, _, err = zm(t, file, "queue", "drop", "dev", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "1 lines dropped")
	assert.NotContains(t, out, "remain")
	out, _, err = zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "Review the change")
}

// Run the exact suggested shell command through the real command tree in an isolated child.
func TestQueueDropHintProcess(t *testing.T) {
	if os.Getenv("ZMINA_HINT_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			root := NewRoot(os.Stdout, os.Stderr)
			root.SetArgs(os.Args[i+1:])
			if root.Execute() != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	t.Fatal("missing child command")
}

func TestQueueDropHintTargetsSelectedCrew(t *testing.T) {
	for _, selector := range []string{"--crew", "--config"} {
		for _, role := range []string{"dev", "dispatcher"} {
			t.Run(selector+"/"+role, func(t *testing.T) {
				home := uiHome(t)
				defaultFile := filepath.Join(home, ".zmina", "crew.yaml")
				config := "ops space's $(literal)"
				dir := filepath.Join(home, ".zmina", config)
				require.NoError(t, os.Rename(filepath.Join(home, ".zmina", "ops"), dir))
				selected := filepath.Join(dir, "crew.yaml")
				if selector == "--config" {
					t.Setenv("ZMINA_CREW", defaultFile)
				}
				for file, text := range map[string]string{defaultFile: "unrelated work", selected: "selected work"} {
					_, _, err := zm(t, file, "send", role, "--when-idle", text)
					require.NoError(t, err)
				}
				var out bytes.Buffer
				root := NewRoot(&out, &out)
				value := selected
				if selector == "--config" {
					value = config
				}
				root.SetArgs([]string{selector, value, "queue", "drop", role})
				require.NoError(t, root.Execute())
				_, hint, found := strings.Cut(out.String(), "; use ")
				require.True(t, found)
				hint = strings.TrimSuffix(strings.TrimSpace(hint), " to drop them")
				t.Setenv("ZMINA_HINT_PROCESS", "1")
				t.Setenv("ZMINA_TEST_EXE", os.Args[0])
				proc := exec.Command("sh", "-c", `zmina() { "$ZMINA_TEST_EXE" -test.run='^TestQueueDropHintProcess$' -- "$@"; }
`+hint)
				raw, err := proc.CombinedOutput()
				require.NoError(t, err, string(raw))
				selectedQueue, _, err := zm(t, selected, "queue", "ls")
				require.NoError(t, err)
				assert.NotContains(t, selectedQueue, "selected work")
				defaultQueue, _, err := zm(t, defaultFile, "queue", "ls")
				require.NoError(t, err)
				assert.Contains(t, defaultQueue, "unrelated work")
			})
		}
	}
}

// queue ls leads each line with its words and puts the ids it carries after them; a line with no id reads as it did.
func TestQueueLineWordsBeforeIds(t *testing.T) {
	for name, tc := range map[string]struct{ in, short, full string }{
		"plain":         {"Review the change", "Review the change", "Review the change"},
		"head task id":  {"T-12 third commit 64c97cb (D-12 a-e: readout)", "third commit 64c97cb (D-12 a-e: readout) · T-12", ""},
		"signal":        {"S-12 mrs: two merge requests wait", "mrs: two merge requests wait · S-12", ""},
		"tail ack":      {"the review came back (zmina S-610)", "the review came back · S-610", ""},
		"both":          {"T-12 third commit (zmina S-610)", "third commit · T-12 · S-610", ""},
		"two acks":      {"a note and another (zmina S-1 S-2)", "a note and another · S-1 · S-2", ""},
		"still pending": {"the review (zmina S-4, still pending since Mon 9 Nov 12:00)", "the review · S-4", ""},
		"lesson":        {"LS-7 rule: wait for the build", "rule: wait for the build · LS-7", ""},
		"id only":       {"T-12", "T-12", ""},
		"id and tail":   {"S-3 (zmina S-3)", "S-3 (zmina S-3)", ""},
		"id mid-line":   {"look at T-5 first", "look at T-5 first", ""},
		"not an id":     {"D-12 is a design id", "D-12 is a design id", ""},
		"tail mid-line": {"(zmina S-9) is first", "(zmina S-9) is first", ""},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.short, queueLine(tc.in, false))

			if tc.full == "" {
				tc.full = tc.short
			}

			assert.Equal(t, tc.full, queueLine(tc.in, true))
		})
	}

	long := "T-9 " + strings.Repeat("word ", 60) + "(zmina S-8)"
	got := queueLine(long, false)
	assert.True(t, strings.HasSuffix(got, "… · T-9 · S-8"), "the ids survive the clip: %s", got)
	assert.Less(t, len([]rune(got)), 180)
	assert.Contains(t, queueLine("a\nb  c T-1", false), "a b c T-1")
}

func TestQueueLsPutsWordsBeforeIdsAndJSONStaysRaw(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "send", "dev", "--when-idle", "T-5 fix the login")
	require.NoError(t, err)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "fix the login · T-5")
	assert.NotContains(t, out, "T-5 fix the login")

	out, _, err = zm(t, file, "queue", "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "T-5 fix the login", "JSON is the stored line")
}
