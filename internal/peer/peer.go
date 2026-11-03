// Package peer posts lines to a Claude Code session's inbox socket (cross-session messaging): the line arrives as a
// message from another session — shown apart from what the owner types, read between tool calls while the session
// works and starting a turn while it is idle — instead of being typed into its input line.
//
// The socket and the first line ({"type":"auth"}) are documented for scripts; the message frame is the one Claude
// Code's own sessions write (peerProtocol 1), which zmina was checked against.
package peer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
)

// Protocol is the peerProtocol version this frame was checked against; a session that advertises another is typed
// into instead.
const Protocol = 1

// lineMax is Claude Code's cap on one frame line.
const lineMax = 1 << 20

// From names zmina as the sender.
const From = "zmina"

// ErrUnavailable is a post that never started: nothing reached the session, so another way may be tried.
var ErrUnavailable = errors.New("inbox socket unavailable")

// Committed is a post whose write broke after it started: the session may have the message or not, so it is never
// tried another way. Post returns the message id with it, so the caller can still look for the message.
type Committed struct{ Err error }

func (c *Committed) Error() string {
	return "inbox socket write broke after it started: " + c.Err.Error()
}

func (c *Committed) Unwrap() error { return c.Err }

// Usable reports whether a session takes posts zmina knows how to write.
func Usable(s *registry.Session) bool {
	if s == nil || s.MessagingSocketPath == "" || s.PeerProtocol != Protocol {
		return false
	}

	st, err := os.Stat(s.MessagingSocketPath)

	return err == nil && st.Mode()&os.ModeSocket != 0
}

// Client posts lines.
type Client struct {
	// Timeout bounds the dial and the write (default 5s).
	Timeout time.Duration
}

// Post sends one line to the session as a message from zmina and returns the message's id, which the session's
// transcript carries once the message reached its conversation (registry.Delivered).
func (c Client) Post(namespace string, s *registry.Session, text string) (string, error) {
	if !Usable(s) {
		return "", fmt.Errorf("%w: the session has no inbox socket zmina can write", ErrUnavailable)
	}

	id, err := newID()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	var out []byte

	if token := peerToken(namespace, s); token != "" {
		auth, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
		out = append(append(out, auth...), '\n')
	}

	frame, err := json.Marshal(map[string]any{
		"msgV": 1, "msg_id": id, "type": "user", "session_id": s.SessionID, "from": From, "priority": "next",
		"message": map[string]string{"role": "user", "content": text},
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	out = append(append(out, frame...), '\n')
	if len(frame) >= lineMax {
		return "", fmt.Errorf("%w: the message is over the %d-character line cap", ErrUnavailable, lineMax)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	conn, err := net.DialTimeout("unix", s.MessagingSocketPath, timeout)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetWriteDeadline(time.Now().Add(timeout))

	if _, err := conn.Write(out); err != nil {
		return id, &Committed{Err: err}
	}

	// Claude Code's own client waits a moment before it closes, so the session reads the line first.
	time.Sleep(150 * time.Millisecond)

	return id, nil
}

var tokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// peerToken is the session's own token from its key file (<pid>.<sha256 of the socket path>.key), empty when there
// is none: on macOS and Linux the auth line is optional.
func peerToken(namespace string, s *registry.Session) string {
	sum := sha256.Sum256([]byte(filepath.Clean(s.MessagingSocketPath)))
	path := filepath.Join(namespace, "sessions", fmt.Sprintf("%d.%s.key", s.PID, hex.EncodeToString(sum[:])))

	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm()&0o077 != 0 {
		return ""
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var key struct {
		PeerToken string `json:"peerToken"`
		ProcStart string `json:"procStart"`
	}

	if json.Unmarshal(raw, &key) != nil || !tokenRe.MatchString(key.PeerToken) {
		return ""
	}

	// A key file of an earlier process under the same pid is not this session's.
	if s.ProcStart != "" && key.ProcStart != "" && key.ProcStart != s.ProcStart {
		return ""
	}

	return key.PeerToken
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)

	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// Inbox is the Client with the check that a posted line reached the session.
type Inbox struct{ Client }

// Usable reports whether a session takes posts.
func (Inbox) Usable(s *registry.Session) bool { return Usable(s) }

// Delivered reports whether the message reached the session's conversation.
func (Inbox) Delivered(namespace, sessionID, msgID string) (bool, error) {
	return (&registry.Registry{Namespace: namespace}).Delivered(sessionID, msgID)
}
