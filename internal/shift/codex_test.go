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
