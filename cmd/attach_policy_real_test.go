package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/require"
)

func TestNativeCodexApprovalCompatibility(t *testing.T) {
	real, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI not installed; fake-binary coverage still runs")
	}
	probeHome := t.TempDir()
	probeEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + probeHome, "CODEX_HOME=" + probeHome}
	helpCmd := exec.Command(real, "resume", "--help")
	helpCmd.Env = probeEnv
	help, err := helpCmd.CombinedOutput()
	require.NoError(t, err)
	if !strings.Contains(string(help), "--remote") {
		t.Skip("installed Codex predates shared remote TUI support")
	}
	versionCmd := exec.Command(real, "--version")
	versionCmd.Env = probeEnv
	version, err := versionCmd.CombinedOutput()
	require.NoError(t, err)
	t.Log(strings.TrimSpace(string(version)))
	for _, policy := range []string{"on-request", "never", "untrusted"} {
		t.Run(policy, func(t *testing.T) {
			verifyNativeCodexApproval(t, real, policy)
		})
	}
}

func verifyNativeCodexApproval(t *testing.T, real, policy string) {
	t.Helper()
	a, dir, oldHome := attachFixture(t, "dev", "shared")
	// Keep Unix socket paths below macOS's 104-byte limit. No production config/auth/daemon is used.
	home, err := os.MkdirTemp("/tmp", "zmina-policy-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	require.NoError(t, os.MkdirAll(filepath.Join(home, "app-server-control"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "test-profile.config.toml"), []byte("model = \"test-model\"\n"), 0600))
	// Conflicting client defaults catch accidental weakening of the existing untrusted policy.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(provider.Close)
	config := fmt.Sprintf(`approval_policy = "never"
model_provider = "fixture"
[model_providers.fixture]
name = "fixture"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
[analytics]
enabled = false
`, provider.URL+"/v1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600))
	socket := filepath.Join(home, "app-server-control", "app-server-control.sock")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "TERM=xterm-256color", "LANG=en_US.UTF-8"}
	log, err := os.Create(filepath.Join(home, "server.log"))
	require.NoError(t, err)
	defer func() { _ = log.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, real, "app-server", "--listen", "unix://"+socket)
	server.Dir = home
	server.Env = env
	server.Stdout = log
	server.Stderr = log
	require.NoError(t, server.Start())
	defer func() { _ = server.Process.Kill(); _ = server.Wait() }()
	require.Eventually(t, func() bool { _, err := os.Stat(socket); return err == nil }, 5*time.Second, 20*time.Millisecond)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	conn, _, err := dialer.Dial("ws://localhost/", nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	seq := 0
	call := func(method string, params any) (json.RawMessage, error) {
		seq++
		if err := conn.WriteJSON(map[string]any{"id": seq, "method": method, "params": params}); err != nil {
			return nil, err
		}
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return nil, err
		}
		for {
			var reply struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := conn.ReadJSON(&reply); err != nil {
				return nil, err
			}
			if reply.ID != seq {
				continue
			}
			if len(reply.Error) > 0 && string(reply.Error) != "null" {
				return nil, fmt.Errorf("%s: %s", method, reply.Error)
			}
			return reply.Result, nil
		}
	}
	mustCall := func(method string, params any) json.RawMessage {
		result, err := call(method, params)
		require.NoError(t, err)
		return result
	}
	mustCall("initialize", map[string]any{"clientInfo": map[string]string{"name": "zmina-test", "version": "test"}})
	require.NoError(t, conn.WriteJSON(map[string]any{"method": "initialized", "params": map[string]any{}}))
	created := mustCall("thread/start", map[string]any{"cwd": dir, "model": "test-model", "sandbox": "read-only", "approvalPolicy": policy})
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Approval string `json:"approvalPolicy"`
	}
	require.NoError(t, json.Unmarshal(created, &thread))
	require.Equal(t, policy, thread.Approval)
	// A failed fixture turn creates the rollout needed by native resume, with no credentials/inference.
	started := mustCall("turn/start", map[string]any{"threadId": thread.Thread.ID, "input": []any{map[string]any{"type": "text", "text": "Fixture only.", "text_elements": []any{}}}})
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	require.NoError(t, json.Unmarshal(started, &turn))
	require.Eventually(t, func() bool { return len(fixtureTurnPolicies(home)) > 0 }, 5*time.Second, 20*time.Millisecond)
	_, _ = call("turn/interrupt", map[string]any{"threadId": thread.Thread.ID, "turnId": turn.Turn.ID})
	raw, err := os.ReadFile(a.crew)
	require.NoError(t, err)
	text := strings.Replace(strings.Replace(string(raw), oldHome, home, 1), "approval_policy: on-request", "approval_policy: "+policy, 1)
	require.NoError(t, os.WriteFile(a.crew, []byte(text), 0600))
	require.NoError(t, a.readState(func(s *shift.Shift) error {
		pane, err := s.Tmux.Probe("dev")
		if err != nil {
			return err
		}
		reg := codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
		rec, err := reg.ByPID(pane.PID)
		if err != nil {
			return err
		}
		rec.SessionID = thread.Thread.ID
		return reg.Save(codex.Record{Session: *rec})
	}))
	process, err := a.attachProcess("dev", false)
	require.NoError(t, err)
	process.Path = real
	process.Env = attachEnvironment(env, []string{"ZMINA_ROLE=dev", "ZMINA_CREW=" + a.crew})
	terminal, err := pty.StartWithSize(process, &pty.Winsize{Rows: 40, Cols: 120})
	require.NoError(t, err)
	capture, err := os.Create(filepath.Join(home, "tui.log"))
	require.NoError(t, err)
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(capture, terminal); close(copied) }()
	defer func() {
		_ = process.Process.Kill()
		_ = process.Wait()
		_ = terminal.Close()
		<-copied
		_ = capture.Close()
	}()
	require.Eventually(t, func() bool {
		body, _ := os.ReadFile(capture.Name())
		return strings.Contains(string(body), "test-model")
	}, 5*time.Second, 20*time.Millisecond)
	// Separate text and Enter so native paste-burst protection does not swallow submission.
	time.Sleep(300 * time.Millisecond)
	_, err = fmt.Fprint(terminal, "fixture-check-policy")
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	_, err = fmt.Fprint(terminal, "\r")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(fixtureTurnPolicies(home)) >= 2 }, 5*time.Second, 20*time.Millisecond, "native TUI must start its own human turn")
	policies := fixtureTurnPolicies(home)
	require.Equal(t, policy, policies[len(policies)-1], "actual human turn policy must not weaken to client defaults")
	resumed := mustCall("thread/resume", map[string]any{"threadId": thread.Thread.ID})
	require.NoError(t, json.Unmarshal(resumed, &thread))
	require.Equal(t, policy, thread.Approval, "original client retains the same thread and policy")
}

func fixtureTurnPolicies(home string) []string {
	paths, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "*.jsonl"))
	var policies []string
	for _, path := range paths {
		raw, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(raw), "\n") {
			var event struct {
				Type    string `json:"type"`
				Payload struct {
					Policy string `json:"approval_policy"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(line), &event) == nil && event.Type == "turn_context" {
				policies = append(policies, event.Payload.Policy)
			}
		}
	}
	return policies
}
