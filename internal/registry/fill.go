package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// BigResult is the size past which a tool's result counts as big: it stays in the context, read again every turn
// until a compaction or a restart.
const BigResult = 20_000

// Fill is what one kind of tool call put into the context: its calls, its results' text in characters, how many of
// them were over BigResult, and the images they carried (a Read of a picture: tokens of its own, no text).
type Fill struct {
	Tool   string `json:"tool"`
	Calls  int    `json:"calls"`
	Chars  int    `json:"chars"`
	Big    int    `json:"big,omitempty"`
	Images int    `json:"images,omitempty"`
}

// Add sums another fill of the same tool into f.
func (f *Fill) Add(o Fill) {
	f.Calls += o.Calls
	f.Chars += o.Chars
	f.Big += o.Big
	f.Images += o.Images
}

// Fills reads what the tools of a session and of its subagents put into their context from since until until (zero:
// up to now), by tool kind (ToolKind).
func Fills(t Transcript, since, until time.Time) (map[string]Fill, error) {
	out := map[string]Fill{}

	paths := []string{t.Path}
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(t.Path, ".jsonl"), "subagents", "*.jsonl"))
	paths = append(paths, subs...)

	for _, p := range paths {
		if err := readFills(p, since, until, out); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// fillLine is the part of a transcript line Fills reads.
type fillLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type fillBlock struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input struct {
		Command string `json:"command"`
	} `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

func readFills(path string, since, until time.Time, out map[string]Fill) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	kinds := map[string]string{}
	rd := bufio.NewReaderSize(f, 1<<20)

	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 && (bytes.Contains(raw, []byte(`"tool_use"`)) || bytes.Contains(raw, []byte(`"tool_result"`))) {
			fillFrom(raw, since, until, kinds, out)
		}

		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}
	}
}

func fillFrom(raw []byte, since, until time.Time, kinds map[string]string, out map[string]Fill) {
	var l fillLine
	if json.Unmarshal(raw, &l) != nil || l.Timestamp.Before(since) || !until.IsZero() && !l.Timestamp.Before(until) {
		return
	}

	var blocks []fillBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return
	}

	for _, b := range blocks {
		switch {
		case l.Type == "assistant" && b.Type == "tool_use":
			k := b.Name
			if k == "Bash" {
				k = "Bash: " + ToolKind(b.Input.Command)
			}

			kinds[b.ID] = k
			f := out[k]
			f.Tool = k
			f.Calls++
			out[k] = f
		case l.Type == "user" && b.Type == "tool_result":
			k, ok := kinds[b.ToolUseID]
			if !ok {
				k = "?"
			}

			chars, images := resultSize(b.Content)
			f := out[k]
			f.Tool = k
			f.Chars += chars
			f.Images += images

			if chars > BigResult {
				f.Big++
			}

			out[k] = f
		}
	}
}

// resultSize is a tool result's text in characters and its images: a picture's base64 is not text in the context.
func resultSize(raw json.RawMessage) (chars, images int) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return len([]rune(s)), 0
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return 0, 0
	}

	for _, p := range parts {
		switch p.Type {
		case "text":
			chars += len([]rune(p.Text))
		case "image":
			images++
		}
	}

	return chars, images
}

// toolKinds name a shell command by its first program, after any cd …&& and VAR=… before it.
var toolKinds = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"cat/sed/head/tail", regexp.MustCompile(`^(?:command\s+)?(?:cat|sed|head|tail|less|nl|awk)\b`)},
	{"grep/rg", regexp.MustCompile(`^(?:grep|rg|egrep|ag)\b`)},
	{"git", regexp.MustCompile(`^git\b`)},
	{"go test", regexp.MustCompile(`^go\s+test\b`)},
	{"go", regexp.MustCompile(`^go\b`)},
	{"make", regexp.MustCompile(`^make\b`)},
	{"find/ls", regexp.MustCompile(`^(?:find|ls|tree|wc)\b`)},
	{"python", regexp.MustCompile(`^python3?\b`)},
	{"curl", regexp.MustCompile(`^curl\b`)},
	{"ssh", regexp.MustCompile(`^(?:ssh|scp)\b`)},
	{"gh/gl", regexp.MustCompile(`^(?:gh|gl|glab)\b`)},
	{"docker/kubectl", regexp.MustCompile(`^(?:docker|kubectl)\b`)},
	{"npm/pnpm/npx", regexp.MustCompile(`^(?:npm|pnpm|npx|yarn|node)\b`)},
}

// zminaSub is a zmina command's subcommand, after its global flags (--crew file, --config name).
var zminaSub = regexp.MustCompile(`^(?:\S*/)?zmina\s+(?:--(?:crew|config)(?:=\S+|\s+\S+)\s+)*([a-z][a-z-]*)`)

// ackFirst is a leading zmina ack and what joins the next command to it.
var ackFirst = regexp.MustCompile(`^(?:\S*/)?zmina\s+(?:--(?:crew|config)(?:=\S+|\s+\S+)\s+)*ack\b[^&;|\n]*(?:&&|;|\|\||\n)`)

var shellPrefix = regexp.MustCompile(`^(?:\s*(?:cd\s+[^&;|]*(?:&&|;)|[A-Za-z_][A-Za-z0-9_]*=\S*\s+|export\s+[^;&]*(?:&&|;)))+\s*`)

// ToolKind names a Bash command's kind: what usage --by tool groups the shell's results by.
func ToolKind(cmd string) string {
	c := shellPrefix.ReplaceAllString(strings.TrimSpace(cmd), "")

	// zmina ack prints a word: a command chained after it (zmina ack S-1 && zmina task show T-2) is what fills.
	for {
		m := ackFirst.FindStringIndex(c)
		if m == nil {
			break
		}

		c = shellPrefix.ReplaceAllString(strings.TrimSpace(c[m[1]:]), "")
	}

	if m := zminaSub.FindStringSubmatch(c); m != nil {
		return "zmina " + m[1] // zmina's own output is zmina's to make lean: by subcommand
	}

	for _, k := range toolKinds {
		if k.re.MatchString(c) {
			return k.kind
		}
	}

	return "other"
}
