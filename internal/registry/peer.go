package registry

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// peerTail is how much of a transcript's end Delivered reads for a message's id.
const peerTail = 8 << 20

// Delivered reports whether a message posted to the session's inbox socket reached its conversation: Claude Code
// records the message's origin, with the msg_id the sender gave it, on the line that carries it to the model.
func (r *Registry) Delivered(sessionID, msgID string) (bool, error) {
	path := r.transcript(sessionID)
	if path == "" || msgID == "" {
		return false, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return false, err
	}

	off := max(st.Size()-peerTail, 0)
	tail := make([]byte, st.Size()-off)

	if _, err := f.ReadAt(tail, off); err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}

	return bytes.Contains(tail, []byte(`"msg_id":"`+msgID+`"`)), nil
}
