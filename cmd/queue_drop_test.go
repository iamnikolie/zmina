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
