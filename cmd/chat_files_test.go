package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/require"
)

func TestAttachmentOnlySendBindsFilesToRoleAndRetryPayload(t *testing.T) {
	uiHome(t)
	a := &app{out: io.Discard, err: io.Discard}
	upload := chat.Upload{ID: "abcdefab-1234-1234-1234-123456789abc", Name: "example.txt", Data: []byte("Original file\nПривет")}
	meta, err := a.localChatUpload("default", "dev", upload)
	require.NoError(t, err)
	id := "ffffffff-1234-1234-1234-123456789abc"
	first, err := a.localChatSendFiles("default", "dev", id, "", []string{meta.ID})
	require.NoError(t, err)
	require.Len(t, first.Pending, 1)
	require.Equal(t, []chat.Attachment{meta}, first.Pending[0].Attachments)
	again, err := a.localChatSendFiles("default", "dev", id, "", []string{meta.ID})
	require.NoError(t, err)
	require.Len(t, again.Pending, 1)
	_, err = a.localChatSend("default", "dev", id, "different")
	require.ErrorContains(t, err, "different text or attachments")
	_, err = a.localChatSendFiles("ops", "dev", id, "", []string{meta.ID})
	require.Error(t, err)
	_, err = a.localChatSendFiles("default", "dispatcher", id, "", []string{meta.ID})
	require.Error(t, err)
	c, err := a.crewApp("default")
	require.NoError(t, err)
	require.NoError(t, c.readState(func(s *shift.Shift) error {
		rs := s.State.Role("dev")
		require.Len(t, rs.Inbox, 1)
		// His files follow his (here no) words in the very line typed at the role's prompt.
		line := rs.Inbox[0]
		require.True(t, strings.HasPrefix(line, chat.Prefix(id)+"Attached files from the owner"), line)
		require.Contains(t, line, "chat-files")
		require.Contains(t, line, "example.txt")
		return nil
	}))
	file, err := a.localChatFile("default", "dev", meta.ID)
	require.NoError(t, err)
	require.Equal(t, upload, file)
}

func TestAttachmentEndpointsRequireWriteGuardAndDownloadSafely(t *testing.T) {
	uiHome(t)
	server, _ := uiServer(t, true, "")
	file := chat.Upload{ID: "abcdefab-1234-1234-1234-123456789abc", Name: "report.html", Data: []byte("<script>unsafe()</script>")}
	payload, _ := json.Marshal(map[string]any{"crew": "default", "role": "dev", "upload": file})
	post := func(guard bool) *http.Response {
		request, err := http.NewRequest("POST", server.URL+"/api/chat/upload", bytes.NewReader(payload))
		require.NoError(t, err)
		if guard {
			request.Header.Set("X-Zmina", "1")
		}
		result, err := server.Client().Do(request)
		require.NoError(t, err)
		return result
	}
	denied := post(false)
	require.Equal(t, http.StatusForbidden, denied.StatusCode)
	_ = denied.Body.Close()
	ok := post(true)
	require.Equal(t, http.StatusOK, ok.StatusCode)
	_ = ok.Body.Close()
	result, err := server.Client().Get(server.URL + "/api/chat/file?crew=default&role=dev&id=" + file.ID)
	require.NoError(t, err)
	defer func() { _ = result.Body.Close() }()
	require.Equal(t, "application/octet-stream", result.Header.Get("Content-Type"))
	require.Contains(t, result.Header.Get("Content-Disposition"), "attachment")
	require.Equal(t, "nosniff", result.Header.Get("X-Content-Type-Options"))
	body, err := io.ReadAll(result.Body)
	require.NoError(t, err)
	require.Equal(t, file.Data, body)
}
