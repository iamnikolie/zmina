package registry

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

// readLines are a Read of path by the role (tool id) and its result line (uuid), the image in it as Claude Code keeps it.
func readLines(id, uuid, path string, data []byte, kind string) []string {
	return []string{
		fmt.Sprintf(`{"type":"assistant","uuid":"c-%s","message":{"content":[{"type":"tool_use","id":"%s","name":"Read","input":{"file_path":"%s"}}]}}`, id, id, path),
		fmt.Sprintf(`{"type":"user","uuid":"%s","message":{"content":[{"type":"tool_result","tool_use_id":"%s","content":[{"type":"image","source":{"type":"base64","media_type":"%s","data":"%s"}}]}]},"toolUseResult":{"type":"image"}}`,
			uuid, id, kind, base64.StdEncoding.EncodeToString(data)),
	}
}

func transcript(t *testing.T, lines ...[]string) *Registry {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	var all []string
	for _, l := range lines {
		all = append(all, l...)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(strings.Join(all, "\n")), 0o600))
	return &Registry{Namespace: root}
}

func one(s string) []string { return []string{s} }

const (
	prompt  = `{"type":"user","uuid":"p%d","message":{"content":"look at it %d"}}`
	reply   = `{"type":"assistant","uuid":"r%d","message":{"content":[{"type":"text","text":"reply %d"}]}}`
	turnEnd = `{"type":"system","subtype":"turn_duration","uuid":"e%d"}`
)

func shape(t *testing.T, r *Registry) []string {
	t.Helper()
	messages, err := r.ChatMessages("session")
	require.NoError(t, err)
	var got []string
	for _, m := range messages {
		s := m.ID
		if m.Role == "looked" {
			var names []string
			for _, l := range m.Looked {
				names = append(names, l.Name)
			}
			s = "[" + strings.Join(names, ",") + "]"
		}
		if m.Provisional {
			s += "*"
		}
		got = append(got, s)
	}
	return got
}

// D-20: the images a role opened in a turn are one strip over the reply that closes it; a turn in work has it last,
// it and what came since its first image provisional; a file read twice is one tile, its newest picture; a file the
// role also sent, a file the owner attached, a pdf and an error are not in it; no image, no strip.
func TestChatMessagesGathersLookedImagesPerTurn(t *testing.T) {
	r := transcript(t,
		one(fmt.Sprintf(prompt, 1, 1)),
		one(`{"type":"assistant","uuid":"w1","message":{"content":[{"type":"text","text":"checking"}]}}`),
		readLines("t1", "i1", "/tmp/s/a.png", tinyPNG, "image/png"),
		readLines("t2", "i2", "/tmp/s/b.png", tinyPNG, "image/png"),
		readLines("t3", "i3", "/tmp/s/a.png", tinyPNG, "image/png"),
		readLines("t4", "i4", "/state/chat-files/"+strings.Repeat("ab", 32)+"/x/data.png", tinyPNG, "image/png"),
		readLines("t5", "i5", "/tmp/s/fake.png", []byte("<svg/>"), "image/png"),
		readLines("t6", "i6", "/tmp/s/sent.png", tinyPNG, "image/png"),
		one(`{"type":"assistant","uuid":"s1","message":{"content":[{"type":"tool_use","id":"u1","name":"SendUserFile","input":{"files":["/tmp/s/sent.png"]}}]}}`),
		one(`{"type":"user","uuid":"s2","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":"1 file delivered"}]},"toolUseResult":{"attachments":[{"path":"/tmp/s/sent.png","size":10}]}}`),
		one(fmt.Sprintf(reply, 1, 1)),
		one(fmt.Sprintf(turnEnd, 1)),
		one(fmt.Sprintf(prompt, 2, 2)),
		one(fmt.Sprintf(reply, 2, 2)),
		one(fmt.Sprintf(turnEnd, 2)),
		one(fmt.Sprintf(prompt, 3, 3)),
		readLines("t7", "i7", "/tmp/s/c.png", tinyPNG, "image/png"),
		one(fmt.Sprintf(reply, 3, 3)),
	)
	got := shape(t, r)
	require.Equal(t, []string{"p1", "w1", "s2", "[a.png,b.png]", "r1", "p2", "r2", "p3", "r3*", "[c.png]*"}, got)

	messages, err := r.ChatMessages("session")
	require.NoError(t, err)
	strip := messages[3]
	require.Equal(t, "i3:0:0", strip.Looked[0].Key, "a.png read twice: its newest picture, in its first place")
	data, kind, err := r.LookedImage("session", strip.Looked[0].Key)
	require.NoError(t, err)
	require.Equal(t, "image/png", kind)
	require.True(t, bytes.Equal(tinyPNG, data))
	_, _, err = r.LookedImage("session", "i5:0:0")
	require.Error(t, err, "bytes that are not the image they claim")
	_, _, err = r.LookedImage("session", "p1:0:0")
	require.Error(t, err, "not an image")

	history, err := r.History("session", 100)
	require.NoError(t, err)
	require.Contains(t, history, "Session:\nLooked at 2 images")
}

// The strip of a turn in work goes above its reply once the turn is over, and nothing a page already has moves: the
// messages since the turn's first image were provisional until then.
func TestLookedStripSettlesOverTheReply(t *testing.T) {
	open := transcript(t,
		one(fmt.Sprintf(prompt, 1, 1)),
		one(`{"type":"assistant","uuid":"w1","message":{"content":[{"type":"text","text":"checking"}]}}`),
		readLines("t1", "i1", "/tmp/s/a.png", tinyPNG, "image/png"),
		one(`{"type":"assistant","uuid":"w2","message":{"content":[{"type":"text","text":"one more"}]}}`),
	)
	require.Equal(t, []string{"p1", "w1", "w2*", "[a.png]*"}, shape(t, open))
	over := transcript(t,
		one(fmt.Sprintf(prompt, 1, 1)),
		one(`{"type":"assistant","uuid":"w1","message":{"content":[{"type":"text","text":"checking"}]}}`),
		readLines("t1", "i1", "/tmp/s/a.png", tinyPNG, "image/png"),
		one(`{"type":"assistant","uuid":"w2","message":{"content":[{"type":"text","text":"one more"}]}}`),
		one(fmt.Sprintf(turnEnd, 1)),
	)
	require.Equal(t, []string{"p1", "w1", "[a.png]", "w2"}, shape(t, over))
}
