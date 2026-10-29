package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// entry is a session entry in the shape Claude Code 2.1 writes (fields zmina does not read included).
func entry(pid int, name, status string, at time.Time) string {
	return fmt.Sprintf(`{"pid": %d, "sessionId": "sid-%d", "cwd": "/work", "startedAt": 1761300000000,
  "procStart": "Mon Oct 26 10:00:00 2026", "version": "2.1.310", "peerProtocol": 1, "peerFeatures": [],
  "kind": "interactive", "entrypoint": "cli", "pidDomain": "host", "tmux": "", "messagingSocketPath": "/tmp/s",
  "name": %q, "nameSource": "flag", "nameSince": 1761300000000, "updatedAt": 1761300000000,
  "status": %q, "statusUpdatedAt": %d, "bridgeSessionId": "b"}`, pid, pid, name, status, at.UnixMilli())
}

func TestRegistry(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	me := os.Getpid()

	write := func(pid int, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), []byte(body), 0o600))
	}

	write(me, entry(me, "zm-dev", "busy", at))
	write(999999, entry(999999, "zm-designer", "idle", at)) // a pid that does not run
	write(4, `{"pid": 4, "status": `)

	r := &Registry{Namespace: ns}

	s, err := r.ByPID(me)
	require.NoError(t, err)
	assert.Equal(t, "zm-dev", s.Name)
	assert.Equal(t, "busy", s.Status)
	assert.Equal(t, fmt.Sprintf("sid-%d", me), s.SessionID)
	assert.Equal(t, at, s.StatusSince().UTC())

	s, err = r.ByPID(12345)
	require.NoError(t, err)
	assert.Nil(t, s, "no entry yet: nil, not an error")

	_, err = r.ByPID(4)
	assert.Error(t, err, "a broken entry is an error, not an idle role")

	assert.NotNil(t, r.LiveByName("zm-dev"))
	assert.Nil(t, r.LiveByName("zm-designer"), "the entry of a process that is gone does not count")
	assert.Nil(t, r.LiveByName("zm-nobody"))
}

func TestContext(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	r := &Registry{Namespace: ns}

	turn := func(read, side string) string {
		return fmt.Sprintf(`{"type":"assistant","isSidechain":%s,"sessionId":"s1","message":{"role":"assistant",`+
			`"usage":{"input_tokens":2,"cache_creation_input_tokens":640,"cache_read_input_tokens":%s,`+
			`"output_tokens":358,"service_tier":"standard"}}}`, side, read)
	}

	write := func(lines ...string) {
		body := ""
		for _, l := range lines {
			body += l + "\n"
		}

		require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o600))
	}

	n, err := r.Context("s1")
	require.NoError(t, err)
	assert.Zero(t, n, "no transcript yet")

	user := `{"type":"user","message":{"role":"user","content":"hi"}}`
	synthetic := `{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":0,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`
	write(user, turn("100000", "false"), user, turn("150000", "false"), turn("5", "true"), synthetic,
		`{"type":"user",`)

	n, err = r.Context("s1")
	require.NoError(t, err)
	assert.Equal(t, 151000, n, "the last turn of the main conversation; sidechains, API errors and a half-written line skipped")

	write(turn("150000", "false"), `{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"summary"}}`)

	n, err = r.Context("s1")
	require.NoError(t, err)
	assert.Zero(t, n, "compacted since the last turn")

	n, err = r.Context("../s1")
	require.NoError(t, err)
	assert.Zero(t, n, "a session id is never a path")
}

func TestLastTurnError(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	turn := `{"type":"assistant","uuid":"a1","message":{"id":"m1","usage":{"input_tokens":10,"cache_read_input_tokens":90000,"output_tokens":5}}}`
	limit := `{"type":"assistant","uuid":"e1","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[` +
		`{"type":"text","text":"Claude usage limit reached. Your limit will reset at 9pm."}],"usage":{"input_tokens":0,` +
		`"output_tokens":0}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(turn+"\n"+limit+"\n"), 0o600))

	got, err := (&Registry{Namespace: ns}).Last("s1")
	require.NoError(t, err)
	assert.Equal(t, Turn{Context: 90015, Error: "Claude usage limit reached. Your limit will reset at 9pm.", ErrorID: "e1"}, got)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(limit+"\n"+turn+"\n"), 0o600))

	got, err = (&Registry{Namespace: ns}).Last("s1")
	require.NoError(t, err)
	assert.Empty(t, got.Error, "a turn after the error: it is over")
}

func TestUsageSince(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s1", "subagents"), 0o700))

	at := func(ts, id string, read int) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"usage":{"input_tokens":2,`+
			`"cache_creation_input_tokens":100,"cache_read_input_tokens":%d,"output_tokens":10}}}`, ts, id, read)
	}

	head := `{"type":"custom-title","customTitle":"zm-dev"}` + "\n" + `{"type":"agent-name","agentName":"zm-dev"}` + "\n"
	main := head + at("2026-10-26T08:00:00Z", "old", 1) + "\n" +
		at("2026-10-26T12:00:00Z", "m1", 1000) + "\n" + at("2026-10-26T12:00:00Z", "m1", 1000) + "\n" + // one reply, two lines
		at("2026-10-26T13:00:00Z", "m2", 2000) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(main), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1", "subagents", "agent-x.jsonl"),
		[]byte(at("2026-10-26T12:30:00Z", "sub1", 500)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s2.jsonl"),
		[]byte(`{"type":"agent-name","agentName":"someone-else"}`+"\n"+at("2026-10-26T12:00:00Z", "x", 9)+"\n"), 0o600))

	since := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	got, err := (&Registry{Namespace: ns}).UsageSince([]string{"zm-dev"}, since.Add(-48*time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1, "only the crew's sessions")

	got, err = (&Registry{Namespace: ns}).UsageSince([]string{"zm-dev"}, since)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, Usage{Session: "zm-dev", ID: "s1", Turns: 3, Input: 6, CacheWrite: 300, CacheRead: 3500, Output: 30,
		Last: time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC)}, got[0])
}
