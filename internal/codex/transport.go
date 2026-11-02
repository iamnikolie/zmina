package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"
)

// sharedStream adapts JSON-RPC WebSocket frames to the bridge's JSONL stream.
// The control socket speaks WebSocket, not raw stdio (codex app-server proxy forwards raw bytes).
type sharedStream struct {
	conn  *websocket.Conn
	frame *bytes.Reader
}

func sharedSocket() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		base, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(base, ".codex")
	}
	return filepath.Join(home, "app-server-control", "app-server-control.sock"), nil
}

func dialShared(socket string) (*sharedStream, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	conn, resp, err := dialer.Dial("ws://localhost/", nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("connect shared Codex server at %s: %w (start it with codex app-server daemon start)", socket, err)
	}
	conn.SetReadLimit(16 << 20)
	return &sharedStream{conn: conn}, nil
}

func (s *sharedStream) Read(p []byte) (int, error) {
	for s.frame == nil || s.frame.Len() == 0 {
		kind, data, err := s.conn.ReadMessage()
		if err != nil {
			return 0, err
		}
		if kind != websocket.TextMessage {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return 0, err
		}
		s.frame = bytes.NewReader(append(compact.Bytes(), '\n'))
	}
	return s.frame.Read(p)
}
func (s *sharedStream) Write(p []byte) (int, error) {
	if err := s.conn.WriteMessage(websocket.TextMessage, bytes.TrimSpace(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (s *sharedStream) Close() error { return s.conn.Close() }

var _ io.ReadWriteCloser = (*sharedStream)(nil)
