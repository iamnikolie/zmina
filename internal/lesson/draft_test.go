package lesson

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDraftTitle(t *testing.T) {
	assert.Equal(t, "Draft: Lessons 2026-11-09", DraftTitle("Lessons 2026-11-09"))
	assert.Equal(t, "Draft: x", DraftTitle("Draft: x"))
}

// stub stands in for glab and gl: which are on PATH, and what each answers.
func stub(t *testing.T, on map[string]bool, answer func(name string, args []string) (string, error)) *[]string {
	t.Helper()

	var calls []string

	was, wasLook := Runner, LookPath
	t.Cleanup(func() { Runner, LookPath = was, wasLook })

	LookPath = func(name string) (string, error) {
		if on[name] {
			return "/bin/" + name, nil
		}

		return "", errors.New("not found")
	}
	Runner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		out, err := answer(name, args)

		return []byte(out), err
	}

	return &calls
}

func TestReadDraft(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GL_CONFIG", "")

	for name, host := range map[string]string{"work": "gitlab.int.acme.example", "own": "gitlab.com"} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".gl", name), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".gl", name, "config.yaml"), []byte("host: "+host+"\ntoken: x\n"), 0o600))
	}

	const mr = "https://gitlab.com/acme/agentic-os/-/merge_requests/12"

	calls := stub(t, map[string]bool{"glab": true}, func(string, []string) (string, error) { return `{"draft":true}`, nil })
	d, via := ReadDraft(mr)
	assert.Equal(t, "yes", d)
	assert.Equal(t, "glab", via)
	assert.Equal(t, []string{"glab api --hostname gitlab.com projects/acme%2Fagentic-os/merge_requests/12"}, *calls)

	// glab fails; gl answers with the profile for gitlab.com, never the other host's.
	calls = stub(t, map[string]bool{"glab": true, "gl": true}, func(name string, _ []string) (string, error) {
		if name == "glab" {
			return "", errors.New("401")
		}

		return `{"draft":false,"work_in_progress":false}`, nil
	})
	d, via = ReadDraft(mr)
	assert.Equal(t, "no", d)
	assert.Equal(t, "gl --config own", via)
	assert.Len(t, *calls, 2)
	assert.Contains(t, (*calls)[1], "gl --config own --format json api GET /projects/acme%2Fagentic-os/merge_requests/12")

	stub(t, nil, func(string, []string) (string, error) { return `{"draft":true}`, nil })
	d, via = ReadDraft(mr)
	assert.Empty(t, d, "no client on the node: not read")
	assert.Empty(t, via)

	d, _ = ReadDraft("lessons/2026-11-09")
	assert.Empty(t, d, "no MR url")
}
