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

func TestChatPreservesMultilineMessageInInbox(t *testing.T) {
	uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	text := "Please inspect:\n```go\nfmt.Println(\"hello\")\n```"
	snapshot, err := a.localChatSend("default", "dev", "abcdefab-1234-1234-1234-123456789abc", text)
	require.NoError(t, err)
	require.Equal(t, text, snapshot.Pending[0].Text)
	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		line := s.State.Role("dev").Inbox[0]
		require.NotContains(t, line, "\n")
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
