package shift

import (
	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexCommandAndQueue(t *testing.T) {
	s, _, _, _ := fixture(t)
	r, err := s.Role("designer")
	require.NoError(t, err)
	r.Runtime = "codex"
	r.Model = "test-model"
	r.ReasoningEffort = "high"
	r.Sandbox = "read-only"
	r.ApprovalPolicy = "on-request"
	s.Crew.Codex = "/opt/bin/codex"
	argv := s.Command(r, "saved-thread")
	assert.Contains(t, argv, "_codex")
	assert.Contains(t, argv, "saved-thread")
	assert.Contains(t, argv, "high")
	assert.NotContains(t, argv, "--remote-control")
	assert.NotContains(t, argv, "--name")
	env := s.Env(r)
	assert.Contains(t, env, "CODEX_HOME="+os.Getenv("HOME")+"/.codex")
	for _, v := range env {
		assert.NotContains(t, v, "CLAUDE_CONFIG_DIR=")
	}
	reg := &codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
	require.NoError(t, s.Up("designer"))
	p, err := s.probe("designer")
	require.NoError(t, err)
	rec := codex.Record{Session: registry.Session{PID: p.PID, Name: r.Session, SessionID: "saved-thread", Status: "waiting", StatusUpdatedAt: s.Now().UnixMilli()}}
	require.NoError(t, reg.Save(rec))
	s.State.Role("designer").Started = s.Now().Add(-time.Hour)
	s.State.Role("designer").Inbox = []string{"do work"}
	assert.Empty(t, s.deliverInbox("designer", s.Now()), "queued work cannot answer an approval")
	rec.Status = "idle"
	require.NoError(t, reg.Save(rec))
	assert.Contains(t, s.deliverInbox("designer", s.Now()), "do work")
	s.remember("designer", p)
	assert.Equal(t, "saved-thread", s.State.Role("designer").LastSession)
}

func TestRuntimeSwitchCannotResumeClaudeSession(t *testing.T) {
	s, _, _, _ := fixture(t)
	r, err := s.Role("designer")
	require.NoError(t, err)
	r.Runtime = "codex"
	r.Resume = "always"
	s.State.Role("designer").LastSession = "claude-conversation"
	assert.Contains(t, s.BootText(s.Crew.Roles["dispatcher"]), MixedRuntimeRule)
	require.ErrorContains(t, s.UpWith("designer", UpOptions{Resume: "last"}), "belongs to claude")
	require.NoError(t, s.Up("designer"))
	assert.Equal(t, "", s.State.Role("designer").LastRuntime, "legacy state remains readable")
}

func TestCodexBusyWithoutInputIsNotBlockedAndWaitingNoticesRevalidate(t *testing.T) {
	s, tm, _, _ := fixture(t)
	r, err := s.Role("designer")
	require.NoError(t, err)
	r.Runtime = "codex"
	require.NoError(t, s.Up("designer"))
	p, err := s.probe("designer")
	require.NoError(t, err)
	reg := &codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
	rec := codex.Record{Session: registry.Session{PID: p.PID, Name: r.Session, SessionID: "thread", Status: "busy", StatusUpdatedAt: s.Now().UnixMilli()}}
	require.NoError(t, reg.Save(rec))
	now := s.Now().Add(time.Hour)
	s.State.Role("designer").Started = now.Add(-time.Hour)
	tm.drafts["designer"] = "-"
	assert.Empty(t, s.watchScreen("designer", now))
	assert.Empty(t, s.watchScreen("designer", now.Add(time.Hour)), "normal long work has no input prompt")
	for _, v := range s.Views() {
		if v.Role == "designer" {
			assert.False(t, v.Prompt)
		}
	}
	rec.Status = "waiting"
	require.NoError(t, reg.Save(rec))
	assert.Empty(t, s.watchScreen("designer", now.Add(2*time.Hour)))
	notes := s.watchScreen("designer", now.Add(3*time.Hour))
	require.Len(t, notes, 1)
	s.addNotes(notes)
	require.Len(t, s.currentNotes(), 1)
	for _, v := range s.Views() {
		if v.Role == "designer" {
			assert.True(t, v.Prompt)
		}
	}
	rec.Status = "busy"
	require.NoError(t, reg.Save(rec))
	assert.Empty(t, s.currentNotes(), "an answered approval cannot leave a stale blocked alert")
	assert.Empty(t, s.watchScreen("designer", now.Add(4*time.Hour)))
	assert.True(t, s.State.Role("designer").NoPrompt.IsZero())
}
