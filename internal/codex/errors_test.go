package codex

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActiveWriterFailureNamesThreadAndSharedRecovery(t *testing.T) {
	for _, method := range []string{"thread/resume", "turn/start"} {
		t.Run(method, func(t *testing.T) {
			r, _, _ := testRunner(t)
			r.o.Role = "dev"
			r.o.Resume = "held-resume-thread"
			if method == "turn/start" {
				r.rec.SessionID = "live-thread"
			}
			require.NoError(t, r.call(method, map[string]string{}))
			id, _ := json.Marshal(r.next)
			err := r.handle(message{ID: id, Error: json.RawMessage(`{"code":-32600,"message":"thread already has an active writer"}`)})
			require.Error(t, err)
			if method == "thread/resume" {
				assert.Contains(t, err.Error(), "held-resume-thread")
			} else {
				assert.Contains(t, err.Error(), "live-thread")
			}
			assert.Contains(t, err.Error(), "outside the shared app-server daemon")
			assert.Contains(t, err.Error(), "Close that external client")
			assert.Contains(t, err.Error(), "zmina attach dev")
			assert.Contains(t, err.Error(), "--remote unix://")
			assert.Contains(t, err.Error(), "same CODEX_HOME")
			assert.Contains(t, err.Error(), `"code":-32600`, "preserve original server diagnostic")
		})
	}
}

func TestUnrelatedRPCFailureKeepsOriginalDiagnostic(t *testing.T) {
	r, _, _ := testRunner(t)
	require.NoError(t, r.call("thread/resume", map[string]string{}))
	id, _ := json.Marshal(r.next)
	err := r.handle(message{ID: id, Error: json.RawMessage(`{"code":-32600,"message":"thread not found"}`)})
	require.EqualError(t, err, `codex thread/resume: {"code":-32600,"message":"thread not found"}`)
}
