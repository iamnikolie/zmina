package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestMessages(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "s1", "subagents"), 0o700))

	at := func(ts, id, model string, read int, extra string) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/work/t1","message":{"id":%q,"model":%q,`+
			`"usage":{"input_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":%d,`+
			`"output_tokens":10%s}}}`, ts, id, model, read, extra)
	}
	user := func(ts, text, extra string) string {
		return fmt.Sprintf(`{"type":"user","timestamp":%q%s,"message":{"role":"user","content":%q}}`, ts, extra, text)
	}

	split := `,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":60},` +
		`"server_tool_use":{"web_search_requests":2},"speed":"standard"`
	main := strings.Join([]string{
		`{"type":"agent-name","agentName":"zm-dev"}`,
		user("2026-10-26T07:59:00Z", "fix the login", `,"origin":{"kind":"human"}`),
		at("2026-10-26T08:00:00Z", "old", "claude-opus-5-5", 1, ""),
		user("2026-10-26T11:59:00Z", "[zmina T-4] fix the login", ""),
		at("2026-10-26T12:00:00Z", "m1", "claude-opus-5-5", 1000, split),
		at("2026-10-26T12:00:00Z", "m1", "claude-opus-5-5", 1000, split), // one reply, two lines
		`{"type":"user","timestamp":"2026-10-26T12:01:00Z","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		user("2026-10-26T12:02:00Z", "<skill text>", `,"isMeta":true`),
		at("2026-10-26T12:03:00Z", "m2", "claude-opus-5-5", 1500, ""),
		user("2026-10-26T12:59:00Z", `<cross-session-message from="uds:x" from-name="zm-dispatcher">go on</cross-session-message>`,
			`,"origin":{"kind":"peer","name":"zm-dispatcher"}`),
		at("2026-10-26T13:00:00Z", "m3", "claude-sonnet-5-5", 2000, `,"speed":"fast"`),
		user("2026-10-26T13:10:00Z", "[zmina] dev idle 40m · dev2 stuck", ""),
		at("2026-10-26T13:11:00Z", "m4", "claude-opus-5-5", 10, ""),
		user("2026-10-26T13:20:00Z", "summary", `,"isCompactSummary":true`),
		at("2026-10-26T13:21:00Z", "m5", "claude-opus-5-5", 10, ""),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(main), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1", "subagents", "agent-x.jsonl"), []byte(
		user("2026-10-26T12:29:00Z", "look it up", `,"isSidechain":true`)+"\n"+
			at("2026-10-26T12:30:00Z", "sub1", "claude-haiku-5-5", 500, "")+"\n"+
			at("2026-10-26T12:30:00Z", "m1", "claude-opus-5-5", 1000, "")+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s2.jsonl"), []byte(at("2026-10-26T12:00:00Z", "x", "m", 9, "")+"\n"),
		0o600))

	reg := &Registry{Namespace: ns}
	ts, err := reg.Transcripts(time.Time{})
	require.NoError(t, err)
	require.Len(t, ts, 2)
	assert.Equal(t, Transcript{Path: filepath.Join(dir, "s1.jsonl"), ID: "s1", Name: "zm-dev"}, ts[0])
	assert.Empty(t, ts[1].Name, "an unnamed session")

	since := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	got, err := Messages(ts[0], since)
	require.NoError(t, err)

	var ids, triggers []string
	for _, m := range got {
		ids = append(ids, m.ID)
		triggers = append(triggers, m.Trigger)
	}

	assert.Equal(t, []string{"m1", "m2", "sub1", "m3", "m4", "m5"}, ids, "since, each reply once, by time")
	assert.Equal(t, []string{TriggerHandoff, TriggerHandoff, TriggerHandoff, TriggerPeer, TriggerNotice, TriggerCompact},
		triggers, "a tool result and a skill's text start no turn; a subagent's reply takes its parent's turn")

	assert.Equal(t, Message{ID: "m1", At: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), Model: "claude-opus-5-5",
		Input: 2, Write5m: 40, Write1h: 60, Read: 1000, Output: 10, WebSearch: 2, Cwd: "/work/t1",
		Trigger: TriggerHandoff}, got[0])
	assert.Equal(t, 100, got[1].Write5m, "no split: the API's default lifetime")
	assert.True(t, got[2].Sub)
	assert.Equal(t, "zm-dispatcher", got[3].From)
	assert.Equal(t, "fast", got[3].Speed)
	assert.Equal(t, 2+100+1000, got[0].Prompt())
}

func TestTriggers(t *testing.T) {
	for text, want := range map[string]string{
		"[zmina T-4, scheduled] run the report":       TriggerSchedule,
		"[zmina] remote mac/acme: \"hi\"":             TriggerRemote,
		"<task-notification>done</task-notification>": TriggerAgent,
		"/compact": TriggerOwner,
	} {
		got, _ := textTrigger(text)
		assert.Equal(t, want, got, text)
	}
}

func TestProjectDir(t *testing.T) {
	assert.Equal(t, filepath.Join("/ns", "projects", "-home-alex--zmina-worktrees-acme-T-1"),
		(&Registry{Namespace: "/ns"}).ProjectDir("/home/alex/.zmina/worktrees/acme-T-1"))
}
