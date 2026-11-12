package shift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
)

// A role whose session file in Claude Code's registry does not read is up with an unknown status; the view says why,
// with the file and what to do, so that ls need not leave the reader to find out.
func TestViewSaysWhyAnUpRolesStatusIsUnknown(t *testing.T) {
	good := `{"pid": 1, "sessionId": "sid-dispatcher", "name": "zm-dispatcher", "status": "idle", "statusUpdatedAt": 1}`

	for name, tc := range map[string]struct {
		file   string
		status string
		why    []string
	}{
		"empty":     {"", "unknown", []string{"is empty", "restart the role"}},
		"blanks":    {" \n", "unknown", []string{"is empty"}},
		"cut short": {`{"pid": 1, "sessionId": "sid-disp`, "unknown", []string{"does not parse", "restart the role"}},
		"fine":      {good, "idle", nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("dispatcher"))

			ns := t.TempDir()
			f.s.Reg = &registry.Registry{Namespace: ns}

			dir := filepath.Join(ns, "sessions")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(f.tm.up["dispatcher"])+".json"), []byte(tc.file), 0o600))

			var got *View

			for _, v := range f.s.Views() {
				if v.Role == "dispatcher" {
					got = &v
				}
			}

			require.NotNil(t, got)
			assert.Equal(t, PresenceUp, got.State)
			assert.Equal(t, tc.status, got.Status)

			if tc.why == nil {
				assert.Empty(t, got.StatusError)

				return
			}

			for _, w := range tc.why {
				assert.Contains(t, got.StatusError, w)
			}

			assert.Contains(t, got.StatusError, strconv.Itoa(f.tm.up["dispatcher"])+".json", "the file it is about")
		})
	}
}

// A role whose session waits says on what, from Claude Code's session file: a permission prompt or a question, in one
// word. Only while it waits, and only for words this zmina knows; nothing is kept in the state for it.
func TestViewSaysWhatAWaitingRoleWaitsOn(t *testing.T) {
	for name, tc := range map[string]struct {
		status, waitingFor, want string
	}{
		"a permission prompt":   {"waiting", "permission prompt", WaitPermission},
		"a question":            {"waiting", "input needed", WaitQuestion},
		"waiting, file silent":  {"waiting", "", ""},
		"a word not known":      {"waiting", "something new", ""},
		"left over after it":    {"idle", "input needed", ""},
		"busy, nothing waited":  {"busy", "", ""},
		"busy with a stale one": {"busy", "permission prompt", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("dispatcher"))
			f.rg.byPID[f.tm.up["dispatcher"]] = &registry.Session{Status: tc.status, WaitingFor: tc.waitingFor,
				StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-dispatcher"}

			var got *View

			for _, v := range f.s.Views() {
				if v.Role == "dispatcher" {
					got = &v
				}
			}

			require.NotNil(t, got)
			assert.Equal(t, tc.status, got.Status)
			assert.Equal(t, tc.want, got.WaitingFor)

			raw, err := json.Marshal(got)
			require.NoError(t, err)

			if tc.want == "" {
				assert.NotContains(t, string(raw), "waiting_for", "omitted when empty")
			} else {
				assert.Contains(t, string(raw), `"waiting_for":"`+tc.want+`"`)
			}
		})
	}
}
