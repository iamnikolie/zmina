package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func attachFixture(t *testing.T, name, transport string) (*app, string, string) {
	t.Helper()
	file := testCrew(t, "  dev:\n    runtime: codex\n    codex_transport: "+transport+"\n    model: test-model\n    reasoning_effort: high\n    profile: test-profile\n    sandbox: read-only\n    approval_policy: on-request\n")
	home := filepath.Join(t.TempDir(), "role home")
	bin := filepath.Join(t.TempDir(), "fake codex")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'cwd=%s\\nhome=%s\\nrole=%s\\n' \"$PWD\" \"$CODEX_HOME\" \"$ZMINA_ROLE\"\nprintf 'arg=%s\\n' \"$@\"\n"), 0700))
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	text := strings.Replace(string(raw), "roles:\n", "codex: "+bin+"\nroles:\n", 1)
	text = strings.Replace(text, "    runtime: codex\n", "    runtime: codex\n    codex_home: "+home+"\n", 1)
	require.NoError(t, os.WriteFile(file, []byte(text), 0600))
	a := &app{crew: file, out: io.Discard, err: io.Discard}
	dir := filepath.Join(t.TempDir(), "task tree")
	require.NoError(t, os.MkdirAll(dir, 0700))
	dir, err = filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		r, err := s.Role(name)
		if err != nil {
			return err
		}
		// The role is a harmless fake console; never start a real Codex/daemon in this test.
		if err := s.Tmux.Start(name, dir, nil, []string{s.Crew.Claude}); err != nil {
			return err
		}
		rs := s.State.Role(name)
		rs.LastSession = "stale-thread"
		rs.Dir = dir
		rs.Instance = strings.Contains(name, "#")
		p, err := s.Tmux.Probe(name)
		if err != nil {
			return err
		}
		reg := codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
		return reg.Save(codex.Record{Session: registry.Session{PID: p.PID, Name: r.Session, SessionID: "current-thread", Cwd: dir, Status: "idle"}})
	}))
	return a, dir, home
}

func TestSharedCodexAttachRunsNativeTUIOnCurrentThreadWithRoleSettings(t *testing.T) {
	for _, name := range []string{"dev", "dev#2"} {
		t.Run(name, func(t *testing.T) {
			a, dir, home := attachFixture(t, name, "shared")
			t.Setenv("CODEX_HOME", "wrong-owner-home")
			process, err := a.attachProcess(name, false)
			require.NoError(t, err)
			assert.Equal(t, dir, process.Dir)
			out, err := process.Output()
			require.NoError(t, err)
			text := string(out)
			assert.Contains(t, text, "cwd="+dir+"\n")
			assert.Contains(t, text, "home="+home+"\n")
			assert.Contains(t, text, "role="+name+"\n")
			assert.Contains(t, text, "arg=resume\narg=current-thread\narg=--remote\narg=unix://\n")
			assert.Contains(t, text, "arg=--model\narg=test-model\n")
			assert.Contains(t, text, "arg=--profile\narg=test-profile\n")
			assert.Contains(t, text, "arg=model_reasoning_effort=\"high\"\n")
			assert.Contains(t, text, "arg=--sandbox\narg=read-only\narg=-c\narg=approval_policy=\"on-request\"\n")
			assert.NotContains(t, text, "stale-thread")
			assert.NotContains(t, text, "app-server")
		})
	}
}

func TestAttachRawAndClaudeKeepTmuxConsole(t *testing.T) {
	a, _, _ := attachFixture(t, "dev", "shared")
	for _, name := range []string{"dev", "dispatcher"} {
		process, err := a.attachProcess(name, true)
		require.NoError(t, err)
		assert.Contains(t, process.Args, "attach-session")
		assert.NotContains(t, process.Args, "resume")
	}
	process, err := a.attachProcess("dispatcher", false)
	require.NoError(t, err)
	assert.Contains(t, process.Args, "attach-session")
}

func TestSharedAttachRequiresLiveCurrentRegistryNotSavedThread(t *testing.T) {
	a, _, _ := attachFixture(t, "dev", "shared")
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		p, err := s.Tmux.Probe("dev")
		if err != nil {
			return err
		}
		return os.Remove(filepath.Join(s.Crew.StateDir(), "codex", fmt.Sprint(p.PID)+".json"))
	}))
	_, err := a.attachProcess("dev", false)
	require.ErrorContains(t, err, "no current Codex thread")
	_, err = a.attachProcess("unknown", false)
	require.Error(t, err)
}

func TestAttachWithoutTTYPrintsScopedWrapperCommand(t *testing.T) {
	a, _, _ := attachFixture(t, "dev", "shared")
	input, output, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = previous; _ = input.Close(); _ = output.Close() })
	for _, raw := range []bool{false, true} {
		args := []string{"--crew", a.crew, "attach", "dev"}
		if raw {
			args = append(args, "--raw")
		}
		root := NewRoot(io.Discard, io.Discard)
		root.SetArgs(args)
		err := root.Execute()
		require.ErrorContains(t, err, "no terminal here")
		assert.Contains(t, err.Error(), "'zmina' '--crew' "+quoteCommandArg(a.crew)+" 'attach' 'dev'")
		if raw {
			assert.Contains(t, err.Error(), "'--raw'")
		}
	}
}

func TestStdioCodexAttachExplainsRawInsteadOfOpeningAnotherWriter(t *testing.T) {
	a, _, _ := attachFixture(t, "dev", "stdio")
	process, err := a.attachProcess("dev", false)
	require.Nil(t, process)
	require.ErrorContains(t, err, "codex_transport: stdio")
	assert.Contains(t, err.Error(), "codex_transport: shared")
	assert.Contains(t, err.Error(), "'zmina' '--crew' "+quoteCommandArg(a.crew)+" 'attach' 'dev' '--raw'")
	assert.Contains(t, err.Error(), "stopping the role and its copies")
	process, err = a.attachProcess("dev", true)
	require.NoError(t, err)
	assert.Contains(t, process.Args, "attach-session")
	assert.NotContains(t, process.Args, "resume")
}
