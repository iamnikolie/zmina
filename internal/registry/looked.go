package registry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/iamnikolie/zmina/internal/chat"
)

// The images a role opens for itself (Claude Code's Read of an image) and Chat shows as one strip per turn (D-20).
// Claude Code keeps each image in the transcript as the role saw it, so Chat serves those bytes: the picture of that
// moment, whatever the file became since.

// imageBlock is an image in a tool's result as Claude Code records it.
type imageBlock struct {
	Type   string `json:"type"`
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// ownerFile is a file the owner attached in Chat (the crew's chat-files store): the role reads it, the owner sent it.
var ownerFile = regexp.MustCompile(`/chat-files/[0-9a-f]{64}/`)

// imageKinds are the images Chat shows by their first bytes (D-16), by media type.
var imageKinds = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// ImageKind is the type an image's first bytes say, of the ones Chat shows; "" for any other.
func ImageKind(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp"
	}

	return ""
}

// shown is the type of an image block Chat may show: a base64 image of a kind it shows by its first bytes, as its
// media type says, at most chat.MaxFileBytes; "" for any other. Only its head is decoded.
func (b imageBlock) shown() string {
	if b.Type != "image" || b.Source.Type != "base64" || !imageKinds[b.Source.MediaType] ||
		base64.StdEncoding.DecodedLen(len(b.Source.Data)) > chat.MaxFileBytes+2 || len(b.Source.Data) < 24 {
		return ""
	}

	head, err := base64.StdEncoding.DecodeString(b.Source.Data[:24])
	if err != nil || ImageKind(head) != b.Source.MediaType {
		return ""
	}

	return b.Source.MediaType
}

// readImages reads a role's Read calls: a call on the role's line is remembered (reads, by tool_use id → path); on
// the line of its result, the images Chat shows, keyed by that line and the block's place in the result.
func readImages(l *line, reads map[string]string) []chat.Looked {
	var blocks []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil
	}

	var out []chat.Looked

	for i, b := range blocks {
		switch {
		case l.Type == "assistant" && b.Type == "tool_use" && b.Name == "Read":
			var in struct {
				Path string `json:"file_path"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Path != "" {
				reads[b.ID] = in.Path
			}
		case l.Type == "user" && b.Type == "tool_result" && l.UUID != "":
			path, ok := reads[b.ToolUseID]
			if !ok {
				continue
			}

			delete(reads, b.ToolUseID)

			if b.IsError || ownerFile.MatchString(path) {
				continue
			}

			var images []imageBlock
			if json.Unmarshal(b.Content, &images) != nil {
				continue
			}

			for j, img := range images {
				if kind := img.shown(); kind != "" {
					out = append(out, chat.Looked{Key: l.UUID + ":" + strconv.Itoa(i) + ":" + strconv.Itoa(j),
						Path: path, Name: filepath.Base(path), Type: kind})
				}
			}
		}
	}

	return out
}

// toolResult is true for a user line that carries a tool's result: part of the role's turn, not a new prompt.
func toolResult(l *line) bool {
	var blocks []struct {
		Type string `json:"type"`
	}

	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return false
	}

	for _, b := range blocks {
		if b.Type == "tool_result" {
			return true
		}
	}

	return false
}

// lookTurn gathers the images of the turn in work.
type lookTurn struct {
	start  int // where in the messages the turn's first image came
	images []chat.Looked
	sent   map[string]bool // paths the role sent the owner this turn: they show in the sent gallery only
}

func (t *lookTurn) add(at int, images []chat.Looked) {
	for _, img := range images {
		if len(t.images) == 0 {
			t.start = at
		}

		replaced := false

		for k := range t.images {
			if t.images[k].Path == img.Path {
				t.images[k] = img // the same file read again: its newest picture, in its first place

				replaced = true

				break
			}
		}

		if !replaced {
			t.images = append(t.images, img)
		}
	}
}

func (t *lookTurn) send(paths []SentFile) {
	if t.sent == nil {
		t.sent = map[string]bool{}
	}

	for _, p := range paths {
		t.sent[filepath.Clean(p.Path)] = true
	}
}

// close puts the turn's strip into out and starts the next turn. A turn that is over has its strip directly above its
// last reply after its first image (the reply that closes it), else after its last message. A turn still in work has
// it last, over the working line, and the strip and every message since the turn's first image are provisional: the
// strip grows and goes above the reply once that lands, so a page's cursor stays before them all.
func (t *lookTurn) close(out []chat.Message, over bool) []chat.Message {
	var shown []chat.Looked

	for _, img := range t.images {
		if !t.sent[filepath.Clean(img.Path)] {
			shown = append(shown, img)
		}
	}

	start := t.start
	*t = lookTurn{}

	if len(shown) == 0 {
		return out
	}

	strip := chat.Message{ID: "looked:" + strings.SplitN(shown[0].Key, ":", 2)[0], Role: "looked", Looked: shown}
	at := len(out)

	if over {
		for k := len(out) - 1; k >= start; k-- {
			if out[k].Role == "assistant" && out[k].Event == nil {
				at = k

				break
			}
		}
	} else {
		for k := start; k < len(out); k++ {
			out[k].Provisional = true
		}

		strip.Provisional = true
	}

	if at > 0 {
		strip.At = out[at-1].At
	}

	return append(out[:at], append([]chat.Message{strip}, out[at:]...)...)
}

// LookedImage is the picture a role saw: the image under key (as a Looked has it) in the session's transcript.
func (r *Registry) LookedImage(sessionID, key string) ([]byte, string, error) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 || parts[0] == "" {
		return nil, "", errors.New("no such image")
	}

	i, err1 := strconv.Atoi(parts[1])
	j, err2 := strconv.Atoi(parts[2])

	if err1 != nil || err2 != nil {
		return nil, "", errors.New("no such image")
	}

	records, err := r.records(sessionID)
	if err != nil {
		return nil, "", err
	}

	marker := []byte(`"uuid":"` + parts[0] + `"`)

	for _, record := range records {
		if !bytes.Contains(record, marker) {
			continue
		}

		var l line
		if json.Unmarshal(record, &l) != nil || l.UUID != parts[0] || l.Type != "user" {
			continue
		}

		var blocks []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}

		var images []imageBlock

		if json.Unmarshal(l.Message.Content, &blocks) != nil || i >= len(blocks) || blocks[i].Type != "tool_result" ||
			json.Unmarshal(blocks[i].Content, &images) != nil || j >= len(images) {
			break
		}

		kind := images[j].shown()
		if kind == "" {
			break
		}

		data, err := base64.StdEncoding.DecodeString(images[j].Source.Data)
		if err != nil || len(data) > chat.MaxFileBytes || ImageKind(data[:min(len(data), 16)]) != kind {
			break
		}

		return data, kind, nil
	}

	return nil, "", errors.New("no such image")
}
