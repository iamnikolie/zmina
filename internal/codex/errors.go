package codex

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (r *runner) rpcError(method string, raw json.RawMessage) error {
	base := fmt.Errorf("codex %s: %s", method, raw)
	if !strings.Contains(strings.ToLower(string(raw)), "already has an active writer") {
		return base
	}
	thread := r.rec.SessionID
	if method == "thread/resume" || thread == "" {
		thread = r.o.Resume
	}
	if thread == "" {
		thread = "unknown (no thread ID reported)"
	}
	role := r.o.Role
	if role == "" {
		role = "<role>"
	}
	return fmt.Errorf("%w; thread %q is held by a Codex client outside the shared app-server daemon. Close that external client, then retry up/resume. For shared human access use zmina attach %s, or codex resume <thread> --remote unix:// with the same CODEX_HOME", base, thread, role)
}
