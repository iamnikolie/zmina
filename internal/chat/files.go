package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
)

const MaxFileBytes = 8 << 20
const MaxFiles = 4
const maxStoredBytes = 256 << 20
const maxStoredFiles = 1024

type Attachment struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int    `json:"size"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	// Width and Height are an image's size as shown (its EXIF orientation applied), so its tile has its final size
	// before the bytes arrive; absent for other files and for files stored before they were kept.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// Looked is an image a role opened for itself (Claude Code's Read): Key finds its bytes as the role saw them, Path and
// Name are the file it read.
type Looked struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// PathInfo is a file a role named in its message that Chat shows: its path as named, name, size and type.
type PathInfo struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int    `json:"size"`
	Type string `json:"type"`
}

type Upload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// FileStore is scoped to a crew and a role. Call Put under the crew state lock
// so quota checks and immutable ID creation serialize across processes.
type FileStore struct{ Dir, Role string }

func (s FileStore) root(id string) string { return filepath.Join(s.Dir, Hash(s.Role), id) }
func fileName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 240 && !strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsControl) < 0
}
func filePath(root, name string) string { return filepath.Join(root, "data"+filepath.Ext(name)) }

func (s FileStore) Get(id string) (Attachment, string, error) {
	if !ValidID(id) {
		return Attachment{}, "", errors.New("invalid attachment ID")
	}
	raw, err := os.ReadFile(filepath.Join(s.root(id), "metadata.json"))
	if err != nil {
		return Attachment{}, "", fmt.Errorf("attachment unavailable: %w", err)
	}
	var meta Attachment
	if err = json.Unmarshal(raw, &meta); err != nil || meta.ID != id || !fileName(meta.Name) || meta.Size < 0 || meta.Size > MaxFileBytes {
		return Attachment{}, "", errors.New("invalid attachment metadata")
	}
	path := filePath(s.root(id), meta.Name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(meta.Size) {
		return Attachment{}, "", errors.New("attachment data unavailable")
	}
	return meta, path, nil
}

func (s FileStore) Put(upload Upload) (Attachment, error) {
	if !ValidID(upload.ID) || !fileName(upload.Name) || len(upload.Data) > MaxFileBytes {
		return Attachment{}, errors.New("attachment needs a valid ID, plain filename and at most 8 MiB")
	}
	sum := sha256.Sum256(upload.Data)
	meta := Attachment{ID: upload.ID, Name: upload.Name, Size: len(upload.Data), Type: http.DetectContentType(upload.Data), SHA256: hex.EncodeToString(sum[:])}
	if existing, _, err := s.Get(upload.ID); err == nil {
		// The size is derived from the bytes, which the hash already pins: a file stored before sizes were kept is the
		// same file, and keeps its metadata as stored (a message's hash is made from it).
		same := existing
		same.Width, same.Height = 0, 0
		if same != meta {
			return Attachment{}, errors.New("attachment ID already has different content")
		}
		return existing, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Attachment{}, err
	}
	if strings.HasPrefix(meta.Type, "image/") {
		meta.Width, meta.Height = imageSize(upload.Data)
	}
	var size int64
	count := 0
	err := filepath.WalkDir(s.Dir, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
			if entry.Name() == "metadata.json" {
				count++
			}
		}
		return nil
	})
	if err != nil {
		return Attachment{}, err
	}
	if size+int64(len(upload.Data))+2048 > maxStoredBytes || count >= maxStoredFiles {
		return Attachment{}, errors.New("attachment storage is full; remove unused files from the crew chat-files directory")
	}
	parent := filepath.Dir(s.root(upload.ID))
	if err = os.MkdirAll(parent, 0700); err != nil {
		return Attachment{}, err
	}
	temp, err := os.MkdirTemp(parent, ".upload-")
	if err != nil {
		return Attachment{}, err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	if err = os.WriteFile(filePath(temp, upload.Name), upload.Data, 0600); err != nil {
		return Attachment{}, err
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return Attachment{}, err
	}
	if err = os.WriteFile(filepath.Join(temp, "metadata.json"), raw, 0600); err != nil {
		return Attachment{}, err
	}
	if err = os.Rename(temp, s.root(upload.ID)); err != nil {
		return Attachment{}, err
	}
	return meta, nil
}

// Sized fills in the size of the images among attachments stored before sizes were kept (D-21 s1): read once from the
// head of the stored file and remembered, since a file never changes under its ID. Others are returned as they are;
// the attachments given are not changed.
func (s FileStore) Sized(attachments []Attachment) []Attachment {
	var out []Attachment
	for i, a := range attachments {
		if a.Width > 0 || !strings.HasPrefix(a.Type, "image/") {
			continue
		}
		w, h := s.size(a.ID)
		if w == 0 {
			continue
		}
		if out == nil {
			out = slices.Clone(attachments)
		}
		out[i].Width, out[i].Height = w, h
	}
	if out == nil {
		return attachments
	}
	return out
}

// sizeHead is how much of a stored image is read for its size: a phone photo's EXIF (its orientation, a thumbnail)
// and the frame header after it.
const sizeHead = 256 << 10

var sizes sync.Map // stored file root (crew dir, role, ID) → [2]int, looked up before its metadata is read

func (s FileStore) size(id string) (int, int) {
	key := s.root(id)
	if v, ok := sizes.Load(key); ok {
		wh := v.([2]int)
		return wh[0], wh[1]
	}
	_, path, err := s.Get(id)
	if err != nil {
		return 0, 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()
	head, err := io.ReadAll(io.LimitReader(f, sizeHead))
	if err != nil {
		return 0, 0
	}
	w, h := imageSize(head)
	sizes.Store(key, [2]int{w, h})
	return w, h
}

func MessageHash(text string, attachments []Attachment) string {
	if len(attachments) == 0 {
		return Hash(text)
	}
	raw, _ := json.Marshal(attachments)
	return Hash(text + "\x00" + string(raw))
}
