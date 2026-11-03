package peer

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
)

// listen is a session's inbox socket that hands back the lines of the first connection.
func listen(t *testing.T) (string, <-chan []string) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "zp") // a unix socket path has to stay short
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	got := make(chan []string, 1)

	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()

		var lines []string
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		got <- lines
	}()

	return path, got
}

func TestPostWritesTheFrame(t *testing.T) {
	path, got := listen(t)
	ns := t.TempDir()
	s := &registry.Session{PID: 4242, SessionID: "sid-1", MessagingSocketPath: path, PeerProtocol: 1, ProcStart: "Mon"}

	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "sessions", fmt.Sprintf("4242.%s.key", hex.EncodeToString(sum[:]))),
		[]byte(`{"peerToken":"0123456789abcdef0123456789abcdef","procStart":"Mon"}`), 0o600))

	id, err := Client{}.Post(ns, s, "[zmina] T-1 result ready")
	require.NoError(t, err)

	lines := <-got
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"type":"auth","token":"0123456789abcdef0123456789abcdef"}`, lines[0])

	var frame map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &frame))
	assert.Equal(t, "user", frame["type"])
	assert.Equal(t, id, frame["msg_id"])
	assert.Equal(t, "sid-1", frame["session_id"])
	assert.Equal(t, From, frame["from"])
	assert.Equal(t, map[string]any{"role": "user", "content": "[zmina] T-1 result ready"}, frame["message"])
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, id)
}

func TestPostWithoutAKeyFile(t *testing.T) {
	path, got := listen(t)
	s := &registry.Session{PID: 1, SessionID: "sid", MessagingSocketPath: path, PeerProtocol: 1}

	_, err := Client{}.Post(t.TempDir(), s, "hello")
	require.NoError(t, err)
	assert.Len(t, <-got, 1, "no auth line: optional on macOS and Linux")
}

func TestUnusable(t *testing.T) {
	path, _ := listen(t)

	for name, s := range map[string]*registry.Session{
		"no socket":      {MessagingSocketPath: "", PeerProtocol: 1},
		"other protocol": {MessagingSocketPath: path, PeerProtocol: 2},
		"gone":           {MessagingSocketPath: path + ".gone", PeerProtocol: 1},
	} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, Usable(s))

			_, err := Client{}.Post(t.TempDir(), s, "x")
			assert.True(t, errors.Is(err, ErrUnavailable))
		})
	}

	assert.True(t, Usable(&registry.Session{MessagingSocketPath: path, PeerProtocol: 1}))
}

func TestDelivered(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sid-1.jsonl"), []byte(
		`{"type":"user","origin":{"kind":"peer","from":"zmina","msg_id":"aaa-1"}}`+"\n"), 0o600))

	ok, err := Inbox{}.Delivered(ns, "sid-1", "aaa-1")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = Inbox{}.Delivered(ns, "sid-1", "aaa-2")
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = Inbox{}.Delivered(ns, "sid-none", "aaa-1")
	require.NoError(t, err)
	assert.False(t, ok, "no transcript yet")
}
