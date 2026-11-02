package codex

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharedTransportKeepsDaemonAlive(t *testing.T) {
	// macOS limits Unix socket names to 104 bytes, shorter than testing's normal temp paths.
	home, err := os.MkdirTemp("/tmp", "zmina-ws-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("CODEX_HOME", home)
	socket, err := sharedSocket()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	upgrader := websocket.Upgrader{}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		conn, err := upgrader.Upgrade(w, q, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			var msg message
			if err = conn.ReadJSON(&msg); err != nil {
				return
			}
			if len(msg.ID) == 0 {
				continue
			}
			result := any(map[string]any{})
			if msg.Method == "thread/start" {
				result = map[string]any{"thread": map[string]string{"id": "shared-thread"}, "model": "test-model"}
			}
			if err = conn.WriteJSON(map[string]any{"id": msg.ID, "result": result}); err != nil {
				return
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	input, send := io.Pipe()
	defer func() { _ = input.Close(); _ = send.Close() }()
	reg := Registry{Dir: t.TempDir()}
	finished := make(chan error, 1)
	go func() {
		finished <- Run(Options{Transport: "shared", Name: "test-seat", Registry: reg}, input, io.Discard, io.Discard)
	}()
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-finished:
			require.NoError(t, err)
			t.Fatal("bridge exited before idle")
		default:
		}
		rec, _ := reg.ByPID(os.Getpid())
		if rec != nil && rec.Status == "idle" && rec.SessionID == "shared-thread" {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, ready, "shared bridge did not become idle")
	// A second connection can use the daemon while the bridge is still connected.
	other, err := dialShared(socket)
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(other).Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{}}))
	var got message
	require.NoError(t, json.NewDecoder(other).Decode(&got))
	assert.JSONEq(t, `1`, string(got.ID))
	require.NoError(t, other.Close())
	_, err = io.WriteString(send, "/exit\n")
	require.NoError(t, err)
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit")
	}
	// Closing a role must not terminate the server shared with the app or other roles.
	after, err := dialShared(socket)
	require.NoError(t, err)
	require.NoError(t, after.Close())
}

func TestSharedNotificationsAndApprovalStatus(t *testing.T) {
	r, _, _ := testRunner(t)
	r.rec.SessionID = "thread"
	notify(t, r, "account/updated", `{"status":"ready"}`)
	notify(t, r, "thread/status/changed", `{"threadId":"thread","status":{"type":"active","activeFlags":["waitingOnApproval"]}}`)
	assert.Equal(t, "waiting", r.rec.Status)
	notify(t, r, "thread/status/changed", `{"threadId":"other","status":{"type":"idle"}}`)
	assert.Equal(t, "waiting", r.rec.Status)
	notify(t, r, "thread/status/changed", `{"threadId":"thread","status":{"type":"idle"}}`)
	assert.Equal(t, "idle", r.rec.Status)
}
