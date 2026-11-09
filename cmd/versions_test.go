package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// doctor names what another zmina started: a cockpit and a Codex bridge running its code warn, Claude roles whose
// hooks only keep its command lines are said without a warning, roles down are not named.
func TestDoctorVersions(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n    boot: hi\n  rev:\n    runtime: codex\n    boot: hi\n")
	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	keep := version
	version = "v1.1.0"
	t.Cleanup(func() { version = keep })

	run := func(st *shift.State) []check {
		var out []check
		checkVersions(c, st, func(name, level, format string, args ...any) {
			require.Equal(t, "versions", name)
			out = append(out, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
		})

		return out
	}

	st := shift.NewState()
	st.Role("dispatcher").Up, st.Role("dispatcher").Version = true, "v1.1.0"
	assert.Equal(t, []check{{"versions", "ok", "this zmina is v1.1.0"}}, run(st), "all current: one line")

	st.Role("dev").Up, st.Role("dev").Version = true, "v1.0.0"
	st.Role("rev").Up, st.Role("rev").Version = true, "v1.0.0"
	st.Role("dev#2").Up = true
	st.Role("gone").Version = "v0.9.0" // down: not named

	got := run(st)
	require.Len(t, got, 4)
	assert.Equal(t, "warn", got[1].Level)
	assert.Contains(t, got[1].Detail, "rev (v1.0.0)")
	assert.Equal(t, "ok", got[2].Level)
	assert.Contains(t, got[2].Detail, "dev (v1.0.0)")
	assert.NotContains(t, got[2].Detail, "rev")
	assert.Contains(t, got[3].Detail, "dev#2")
	for _, ck := range got {
		assert.NotContains(t, ck.Detail, "gone")
	}

	// A cockpit of an earlier zmina still serving.
	raw, err := json.Marshal(uiRunning{URL: "http://127.0.0.1:7517/", PID: os.Getpid(), Handoff: "h", Version: "v1.0.0"})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(uiStatePath()), 0o700))
	require.NoError(t, os.WriteFile(uiStatePath(), raw, 0o600))

	got = run(nil)
	require.Len(t, got, 2)
	assert.Equal(t, check{"versions", "warn", "zmina ui (pid " + strconv.Itoa(os.Getpid()) + ") runs v1.0.0, this zmina is " +
		"v1.1.0: stop it and run zmina ui again to serve this one"}, got[1])
}
