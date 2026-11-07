package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/registry"
)

var (
	pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	pdfBytes = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
)

// A path a role names is served only when it is an image or a pdf by its extension and its first bytes, a regular
// file of at most 8 MiB under one of the role's roots after symlinks; any other path is refused the same way.
func TestRoleFileServesOnlyItsRoots(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	roots := []string{mustReal(t, root)}
	write := func(dir, name string, data []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, data, 0o600))

		return p
	}

	ok := write(root, "shots/a.png", pngBytes)
	f, info, err := roleFile(ok, roots, nil)
	require.NoError(t, err)
	_ = f.Close()
	assert.Equal(t, chat.PathInfo{Path: ok, Name: "a.png", Size: len(pngBytes), Type: "image/png"}, info)

	doc := write(root, "report.PDF", pdfBytes)
	f, info, err = roleFile(doc, roots, nil)
	require.NoError(t, err)
	_ = f.Close()
	assert.Equal(t, "application/pdf", info.Type)

	link := filepath.Join(root, "escape.png")
	require.NoError(t, os.Symlink(write(outside, "secret.png", pngBytes), link))
	fifo := filepath.Join(root, "pipe.png")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	big := write(root, "big.png", append(append([]byte{}, pngBytes...), make([]byte, chat.MaxFileBytes)...))

	for name, p := range map[string]string{
		"outside the roots":         write(outside, "b.png", pngBytes),
		"a symlink out of the root": link,
		"html named .png":           write(root, "page.png", []byte("<html><script>x()</script>")),
		"a png named .pdf":          write(root, "fake.pdf", pngBytes),
		"svg":                       write(root, "pic.svg", []byte("<svg onload=x()></svg>")),
		"over 8 MiB":                big,
		"a fifo named .png":         fifo,
		"relative":                  "shots/a.png",
		"a directory":               filepath.Join(root, "shots"),
		"gone":                      filepath.Join(root, "none.png"),
		"dot-dot out":               filepath.Join(root, "..", filepath.Base(outside), "b.png"),
	} {
		_, _, err := roleFile(p, roots, nil)
		require.Error(t, err, name)
	}
}

// A file the role handed the owner with SendUserFile is served wherever it is, by the path it sent, while it is that
// very file: no link anywhere on the path, the size sent, not written since the send; every other check as before.
func TestRoleFileServesWhatTheRoleSent(t *testing.T) {
	root, outside, elsewhere := t.TempDir(), mustReal(t, t.TempDir()), mustReal(t, t.TempDir())
	roots := []string{mustReal(t, root)}
	write := func(p string, data []byte) string {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, data, 0o600))

		return p
	}
	sentAt := time.Now().Add(time.Minute)
	send := func(paths ...string) map[string]registry.SentFile {
		out := map[string]registry.SentFile{}
		for _, p := range paths {
			st, err := os.Lstat(p)
			size := int64(len(pdfBytes))
			if err == nil && st.Mode().IsRegular() {
				size = st.Size()
			}
			out[p] = registry.SentFile{Path: p, Size: size, At: sentAt}
		}

		return out
	}

	doc := write(filepath.Join(outside, "out", "report.pdf"), pdfBytes)
	f, info, err := roleFile(doc, roots, send(doc))
	require.NoError(t, err)
	_ = f.Close()
	assert.Equal(t, "report.pdf", info.Name)

	private := write(filepath.Join(elsewhere, "private.pdf"), pdfBytes)
	last := filepath.Join(outside, "swapped.pdf")
	require.NoError(t, os.Symlink(private, last))
	write(filepath.Join(elsewhere, "dir", "report.pdf"), pdfBytes)
	parent := filepath.Join(outside, "linked")
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, "dir"), parent))
	fake := write(filepath.Join(outside, "fake.pdf"), []byte("<html>"))
	rewritten := write(filepath.Join(outside, "rewritten.pdf"), pdfBytes)
	// A hard link swapped in at a sent path to another file of that size, its mtime from before the send.
	hardlinked := filepath.Join(outside, "hardlinked.pdf")
	require.NoError(t, os.Link(write(filepath.Join(elsewhere, "secret.pdf"), pdfBytes), hardlinked))
	// Written after the send with its mtime set back: the inode's change time says so.
	backdatedAt := time.Now()
	time.Sleep(20 * time.Millisecond)
	backdated := write(filepath.Join(outside, "backdated.pdf"), pdfBytes)
	require.NoError(t, os.Chtimes(backdated, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	resized := write(filepath.Join(outside, "resized.pdf"), pdfBytes)

	for name, c := range map[string]struct {
		path string
		sent map[string]registry.SentFile
	}{
		"sent, but not a pdf by its bytes": {fake, send(fake)},
		"next to a sent file, not sent":    {write(filepath.Join(outside, "other.pdf"), pdfBytes), send(doc)},
		"outside the roots and not sent":   {doc, nil},
		"the sent file is now a link":      {last, send(last)},
		"a directory above is now a link":  {filepath.Join(parent, "report.pdf"), send(filepath.Join(parent, "report.pdf"))},
		"written since the send":           {rewritten, map[string]registry.SentFile{rewritten: {Path: rewritten, Size: int64(len(pdfBytes)), At: time.Now().Add(-time.Hour)}}},
		"another size than sent":           {resized, map[string]registry.SentFile{resized: {Path: resized, Size: 1, At: sentAt}}},
		"sent with no size recorded":       {doc, map[string]registry.SentFile{doc: {Path: doc, At: sentAt}}},
		"a hard link swapped in":           {hardlinked, send(hardlinked)},
		"written since, mtime set back":    {backdated, map[string]registry.SentFile{backdated: {Path: backdated, Size: int64(len(pdfBytes)), At: backdatedAt}}},
	} {
		_, _, err := roleFile(c.path, roots, c.sent)
		require.Error(t, err, name)
	}
}

func mustReal(t *testing.T, p string) string {
	t.Helper()

	real, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)

	return real
}

// Chat asks which paths a role named it shows, and opens them inline; its own uploads open inline only as an image
// or a pdf.
func TestChatRoleFilesOverHTTP(t *testing.T) {
	home := uiHome(t)
	srv, _ := uiServer(t, true, "")
	shot := filepath.Join(home, "work", "shot.png")
	require.NoError(t, os.MkdirAll(filepath.Dir(shot), 0o700))
	require.NoError(t, os.WriteFile(shot, pngBytes, 0o600))
	fake := filepath.Join(home, "work", "fake.png")
	require.NoError(t, os.WriteFile(fake, []byte("<html>"), 0o600))

	post := func(paths []string, guard bool) *http.Response {
		body, _ := json.Marshal(map[string]any{"crew": "default", "role": "dev", "paths": paths})
		req, err := http.NewRequest("POST", srv.URL+"/api/chat/paths", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if guard {
			req.Header.Set("X-Zmina", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		return resp
	}

	resp := post([]string{shot}, false)
	_ = resp.Body.Close()
	assert.NotEqual(t, http.StatusOK, resp.StatusCode, "a POST without the page's header is refused")

	resp = post([]string{shot, fake, "/etc/hosts", filepath.Join(home, "none.png")}, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Paths []chat.PathInfo `json:"paths"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Paths, 1, "only the real image")
	assert.Equal(t, "shot.png", out.Paths[0].Name)

	get := func(path, extra string) *http.Response {
		r, err := http.Get(srv.URL + "/api/chat/rolefile?" + url.Values{"crew": {"default"}, "role": {"dev"}, "path": {path}}.Encode() + extra)
		require.NoError(t, err)

		return r
	}

	r := get(shot, "")
	data, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	require.Equal(t, http.StatusOK, r.StatusCode)
	assert.Equal(t, "image/png", r.Header.Get("Content-Type"))
	assert.True(t, strings.HasPrefix(r.Header.Get("Content-Disposition"), "inline"))
	assert.Equal(t, "nosniff", r.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, pngBytes, data)

	r = get(shot, "&download=1")
	_ = r.Body.Close()
	assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))

	for _, p := range []string{fake, "/etc/hosts"} {
		r = get(p, "")
		_ = r.Body.Close()
		assert.Equal(t, http.StatusNotFound, r.StatusCode, p)
	}

	// The owner's own upload: inline only as an image.
	a := &app{out: io.Discard, err: io.Discard}
	for name, data := range map[string][]byte{"pic.png": pngBytes, "page.html": []byte("<script>x()</script>")} {
		meta, err := a.localChatUpload("default", "dev", chat.Upload{ID: "abcdefab-1234-1234-1234-12345678900" + map[string]string{"pic.png": "1", "page.html": "2"}[name], Name: name, Data: data})
		require.NoError(t, err)
		r, err := http.Get(srv.URL + "/api/chat/file?" + url.Values{"crew": {"default"}, "role": {"dev"}, "id": {meta.ID}, "inline": {"1"}}.Encode())
		require.NoError(t, err)
		_ = r.Body.Close()
		if name == "pic.png" {
			assert.Equal(t, "image/png", r.Header.Get("Content-Type"))
		} else {
			assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"), "HTML is never inline")
			assert.True(t, strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment"))
		}
	}
}
