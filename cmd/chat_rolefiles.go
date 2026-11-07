package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
)

// A role's files in Chat (design D-16 r1): a path a role names in its message shows as a preview or a chip when the
// file is one zmina may serve — an image (png, jpeg, gif, webp) or a pdf, by its first bytes and its extension, at most
// chat.MaxFileBytes, under the role's dir, a worktree of one of its tasks, or the system temp dir (where sessions keep
// their scratchpads) — or a file the role's session handed the owner with SendUserFile, wherever it is: Claude Code
// delivered those very bytes to the owner already. Any other path stays text: the page never learns whether it exists.

// rolePathsMax bounds one lookup; rolePathMax one path.
const (
	rolePathsMax = 64
	rolePathMax  = 1024
)

// fileKinds are the types a role's file may be served as, by extension.
var fileKinds = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".pdf": "application/pdf"}

// sniffKind is the type a file's first bytes say, of the ones zmina serves inline; "" for any other.
func sniffKind(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp"
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return "application/pdf"
	}

	return ""
}

// roleRoots are the directories a role's files may be served from, each with its symlinks resolved.
func roleRoots(s *shift.Shift, role string) ([]string, error) {
	r, err := s.Role(role)
	if err != nil {
		return nil, err
	}

	base, _, _ := shift.SplitInstance(role)
	dirs := []string{r.Dir, os.TempDir(), "/tmp"}

	tasks, _, _ := taskStore(s.Crew).Scan()
	for _, t := range tasks {
		if t.Worktree == "" {
			continue
		}

		if tb, _, _ := shift.SplitInstance(t.Role); tb == base {
			dirs = append(dirs, t.Worktree)
		}
	}

	var roots []string

	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}

		if real, err := filepath.EvalSymlinks(d); err == nil {
			roots = append(roots, real)
		}
	}

	return roots, nil
}

// roleSent are the paths the role's conversation (its current one, else its last) handed the owner with SendUserFile.
func roleSent(s *shift.Shift, role string) map[string]bool {
	r, err := s.Role(role)
	if err != nil || r.IsCodex() {
		return nil
	}

	session := ""

	for _, v := range s.Views() {
		if v.Role == role {
			session = v.SessionID

			break
		}
	}

	if rs := s.State.Role(role); session == "" && (rs.LastRuntime == "" || rs.LastRuntime == "claude") {
		session = rs.LastSession
	}

	paths, err := (&registry.Registry{Namespace: s.Crew.Namespace}).SentFiles(session)
	if err != nil || len(paths) == 0 {
		return nil
	}

	sent := map[string]bool{}
	for _, p := range paths {
		sent[filepath.Clean(p)] = true
	}

	return sent
}

// within is true when path is root or under it.
func within(path, root string) bool {
	if root == string(filepath.Separator) {
		return true
	}

	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// roleFile opens a path a role named when zmina may serve it: absolute (or ~/…), a regular file under one of roots
// after its symlinks are resolved or one the role sent (sent, by its path as sent), a kind of fileKinds by its
// extension and its first bytes, at most MaxFileBytes. The caller closes the file.
func roleFile(path string, roots []string, sent map[string]bool) (*os.File, chat.PathInfo, error) {
	refuse := errors.New("not a file this conversation shows")

	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, chat.PathInfo{}, refuse
		}

		path = filepath.Join(home, path[2:])
	}

	if len(path) > rolePathMax || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return nil, chat.PathInfo{}, refuse
	}

	kind := fileKinds[strings.ToLower(filepath.Ext(path))]
	if kind == "" {
		return nil, chat.PathInfo{}, refuse
	}

	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return nil, chat.PathInfo{}, refuse
	}

	inside := sent[filepath.Clean(path)]

	for _, root := range roots {
		if within(real, root) {
			inside = true

			break
		}
	}

	if !inside {
		return nil, chat.PathInfo{}, refuse
	}

	// A regular file before it is opened: open(2) on a FIFO named a.png would wait for a writer forever. Opened
	// without blocking and without following a last link, then the same file again, so a swap since is refused.
	before, err := os.Lstat(real)
	if err != nil || !before.Mode().IsRegular() || before.Size() > chat.MaxFileBytes {
		return nil, chat.PathInfo{}, refuse
	}

	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, chat.PathInfo{}, refuse
	}

	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > chat.MaxFileBytes || !os.SameFile(before, st) {
		_ = f.Close()

		return nil, chat.PathInfo{}, refuse
	}

	// A directory above swapped for a link between the resolve and the open leads somewhere else: resolve again.
	if again, err := filepath.EvalSymlinks(real); err != nil || again != real {
		_ = f.Close()

		return nil, chat.PathInfo{}, refuse
	}

	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)

	if sniffKind(head[:n]) != kind {
		_ = f.Close()

		return nil, chat.PathInfo{}, refuse
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()

		return nil, chat.PathInfo{}, refuse
	}

	return f, chat.PathInfo{Path: path, Name: filepath.Base(path), Size: int(st.Size()), Type: kind}, nil
}

// localChatPaths says which of the paths a role named are files Chat shows (newline-separated in, one entry each out).
func (a *app) localChatPaths(key, role, list string) (chat.Snapshot, error) {
	paths := strings.Split(list, "\n")
	if len(paths) > rolePathsMax {
		return chat.Snapshot{}, fmt.Errorf("at most %d paths", rolePathsMax)
	}

	c, err := a.crewApp(key)
	if err != nil {
		return chat.Snapshot{}, err
	}

	out := []chat.PathInfo{}

	err = c.readState(func(s *shift.Shift) error {
		roots, err := roleRoots(s, role)
		if err != nil {
			return err
		}

		sent := roleSent(s, role)

		for _, p := range paths {
			if p == "" {
				continue
			}

			f, info, err := roleFile(p, roots, sent)
			if err != nil {
				continue
			}

			_ = f.Close()
			out = append(out, info)
		}

		return nil
	})

	return chat.Snapshot{Paths: out}, err
}

// localChatRoleFile reads one of a role's files Chat shows.
func (a *app) localChatRoleFile(key, role, path string) (chat.Upload, error) {
	c, err := a.crewApp(key)
	if err != nil {
		return chat.Upload{}, err
	}

	var file chat.Upload

	err = c.readState(func(s *shift.Shift) error {
		roots, err := roleRoots(s, role)
		if err != nil {
			return err
		}

		f, info, err := roleFile(path, roots, roleSent(s, role))
		if err != nil {
			return err
		}

		defer func() { _ = f.Close() }()

		data, err := io.ReadAll(io.LimitReader(f, chat.MaxFileBytes+1))
		if err != nil {
			return err
		}

		if len(data) > chat.MaxFileBytes {
			return errors.New("not a file this conversation shows")
		}

		file = chat.Upload{Name: info.Name, Data: data}

		return nil
	})

	return file, err
}

// serveFile writes a file Chat shows: inline when its first bytes are an image or a pdf (never HTML, SVG or anything a
// browser would run), else as a download.
func serveFile(w http.ResponseWriter, file *chat.Upload, inline bool) {
	kind := sniffKind(file.Data[:min(len(file.Data), 16)])

	w.Header().Set("X-Content-Type-Options", "nosniff")

	if inline && kind != "" {
		w.Header().Set("Content-Type", kind)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": file.Name}))
		// The bytes of an id or a path may change (a role writes its screenshot again): kept briefly, privately.
		w.Header().Set("Cache-Control", "private, max-age=60")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Name}))
		w.Header().Set("Cache-Control", "private, no-store")
	}

	w.Header().Set("Content-Length", fmt.Sprint(len(file.Data)))
	_, _ = w.Write(file.Data)
}

func (a *app) uiChatRoleFiles(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat/paths", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Node, Crew, Role string
			Paths            []string
		}

		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		if len(body.Paths) > rolePathsMax {
			http.Error(w, fmt.Sprintf("at most %d paths", rolePathsMax), http.StatusBadRequest)

			return
		}

		for _, p := range body.Paths {
			if p == "" || len(p) > rolePathMax || strings.ContainsAny(p, "\n\x00") {
				http.Error(w, "a path is one line, 1 to 1024 bytes", http.StatusBadRequest)

				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		snapshot, err := a.chatCall(ctx, body.Node, "chat/paths", mesh.TerminalRequest{Crew: body.Crew, Role: body.Role,
			Text: strings.Join(body.Paths, "\n")})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)

			return
		}

		paths := snapshot.Paths
		if paths == nil {
			paths = []chat.PathInfo{}
		}

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"paths": paths})
	})
	mux.HandleFunc("GET /api/chat/rolefile", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		snapshot, err := a.chatCall(ctx, q.Get("node"), "chat/rolefile", mesh.TerminalRequest{Crew: q.Get("crew"), Role: q.Get("role"),
			Text: q.Get("path")})
		if err != nil || snapshot.File == nil {
			http.Error(w, "not a file this conversation shows", http.StatusNotFound)

			return
		}

		serveFile(w, snapshot.File, q.Get("download") == "")
	})
}
