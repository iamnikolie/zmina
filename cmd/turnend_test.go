package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Stop hook returns at once and says nothing: it only starts its detached copy for the session it ran for.
func TestTurnEndHookIsSilent(t *testing.T) {
	crewFile := testCrew(t, "")

	for _, tc := range []struct {
		name, stdin string
		args        []string
		spawned     []string
	}{
		{"a turn ended", `{"session_id":"s-1","hook_event_name":"Stop"}`, []string{"--role", "dev"},
			[]string{"--crew", crewFile, "_turn-end", "--role", "dev", "--session", "s-1", "--wait"}},
		{"no session id", `{"hook_event_name":"Stop"}`, []string{"--role", "dev"}, nil},
		{"not json", "not json", []string{"--role", "dev"}, nil},
		{"no role", `{"session_id":"s-1"}`, nil, nil},
		{"an unknown flag", `{"session_id":"s-1"}`, []string{"--nope"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer

			var spawned []string

			a := &app{out: &out, err: &errb, spawn: func(args []string) error { spawned = args; return nil }}
			root := newRoot(a)
			root.SetIn(strings.NewReader(tc.stdin))
			root.SetArgs(append([]string{"--crew", crewFile, "_turn-end"}, tc.args...))
			err := root.Execute()

			if tc.name != "an unknown flag" {
				require.NoError(t, err)
			}

			assert.Equal(t, tc.spawned, spawned)
			assert.Empty(t, out.String(), "nothing on stdout")
			assert.Empty(t, errb.String(), "nothing on stderr")
		})
	}
}

// The detached copy gives up silently when the role's session is not the one the hook ran for.
func TestTurnEndWaitForAnotherSessionPostsNothing(t *testing.T) {
	crewFile := testCrew(t, "")

	out, errOut, err := zm(t, crewFile, "_turn-end", "--role", "dev", "--session", "s-1", "--wait")
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Empty(t, errOut)
}
