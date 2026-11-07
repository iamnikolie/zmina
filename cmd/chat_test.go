package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/require"
)

func TestChatSendIsDurableIdempotentAndRoleScoped(t *testing.T) {
	uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	id := "12345678-1234-1234-1234-123456789abc"
	first, err := a.localChatSend("default", "dev", id, "hello")
	require.NoError(t, err)
	require.Len(t, first.Pending, 1)
	require.Equal(t, "queued", first.Pending[0].State)
	again, err := (&app{out: io.Discard, err: io.Discard}).localChatSend("default", "dev", id, "hello")
	require.NoError(t, err)
	require.Len(t, again.Pending, 1)
	_, err = a.localChatSend("default", "dev", id, "different")
	require.ErrorContains(t, err, "different text")
	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		require.Len(t, s.State.Role("dev").Inbox, 1)
		require.Len(t, s.State.Role("dev").ChatReceipts, 1)
		return nil
	}))
	other, err := a.localChatRead("ops", "dev")
	require.NoError(t, err)
	require.Empty(t, other.Pending)
	_, err = a.localChatSend("default", "dev", "../../anything", "hello")
	require.Error(t, err)
	_, err = a.localChatSend("default", "dev", id, "\x03")
	require.Error(t, err)
}

// A Claude role gets the owner's message whole, line breaks and his files after it, as one queued line the queue types
// at its prompt (his own turn); nothing goes through the crew's inbox.
func TestChatQueuesTheWholeMessageForAClaudeRole(t *testing.T) {
	uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	text := "Please inspect:\n\n```go\nfmt.Println(\"hello\")\n```\n"
	snapshot, err := a.localChatSend("default", "dev", "abcdefab-1234-1234-1234-123456789abc", text)
	require.NoError(t, err)
	require.Equal(t, text, snapshot.Pending[0].Text)
	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		rs := s.State.Role("dev")
		require.Equal(t, "[zmina chat abcdefab-1234-1234-1234-123456789abc] Please inspect:\n\n```go\nfmt.Println(\"hello\")\n```", rs.Inbox[0])
		require.Equal(t, rs.Inbox[0], rs.ChatReceipts[0].Line, "its receipt names the very line: the queue types it")
		entries, err := s.Inbox.List()
		require.NoError(t, err)
		require.Empty(t, entries)
		return nil
	}))
}

// A Codex bridge takes one line: a message with line breaks goes to the crew's inbox, and the line says whose words.
func TestChatPreservesMultilineMessageInInboxForCodex(t *testing.T) {
	home := uiHome(t)
	crewFile := filepath.Join(home, ".zmina", "crew.yaml")
	raw, err := os.ReadFile(crewFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(crewFile, []byte(strings.Replace(string(raw), "  dev:\n", "  dev:\n    runtime: codex\n", 1)), 0o600))
	a := &app{out: io.Discard, err: io.Discard}
	text := "Please inspect:\n```go\nfmt.Println(\"hello\")\n```"
	_, err = a.localChatSend("default", "dev", "abcdefab-1234-1234-1234-123456789abc", text)
	require.NoError(t, err)
	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		line := s.State.Role("dev").Inbox[0]
		require.NotContains(t, line, "\n")
		require.Contains(t, line, "the owner's own words")
		_, id, _ := strings.Cut(line, "zmina inbox show ")
		entry, err := s.Inbox.Get(id)
		require.NoError(t, err)
		require.Equal(t, text, entry.Body)
		return nil
	}))
}

func TestChatEndpointUsesExistingWriteGuard(t *testing.T) {
	uiHome(t)
	srv, _ := uiServer(t, true, "")
	body := []byte(`{"crew":"default","role":"dev","id":"abcdefab-1234-1234-1234-123456789abc","text":"hello"}`)
	post := func(header bool) *http.Response {
		req, err := http.NewRequest("POST", srv.URL+"/api/chat/send", bytes.NewReader(body))
		require.NoError(t, err)
		if header {
			req.Header.Set("X-Zmina", "1")
		}
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return response
	}
	denied := post(false)
	require.Equal(t, http.StatusForbidden, denied.StatusCode)
	_ = denied.Body.Close()
	ok := post(true)
	defer func() { _ = ok.Body.Close() }()
	require.Equal(t, http.StatusOK, ok.StatusCode)
	var snapshot chat.Snapshot
	require.NoError(t, json.NewDecoder(ok.Body).Decode(&snapshot))
	require.Len(t, snapshot.Pending, 1)
	blocked, err := srv.Client().Get(srv.URL + "/api/chat?crew=../../outside&role=dev")
	require.NoError(t, err)
	require.NotEqual(t, http.StatusOK, blocked.StatusCode)
	_ = blocked.Body.Close()
}

func TestChatRetryIdentitySurvivesMoreThan128AcceptedMessages(t *testing.T) {
	uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	c, err := a.crewApp("default")
	require.NoError(t, err)
	oldest := "abcdefab-1234-1234-1234-123456789abc"
	require.NoError(t, c.withState(func(s *shift.Shift) error {
		for i := 0; i < 140; i++ {
			id := fmt.Sprintf("%032x", i)
			if i == 0 {
				id = oldest
			}
			s.State.Role("dev").ChatReceipts = append(s.State.Role("dev").ChatReceipts, chat.Receipt{ID: id, Hash: chat.Hash("hello"), Text: "hello", Line: "old line", At: s.Now()})
		}
		return nil
	}))
	_, err = a.localChatSend("default", "dev", oldest, "hello")
	require.NoError(t, err)
	_, err = a.localChatSend("default", "dev", "ffffffff-1234-1234-1234-123456789abc", "new")
	require.NoError(t, err)
	_, err = a.localChatSend("default", "dev", oldest, "hello")
	require.NoError(t, err)
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		require.Len(t, s.State.Role("dev").Inbox, 1)
		require.Len(t, s.State.Role("dev").ChatReceipts, 141)
		return nil
	}))
}

func TestChatAcceptanceRequiresMatchingUserTranscript(t *testing.T) {
	home := uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	id := "abcdefab-1234-1234-1234-123456789abc"
	_, err := a.localChatSend("default", "dev", id, "hello")
	require.NoError(t, err)
	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.withState(func(s *shift.Shift) error {
		rs := s.State.Role("dev")
		rs.Inbox = nil
		rs.LastSession = "session"
		rs.LastRuntime = "claude"
		return nil
	}))
	before, err := a.localChatRead("default", "dev")
	require.NoError(t, err)
	require.Equal(t, "sent", before.Pending[0].State)
	dir := filepath.Join(home, ".zmina", "ns", "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	body := `{"type":"user","uuid":"one","message":{"role":"user","content":"` + chat.Prefix(id) + `hello"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(body), 0600))
	after, err := a.localChatReadSince("default", "dev", before.Cursor)
	require.NoError(t, err)
	require.Empty(t, after.Pending)
	require.Len(t, after.Messages, 1)
	require.Equal(t, "accepted", after.Messages[0].State)
	require.Equal(t, "web:"+id, after.Messages[0].ID)
	require.False(t, after.Reset)
}

// A message posted to the role's inbox socket shows in the transcript as Claude Code's meta line from a peer, with
// zmina's tag: it is the owner's once its ID is one of his receipts, and never the owner's without one.
func TestChatAcceptsAMessagePostedToTheInboxSocket(t *testing.T) {
	home := uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	id, forged := "abcdefab-1234-1234-1234-123456789abc", "cccccccc-1234-1234-1234-123456789abc"
	_, err := a.localChatSend("default", "dev", id, "line one\nline two")
	require.NoError(t, err)
	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.withState(func(s *shift.Shift) error {
		rs := s.State.Role("dev")
		rs.Inbox = nil
		rs.LastSession, rs.LastRuntime = "session", "claude"
		return nil
	}))
	posted := func(uuid, chatID string) string {
		content := "Another Claude session sent a message:\n[zmina:abcdefgh2345] " + chat.Prefix(chatID) +
			"line one line two — full owner message (preserve formatting): zmina inbox show in-0123456789ab\n\n" +
			"This came from another Claude session — not typed by your user."
		raw, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "isMeta": true,
			"origin": map[string]string{"kind": "peer", "from": "zmina"}, "message": map[string]string{"role": "user", "content": content}})
		return string(raw) + "\n"
	}
	dir := filepath.Join(home, ".zmina", "ns", "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	other := `{"type":"user","uuid":"meta","isMeta":true,"message":{"role":"user","content":"internal metadata"}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(posted("one", id)+other+posted("two", forged)), 0600))

	snapshot, err := a.localChatRead("default", "dev")
	require.NoError(t, err)
	require.Empty(t, snapshot.Pending, "the message shows in the conversation: it is no longer awaiting confirmation")
	require.Len(t, snapshot.Messages, 2, "other meta lines stay out")
	require.Equal(t, "web:"+id, snapshot.Messages[0].ID)
	require.Equal(t, "accepted", snapshot.Messages[0].State)
	require.Equal(t, "line one\nline two", snapshot.Messages[0].Text, "the owner's own text, from his receipt")
	require.Equal(t, "event", snapshot.Messages[1].Role, "an ID no browser sent is a session's message, not the owner's")
	require.Equal(t, "peer:"+forged, snapshot.Messages[1].ID)
}

func TestChatSnapshotSeparatesSessionInputFromBrowserOwner(t *testing.T) {
	home := uiHome(t)
	app := &app{out: io.Discard, err: io.Discard}
	crew, err := app.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, crew.withState(func(s *shift.Shift) error {
		s.State.Role("dev").LastSession = "session"
		s.State.Role("dev").LastRuntime = "claude"
		return nil
	}))
	dir := filepath.Join(home, ".zmina", "ns", "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	body := `{"type":"user","uuid":"session-input","message":{"role":"user","content":"dispatcher: inspect this task\nTechnical implementation details"}}
{"type":"user","uuid":"owner","message":{"role":"user","content":"[zmina chat abcdefab-1234-1234-1234-123456789abc] literal owner message"}}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(body), 0600))
	snapshot, err := app.localChatRead("default", "dev")
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, 2)
	require.Equal(t, "event", snapshot.Messages[0].Role)
	require.Equal(t, "session", snapshot.Messages[0].Event.Kind)
	require.Contains(t, snapshot.Messages[0].Event.Detail, "Technical implementation details")
	require.Equal(t, "user", snapshot.Messages[1].Role)
	require.Nil(t, snapshot.Messages[1].Event)
	require.Equal(t, "literal owner message", snapshot.Messages[1].Text)
}

// A message whose conversation changed before it showed there can be set aside; one still in play cannot.
func TestChatDismissesOnlyAnUnconfirmedMessage(t *testing.T) {
	uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	old, live := "aaaaaaaa-1234-1234-1234-123456789abc", "bbbbbbbb-1234-1234-1234-123456789abc"
	_, err := a.localChatSend("default", "dev", old, "from an earlier conversation")
	require.NoError(t, err)
	_, err = a.localChatSend("default", "dev", live, "still queued")
	require.NoError(t, err)

	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.withState(func(s *shift.Shift) error {
		rs := s.State.Role("dev")
		rs.Inbox = rs.Inbox[1:] // the first went, into a conversation since replaced
		rs.ChatReceipts[0].Session = "earlier"
		rs.LastSession, rs.LastRuntime = "session", "claude"
		return nil
	}))

	before, err := a.localChatRead("default", "dev")
	require.NoError(t, err)
	require.Len(t, before.Pending, 2)
	require.Equal(t, "unconfirmed", before.Pending[0].State)

	_, err = a.localChatAction("default", "dev", "dismiss:"+live)
	require.ErrorContains(t, err, "only a message whose delivery is unconfirmed")
	_, err = a.localChatAction("default", "dev", "dismiss:../x")
	require.ErrorContains(t, err, "invalid")

	after, err := a.localChatAction("default", "dev", "dismiss:"+old)
	require.NoError(t, err)
	require.Len(t, after.Pending, 1)
	require.Equal(t, "web:"+live, after.Pending[0].ID)

	// The receipt stays: the same ID sent again is recognised, not taken as new.
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		require.Len(t, s.State.Role("dev").ChatReceipts, 2)
		return nil
	}))
}
