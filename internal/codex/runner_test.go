package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRunner(t *testing.T) (*runner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	wire, out := new(bytes.Buffer), new(bytes.Buffer)
	r := &runner{o: Options{Registry: Registry{Dir: t.TempDir()}, Model: "test-model", Effort: "high", Sandbox: "read-only", Approval: "on-request"}, out: out, enc: json.NewEncoder(wire), pending: map[int]string{}}
	r.rec.PID = os.Getpid()
	return r, wire, out
}
func notify(t *testing.T, r *runner, method, params string) {
	t.Helper()
	require.NoError(t, r.handle(message{Method: method, Params: json.RawMessage(params)}))
}
func response(t *testing.T, r *runner, result string) {
	t.Helper()
	id, _ := json.Marshal(r.next)
	require.NoError(t, r.handle(message{ID: id, Result: json.RawMessage(result)}))
}
func TestLifecycleAndSettings(t *testing.T) {
	r, wire, _ := testRunner(t)
	r.o.Boot = "read AGENT.md"
	r.o.Resume = "saved-thread"
	require.NoError(t, r.call("initialize", map[string]any{}))
	response(t, r, `{}`)
	assert.Contains(t, wire.String(), `"method":"initialized"`)
	assert.Contains(t, wire.String(), `"method":"thread/resume"`)
	assert.Contains(t, wire.String(), `"threadId":"saved-thread"`)
	assert.Contains(t, wire.String(), `"sandbox":"read-only"`)
	assert.Contains(t, wire.String(), `"approvalPolicy":"on-request"`)
	response(t, r, `{"thread":{"id":"saved-thread"}}`)
	assert.Contains(t, wire.String(), `"effort":"high"`)
	assert.Contains(t, wire.String(), `"text":"read AGENT.md"`)
	response(t, r, `{"turn":{"id":"t1"}}`)
	assert.Equal(t, "busy", r.rec.Status)
	notify(t, r, "thread/tokenUsage/updated", `{"tokenUsage":{"last":{"totalTokens":123}}}`)
	notify(t, r, "item/completed", `{"item":{"type":"contextCompaction"}}`)
	assert.Equal(t, "busy", r.rec.Status, "automatic compaction cannot make an active turn idle")
	notify(t, r, "turn/completed", `{"turn":{"id":"t1","status":"completed"}}`)
	assert.Equal(t, "idle", r.rec.Status)
	sess, err := r.o.Registry.ByPID(os.Getpid())
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "saved-thread", sess.SessionID)
	last, err := r.o.Registry.Last(sess.SessionID)
	require.NoError(t, err)
	assert.True(t, last.Ended)
	assert.False(t, last.At.IsZero(), "a turn's end carries its time: the boot is over once one ended after the start")
	assert.WithinDuration(t, time.Now(), last.At, time.Minute)
	assert.Equal(t, 123, last.Context)
	require.NoError(t, r.input("next message"))
	assert.Equal(t, "busy", r.rec.Status)
	assert.False(t, r.rec.Turn.Ended)
	assert.True(t, r.rec.Turn.At.IsZero(), "a new turn clears the time of the last end")
}
func TestApprovalNeverConsumesQueuedWork(t *testing.T) {
	r, wire, out := testRunner(t)
	r.turn = "t1"
	r.rec.SessionID = "thread"
	require.NoError(t, r.handle(message{ID: json.RawMessage(`"approval-1"`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"command":"touch example"}`)}))
	assert.Equal(t, "waiting", r.rec.Status)
	require.NoError(t, r.input("a task is not consent"))
	assert.Empty(t, wire.String())
	assert.NotNil(t, r.request)
	assert.Contains(t, out.String(), "touch example")
	require.NoError(t, r.input("n"))
	assert.Contains(t, wire.String(), `"decision":"decline"`)
	assert.Contains(t, wire.String(), `"id":"approval-1"`)
	assert.Nil(t, r.request)
	assert.Equal(t, "busy", r.rec.Status)
}
func TestFailedTurnAndExplicitCompaction(t *testing.T) {
	r, _, _ := testRunner(t)
	r.rec.SessionID = "thread"
	notify(t, r, "turn/completed", `{"turn":{"id":"t1","status":"failed","error":{"message":"limit reached"}}}`)
	last, err := r.o.Registry.Last("thread")
	require.NoError(t, err)
	assert.Equal(t, "limit reached", last.Error)
	require.NoError(t, r.input("/compact"))
	assert.Equal(t, "busy", r.rec.Status)
	notify(t, r, "item/completed", `{"item":{"type":"contextCompaction"}}`)
	assert.Equal(t, "idle", r.rec.Status)
	assert.Zero(t, r.rec.Turn.Context)
}

// The child process implements a tiny App Server. No model, credentials or network required.
func TestMain(m *testing.M) {
	if os.Getenv("ZMINA_CODEX_TEST_CHILD") == "1" {
		dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		for {
			var msg message
			if dec.Decode(&msg) != nil {
				os.Exit(0)
			}
			if len(msg.ID) == 0 {
				continue
			}
			result := any(map[string]any{})
			switch msg.Method {
			case "thread/start", "thread/resume":
				result = map[string]any{"thread": map[string]string{"id": "fake-thread"}}
			case "turn/start":
				result = map[string]any{"turn": map[string]string{"id": "fake-turn"}}
			}
			_ = enc.Encode(map[string]any{"id": msg.ID, "result": result})
			if msg.Method == "turn/start" {
				_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"id": "fake-turn", "status": "completed"}}})
			}
		}
	}
	os.Exit(m.Run())
}
func TestRunSubprocess(t *testing.T) {
	t.Setenv("ZMINA_CODEX_TEST_CHILD", "1")
	bin, err := os.Executable()
	require.NoError(t, err)
	input, send := io.Pipe()
	defer func() { _ = input.Close() }()
	defer func() { _ = send.Close() }()
	reg := Registry{Dir: filepath.Join(t.TempDir(), "registry")}
	finished := make(chan error, 1)
	go func() {
		finished <- Run(Options{Bin: bin, Name: "test-seat", Dir: t.TempDir(), Boot: "boot", Registry: reg}, input, io.Discard, io.Discard)
	}()
	require.Eventually(t, func() bool {
		s, _ := reg.ByPID(os.Getpid())
		return s != nil && s.SessionID == "fake-thread" && s.Status == "idle"
	}, 5*time.Second, 10*time.Millisecond)
	_, err = io.WriteString(send, "next\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		s, _ := reg.ByPID(os.Getpid())
		return s != nil && s.Status == "idle"
	}, 5*time.Second, 10*time.Millisecond)
	_, err = io.WriteString(send, "/exit\n")
	require.NoError(t, err)
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not exit")
	}
	s, err := reg.ByPID(os.Getpid())
	require.NoError(t, err)
	assert.Nil(t, s)
}

func TestConcurrentRequestsAndResolution(t *testing.T) {
	r, wire, _ := testRunner(t)
	r.rec.SessionID = "thread"
	r.turn = "turn"
	for _, id := range []string{"1", "2"} {
		require.NoError(t, r.handle(message{ID: json.RawMessage(id), Method: "item/fileChange/requestApproval", Params: json.RawMessage(`{}`)}))
	}
	require.Len(t, r.requests, 1)
	require.NoError(t, r.input("n"))
	assert.Equal(t, "waiting", r.rec.Status)
	require.NotNil(t, r.request)
	assert.Equal(t, "2", string(r.request.ID))
	assert.Contains(t, wire.String(), `"decision":"decline"`)
	notify(t, r, "serverRequest/resolved", `{"requestId":2}`)
	assert.Nil(t, r.request)
	assert.Equal(t, "busy", r.rec.Status)
}
