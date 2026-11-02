package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/tmux"
	"github.com/stretchr/testify/require"
)

type browserTTY struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
	size   chan [2]uint16
}

func (t *browserTTY) Resize(x, y uint16) error { t.size <- [2]uint16{x, y}; return nil }
func (t *browserTTY) Close() error {
	t.once.Do(func() { close(t.closed); _ = t.Conn.Close() })
	return nil
}

func TestBrowserTerminalBytesResizeAndDisconnect(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	tty := &browserTTY{Conn: a, closed: make(chan struct{}), size: make(chan [2]uint16, 1)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveBrowserTerminal(w, r, func(_ context.Context, node, crew, role string, x, y uint16) (mesh.Terminal, error) {
			require.Equal(t, "worker", node)
			require.Equal(t, "ops", crew)
			require.Equal(t, "dev", role)
			require.Equal(t, uint16(80), x)
			require.Equal(t, uint16(24), y)
			return tty, nil
		})
	}))
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"?node=worker&crew=ops&role=dev&cols=80&rows=24", http.Header{"Origin": []string{srv.URL}})
	require.NoError(t, err)
	var ready map[string]string
	require.NoError(t, ws.ReadJSON(&ready))
	require.Equal(t, "connected", ready["status"])
	require.NoError(t, ws.WriteMessage(websocket.BinaryMessage, []byte("hi\x03")))
	got := make([]byte, 3)
	_, err = io.ReadFull(b, got)
	require.NoError(t, err)
	require.Equal(t, "hi\x03", string(got))
	go func() { _, _ = b.Write([]byte("\x1b[2Jhello")) }()
	kind, body, err := ws.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.BinaryMessage, kind)
	require.Equal(t, "\x1b[2Jhello", string(body))
	require.NoError(t, ws.WriteJSON(map[string]any{"type": "resize", "cols": 42, "rows": 14}))
	require.Equal(t, [2]uint16{42, 14}, <-tty.size)
	require.NoError(t, ws.Close())
	select {
	case <-tty.closed:
	case <-time.After(time.Second):
		t.Fatal("browser disconnect did not detach")
	}
}

func TestBrowserTerminalRejectsForeignAndMissingOrigins(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveBrowserTerminal(w, r, func(context.Context, string, string, string, uint16, uint16) (mesh.Terminal, error) {
			called = true
			return nil, nil
		})
	}))
	defer srv.Close()
	for _, origin := range []string{"", "https://foreign.invalid"} {
		ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"?cols=80&rows=24", http.Header{"Origin": []string{origin}})
		require.Error(t, err)
		require.Nil(t, ws)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		_ = resp.Body.Close()
	}
	require.False(t, called)
}

func TestTerminalUpgradeStillNeedsUIToken(t *testing.T) {
	handler := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unauthenticated request reached terminal") }), "127.0.0.1:7517", false, "secret")
	req := httptest.NewRequest("GET", "http://host/api/terminal/connect", nil)
	req.Header.Set("Origin", "http://host")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestProxyHostNeedsExplicitAllowAndToken(t *testing.T) {
	for _, tc := range []struct {
		name, token, host string
		allow             []string
		want              int
	}{
		{"unknown", "secret", "panel.example", nil, http.StatusForbidden},
		{"allowed", "secret", "panel.example", []string{"panel.example"}, http.StatusOK},
		{"wrong port", "secret", "panel.example:8443", []string{"panel.example"}, http.StatusForbidden},
		{"no token", "", "panel.example", []string{"panel.example"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }), "127.0.0.1:7518", true, tc.token, tc.allow...)
			req := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
			req.AddCookie(&http.Cookie{Name: uiCookie, Value: "secret"})
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			require.Equal(t, tc.want, out.Code)
		})
	}
}

func TestTerminalHistoryReadsScrollbackWithoutCopyMode(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	home := uiHome(t)
	srv, a := uiServer(t, true, "")
	c, err := a.crewApp("default")
	require.NoError(t, err)
	shift, err := c.newShift()
	require.NoError(t, err)
	client, err := tmux.New(shift.Crew.Socket())
	require.NoError(t, err)
	require.NoError(t, client.Start("dev", home, nil, []string{"sh", "-c", `i=1; while [ "$i" -le 400 ]; do printf 'HISTORY_%03d\n' "$i"; i=$((i+1)); done; exec cat`}))
	defer func() { _ = client.Kill("dev") }()
	require.Eventually(t, func() bool {
		text, e := a.localTerminalHistory("default", "dev", 10000)
		return e == nil && strings.Contains(text, "HISTORY_400")
	}, 3*time.Second, 20*time.Millisecond)
	response, err := http.Get(srv.URL + "/api/terminal/history?crew=default&role=dev")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var result map[string]string
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.Contains(t, result["text"], "HISTORY_001")
	require.Contains(t, result["text"], "HISTORY_400")
	cmd := exec.Command(client.Bin, "-L", client.Socket, "display-message", "-p", "-t", "=dev:", "#{pane_in_mode}")
	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "0", strings.TrimSpace(string(out)))
	_, err = a.localTerminalHistory("default", "not-a-role", 10000)
	require.Error(t, err)
}
