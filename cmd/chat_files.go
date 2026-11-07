package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) localChatUpload(key, role string, file chat.Upload) (chat.Attachment, error) {
	c, err := a.crewApp(key)
	if err != nil {
		return chat.Attachment{}, err
	}
	var meta chat.Attachment
	err = c.withState(func(s *shift.Shift) error {
		if _, err := s.Role(role); err != nil {
			return err
		}
		var err error
		meta, err = (chat.FileStore{Dir: filepath.Join(s.Crew.StateDir(), "chat-files"), Role: role}).Put(file)
		return err
	})
	return meta, err
}

func (a *app) localChatFile(key, role, id string) (chat.Upload, error) {
	c, err := a.crewApp(key)
	if err != nil {
		return chat.Upload{}, err
	}
	var file chat.Upload
	err = c.readState(func(s *shift.Shift) error {
		if _, err := s.Role(role); err != nil {
			return err
		}
		meta, path, err := (chat.FileStore{Dir: filepath.Join(s.Crew.StateDir(), "chat-files"), Role: role}).Get(id)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) > chat.MaxFileBytes {
			return errors.New("attachment is too large")
		}
		file = chat.Upload{ID: id, Name: meta.Name, Data: data}
		return nil
	})
	return file, err
}

func (a *app) uiChatFiles(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat/upload", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Node, Crew, Role string
			Upload           chat.Upload
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body.Upload.Data) > chat.MaxFileBytes {
			http.Error(w, "file exceeds 8 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		snapshot, err := a.chatCall(ctx, body.Node, "chat/upload", mesh.TerminalRequest{Crew: body.Crew, Role: body.Role, Upload: &body.Upload})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, snapshot)
	})
	mux.HandleFunc("GET /api/chat/file", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		snapshot, err := a.chatCall(ctx, q.Get("node"), "chat/file", mesh.TerminalRequest{Crew: q.Get("crew"), Role: q.Get("role"), FileID: q.Get("id")})
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if snapshot.File == nil {
			http.Error(w, "attachment unavailable on this node", http.StatusBadGateway)
			return
		}
		// ?inline=1: a thumbnail or the viewer — inline only when its first bytes are an image or a pdf.
		serveFile(w, snapshot.File, q.Get("inline") != "")
	})
}
