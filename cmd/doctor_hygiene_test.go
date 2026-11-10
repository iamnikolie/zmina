package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuth is a claude that passes doctor's flag check and answers auth status with the given JSON.
func fakeAuth(t *testing.T, dir, status string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = auth ]; then echo '%s'; exit 0; fi
echo '--name --resume --remote-control --model'
`, status)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700))
}

func TestDoctorLogin(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)

	fakeAuth(t, dir, `{"loggedIn": false}`)
	out, _, err := zm(t, crewFile, "doctor")
	assert.Contains(t, out, "fail  login")
	assert.Contains(t, out, "is not logged in")
	assert.Error(t, err)

	soon := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	fakeAuth(t, dir, `{"loggedIn": true, "loginExpiresAt": "`+soon+`"}`)
	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  login")
	assert.Contains(t, out, "the login expires")

	later := time.Now().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fakeAuth(t, dir, `{"loggedIn": true, "loginExpiresAt": "`+later+`"}`)
	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "ok    login")

	fakeAuth(t, dir, `not json`)
	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  login")
	assert.Contains(t, out, "could not tell")
}

func TestDoctorHygiene(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)
	fakeAuth(t, dir, `{"loggedIn": true}`)

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err)
	for _, name := range []string{"asks", "later", "worktrees"} {
		assert.NotContains(t, out, "warn  "+name, "a fresh crew has nothing piled up")
	}

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Now()
	old := now.Add(-20 * 24 * time.Hour)

	_, err = askStore(c).Add(asks.Ask{Kind: asks.Check, Text: "old check"}, old)
	require.NoError(t, err)
	_, err = askStore(c).Add(asks.Ask{Kind: asks.Check, Text: "new check"}, now)
	require.NoError(t, err)
	_, err = laterStore(c).Add(later.Item{Text: "old idea"}, old)
	require.NoError(t, err)

	wt := filepath.Join(dir, "wt")
	require.NoError(t, os.MkdirAll(wt, 0o700))

	done, err := taskStore(c).Add(task.Task{Role: "dispatcher", Asked: "x", Worktree: wt}, old)
	require.NoError(t, err)
	done.State = task.Done
	require.NoError(t, taskStore(c).Put(done))
	_, err = taskStore(c).Add(task.Task{Role: "dispatcher", Asked: "y", Worktree: wt}, old)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(crewFile+".bak", []byte("x"), 0o600))

	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  asks      1 open for the owner over 7 days (A-1)")
	assert.Contains(t, out, "warn  later     1 parked over 14 days (L-1)")
	assert.Contains(t, out, "warn  worktrees 1 closed tasks keep their worktree", "an open task's worktree is not counted")
	assert.NotContains(t, out, "copies of the crew file", "seat's own .bak is fine")

	require.NoError(t, os.WriteFile(crewFile+".bak-by-hand", []byte("x"), 0o600))

	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  crew      2 copies of the crew file next to it")
}
