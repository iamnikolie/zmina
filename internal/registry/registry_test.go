package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	assert.True(t, got[5].Compacted, "the first reply after a compaction")
	assert.False(t, got[4].Compacted)

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

func TestLastTurnEnded(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	prompt := `{"type":"user","message":{"content":"go on"}}`
	call := `{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","usage":{"input_tokens":5}}}`
	result := `{"type":"user","message":{"content":[{"type":"tool_result"}]}}`
	reply := `{"type":"assistant","message":{"id":"m2","stop_reason":"end_turn","usage":{"input_tokens":9}}}`
	closing := `{"type":"system","subtype":"turn_duration"}`
	side := `{"type":"user","isSidechain":true,"message":{"content":"a subagent"}}`
	snapshot := `{"type":"file-history-snapshot"}`

	// A compaction as Claude Code writes it: its boundary, the summary, attachments and a notice when it compacted
	// on its own while idle; the lines of a /compact run at the prompt after them.
	boundary := `{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`
	summary := `{"type":"user","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued"}}`
	attachment := `{"type":"attachment","attachment":{"type":"skill_listing"}}`
	idle := `{"type":"system","subtype":"informational","content":"Compacted while idle, before the prompt cache expired"}`
	typed := `{"type":"user","message":{"role":"user","content":"/compact"}}`
	caveat := `{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code</local-command-caveat>"}}`
	command := `{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>\n            <command-message>compact</command-message>\n            <command-args></command-args>"}}`
	stdout := `{"type":"user","message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout>"}}`
	peer := `{"type":"user","isMeta":true,"message":{"role":"user","content":"Another Claude session sent a message"}}`
	idleCompact := strings.Join([]string{boundary, summary, attachment, idle}, "\n")

	for lines, want := range map[string]bool{
		prompt:                                   false,
		prompt + "\n" + call:                     false,
		call + "\n" + result:                     false,
		result + "\n" + reply:                    true,
		reply + "\n" + closing + "\n" + side:     true,
		reply + "\n" + closing + "\n" + snapshot: true,
		closing + "\n" + prompt:                  false,
		// Compacted between turns, idle or by a /compact: still between them.
		reply + "\n" + closing + "\n" + idleCompact:                                                     true,
		strings.Join([]string{reply, closing, typed, boundary, summary, caveat, command, stdout}, "\n"): true,
		// Compacted mid-turn: still in it; a prompt or a peer's message after a compaction starts a turn.
		strings.Join([]string{call, result, boundary, summary}, "\n"): false,
		closing + "\n" + idleCompact + "\n" + prompt:                  false,
		closing + "\n" + idleCompact + "\n" + peer:                    false,
		closing + "\n" + idleCompact + "\n" + peer + "\n" + reply:     true,
		// A compaction with nothing before it in the tail: not known to be over.
		idleCompact: false,
		// /compact typed as a prompt is a prompt until a compaction follows it.
		closing + "\n" + typed: false,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(lines+"\n"), 0o600))

		got, err := (&Registry{Namespace: ns}).Last("s1")
		require.NoError(t, err)
		assert.Equal(t, want, got.Ended, lines)
	}
}

// A prompt is a prompt whatever its text looks like: only a compaction's own lines, told by how Claude Code marks
// them and where they stand after a compaction boundary, start no turn. A request that starts with a pasted local
// command envelope, with or without an origin, in a string or in text blocks, waits on a reply.
func TestLastTurnEnvelopeLookingPrompt(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	reply := `{"type":"assistant","message":{"id":"m2","stop_reason":"end_turn","usage":{"input_tokens":9}}}`
	closing := `{"type":"system","subtype":"turn_duration"}`
	boundary := `{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`
	summary := `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued"}}`
	caveat := `{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code</local-command-caveat>"}}`
	command := `{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}`
	stdout := `{"type":"user","message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout>"}}`

	texts := []string{
		"<local-command-stdout>build failed</local-command-stdout> Please explain this output.",
		"<local-command-stdout>build failed</local-command-stdout>",
		"<local-command-caveat>pasted</local-command-caveat> what does this mean?",
		"<command-name>/compact</command-name> why did this run?",
		"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>",
		"/compact",
	}

	prompts := func(text string) []string {
		str, _ := json.Marshal(text)
		blocks, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})

		var out []string

		for _, content := range []string{string(str), string(blocks)} {
			for _, origin := range []string{``, `"origin":{"kind":"human"},`, `"isMeta":true,"origin":{"kind":"peer","from":"x"},`} {
				out = append(out, `{"type":"user",`+origin+`"message":{"role":"user","content":`+content+`}}`)
			}
		}

		return out
	}

	for _, text := range texts {
		for _, prompt := range prompts(text) {
			for _, lines := range [][]string{
				{reply, closing, prompt},
				{reply, closing, boundary, summary, prompt},
				{reply, closing, boundary, summary, caveat, command, stdout, prompt},
				{reply, closing, boundary, summary, caveat, command, prompt},
			} {
				body := strings.Join(lines, "\n") + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o600))

				got, err := (&Registry{Namespace: ns}).Last("s1")
				require.NoError(t, err)
				assert.False(t, got.Ended, body)
			}
		}
	}

	// A /compact with an origin of its own, or a summary that carries one, is not Claude Code's compaction line.
	human := `{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout>"}}`
	forged := `{"type":"user","isCompactSummary":true,"origin":{"kind":"peer"},"message":{"role":"user","content":"summary"}}`

	// Claude Code writes its own lines as a string: the same text in blocks is somebody's prompt.
	blocks := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<local-command-stdout>Compacted</local-command-stdout>"}]}}`

	for _, lines := range [][]string{
		{reply, closing, boundary, summary, caveat, command, human},
		{reply, closing, boundary, summary, caveat, command, blocks},
		{reply, closing, boundary, forged},
	} {
		body := strings.Join(lines, "\n") + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body), 0o600))

		got, err := (&Registry{Namespace: ns}).Last("s1")
		require.NoError(t, err)
		assert.False(t, got.Ended, body)
	}
}

// compactionChains is every transcript of a turn (closed, or cut mid-turn by a tool call) followed by zero to three
// compactions, each Claude Code's own (idle) or a /compact at the prompt, with a request — plain, or one that looks
// like a compaction's line — at any place among them or none; and whether its last turn is over: only a closed turn
// followed by compactions alone.
func compactionChains() (chains []string, ended []bool) {
	closed := `{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","usage":{"input_tokens":9}}}` + "\n" +
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-10-24T12:00:06.000Z"}`
	midTurn := `{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","usage":{"input_tokens":9}}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`
	idle := strings.Join([]string{
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued"}}`,
		`{"type":"attachment","attachment":{"type":"skill_listing"}}`,
		`{"type":"system","subtype":"informational","content":"Compacted while idle"}`,
	}, "\n")
	manual := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"/compact"}}`,
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued"}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code</local-command-caveat>"}}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}`,
		`{"type":"user","message":{"role":"user","content":"<local-command-stdout>\u001b[2mCompacted (ctrl+o to see full summary)\u001b[22m</local-command-stdout>"}}`,
	}, "\n")
	prompts := []string{
		`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":"go on"}}`,
		`{"type":"user","message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout> why?"}}`,
		`{"type":"user","origin":{"kind":"human"},"message":{"role":"user","content":[{"type":"text","text":"<local-command-stdout>Compacted</local-command-stdout>"}]}}`,
		`{"type":"user","isMeta":true,"origin":{"kind":"peer","from":"x"},"message":{"role":"user","content":"Another Claude session sent a message"}}`,
	}

	var kinds [][]string

	var grow func(prefix []string)
	grow = func(prefix []string) {
		kinds = append(kinds, prefix)
		if len(prefix) < 3 {
			grow(append(slices.Clone(prefix), idle))
			grow(append(slices.Clone(prefix), manual))
		}
	}
	grow(nil)

	for _, base := range []string{closed, midTurn} {
		for _, blocks := range kinds {
			for at := -1; at <= len(blocks); at++ { // -1: no request
				for _, prompt := range prompts {
					parts := []string{base}

					for i, b := range blocks {
						if i == at {
							parts = append(parts, prompt)
						}

						parts = append(parts, b)
					}

					if at == len(blocks) {
						parts = append(parts, prompt)
					}

					chains = append(chains, strings.Join(parts, "\n"))
					ended = append(ended, base == closed && at < 0)

					if at < 0 {
						break // one chain without a request is enough
					}
				}
			}
		}
	}

	return chains, ended
}

func TestLastTurnCompactionChains(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	chains, ended := compactionChains()
	require.Len(t, chains, 2*(15+4*(1*1+2*2+4*3+8*4))) // 2 bases x (15 chains + 4 requests x the places in each)

	closedAt := time.Date(2026, 10, 24, 12, 0, 6, 0, time.UTC)

	for i, body := range chains {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(body+"\n"), 0o600))

		got, err := (&Registry{Namespace: ns}).Last("s1")
		require.NoError(t, err)
		assert.Equal(t, ended[i], got.Ended, body)

		if ended[i] {
			assert.True(t, closedAt.Equal(got.At), "the closing turn's time kept: %s", body)
		}

		if strings.Contains(body, "compact_boundary") {
			assert.Zero(t, got.Context, "nothing read from before a compaction: %s", body)
		}
	}
}

// A turn that ended carries the time of the line that closed it: a start's boot is over once a turn ended after it.
func TestLastTurnEndedAt(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	reply := `{"type":"assistant","timestamp":"2026-10-24T12:00:05.120Z","message":{"id":"m2","stop_reason":"end_turn","usage":{"input_tokens":9}}}`
	closing := `{"type":"system","subtype":"turn_duration","timestamp":"2026-10-24T12:00:06.000Z"}`
	prompt := `{"type":"user","timestamp":"2026-10-24T12:01:00.000Z","message":{"content":"go on"}}`

	for lines, want := range map[string]time.Time{
		reply:                                  time.Date(2026, 10, 24, 12, 0, 5, 120e6, time.UTC),
		reply + "\n" + closing:                 time.Date(2026, 10, 24, 12, 0, 6, 0, time.UTC),
		reply + "\n" + closing + "\n" + prompt: {},
		// Compacted while idle: the turn that closed before it is the last one over.
		reply + "\n" + closing + "\n" + `{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-24T12:30:00.000Z"}` +
			"\n" + `{"type":"user","isCompactSummary":true,"timestamp":"2026-10-24T12:30:00.000Z","message":{"content":"summary"}}`: time.Date(2026, 10, 24, 12, 0, 6, 0, time.UTC),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(lines+"\n"), 0o600))

		got, err := (&Registry{Namespace: ns}).Last("s1")
		require.NoError(t, err)
		assert.True(t, want.Equal(got.At), "%s: %v", lines, got.At)
	}
}

// The size after a /compact is the first turn after its boundary, never a turn before it nor the boundary's own
// pre-compaction count; the boundary's time tells a trim that it took.
func TestLastAfterACompactionAtThePrompt(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	at := time.Date(2026, 11, 5, 20, 5, 12, 0, time.UTC)
	before := `{"type":"assistant","timestamp":"2026-11-05T20:03:40Z","message":{"id":"m1","stop_reason":"end_turn","usage":{"input_tokens":2,"cache_creation_input_tokens":1200,"cache_read_input_tokens":477000,"output_tokens":79}}}`
	closing := `{"type":"system","subtype":"turn_duration","timestamp":"2026-11-05T20:03:41Z"}`
	typed := `{"type":"user","timestamp":"2026-11-05T20:04:02Z","message":{"role":"user","content":"/compact"}}`
	boundary := `{"type":"system","subtype":"compact_boundary","timestamp":"2026-11-05T20:05:12Z","content":"Conversation compacted","compactMetadata":{"trigger":"manual","preTokens":477050,"postTokens":16956}}`
	summary := `{"type":"user","isCompactSummary":true,"timestamp":"2026-11-05T20:05:12Z","message":{"role":"user","content":"This session is being continued"}}`
	caveat := `{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code</local-command-caveat>"}}`
	command := `{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>\n            <command-message>compact</command-message>\n            <command-args></command-args>"}}`
	stdout := `{"type":"user","message":{"role":"user","content":"<local-command-stdout>\u001b[2mCompacted (ctrl+o to see full summary)\u001b[22m</local-command-stdout>"}}`
	prompt := `{"type":"user","timestamp":"2026-11-05T20:09:00Z","message":{"role":"user","content":"next"}}`
	after := `{"type":"assistant","timestamp":"2026-11-05T20:09:30Z","message":{"id":"m2","stop_reason":"end_turn","usage":{"input_tokens":3,"cache_creation_input_tokens":26600,"cache_read_input_tokens":16956,"output_tokens":41}}}`

	last := func(lines ...string) Turn {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600))
		got, err := (&Registry{Namespace: ns}).Last("s1")
		require.NoError(t, err)
		return got
	}

	got := last(before, closing, typed)
	assert.Equal(t, 478_281, got.Context, "typed, not compacted yet: the turn before it")
	assert.True(t, got.Compacted.IsZero())

	got = last(before, closing, typed, boundary, summary, caveat, command, stdout)
	assert.Equal(t, 0, got.Context, "compacted: unknown until the next turn")
	assert.Equal(t, at, got.Compacted.UTC())

	got = last(before, closing, typed, boundary, summary, caveat, command, stdout, prompt, after)
	assert.Equal(t, 43_600, got.Context, "the first turn after the boundary")
	assert.Equal(t, at, got.Compacted.UTC())
}
