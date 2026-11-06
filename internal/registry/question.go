package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/iamnikolie/zmina/internal/chat"
)

// questionTail is how much of the transcript's end is read for an open question: it is asked in the last turn.
const questionTail = 2 << 20

// PendingQuestion is the role's last AskUserQuestion that has no answer yet in the transcript, nil when there is
// none.
func (r *Registry) PendingQuestion(sessionID string) (*chat.Question, error) {
	path := r.transcript(sessionID)
	if path == "" {
		return nil, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}

	offset := max(stat.Size()-questionTail, 0)
	raw := make([]byte, stat.Size()-offset)
	if _, err = file.ReadAt(raw, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	return pendingQuestion(raw), nil
}

// pendingQuestion scans transcript lines for the last AskUserQuestion call no tool_result has answered.
func pendingQuestion(raw []byte) *chat.Question {
	var open *chat.Question

	for _, record := range bytes.Split(raw, []byte("\n")) {
		if !bytes.Contains(record, []byte("AskUserQuestion")) && (open == nil || !bytes.Contains(record, []byte(open.ID))) {
			continue
		}

		var l line
		if json.Unmarshal(record, &l) != nil || l.Sidechain {
			continue
		}

		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			Input     struct {
				Questions []struct {
					Question    string `json:"question"`
					Header      string `json:"header"`
					MultiSelect bool   `json:"multiSelect"`
					Options     []struct {
						Label       string `json:"label"`
						Description string `json:"description"`
					} `json:"options"`
				} `json:"questions"`
			} `json:"input"`
		}

		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			continue
		}

		for _, b := range blocks {
			switch {
			case l.Type == "assistant" && b.Type == "tool_use" && b.Name == "AskUserQuestion" && len(b.Input.Questions) > 0:
				q := &chat.Question{ID: b.ID}
				for _, item := range b.Input.Questions {
					it := chat.QuestionItem{Question: item.Question, Header: item.Header, MultiSelect: item.MultiSelect}
					for _, o := range item.Options {
						it.Options = append(it.Options, chat.QuestionOption{Label: o.Label, Description: o.Description})
					}
					q.Items = append(q.Items, it)
				}
				open = q
			case l.Type == "user" && b.Type == "tool_result" && open != nil && b.ToolUseID == open.ID:
				open = nil
			}
		}
	}

	return open
}
