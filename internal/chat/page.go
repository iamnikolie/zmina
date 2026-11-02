package chat

import (
	"encoding/base64"
	"encoding/json"
)

type historyCursor struct {
	Session string `json:"s"`
	ID      string `json:"i"`
	Digest  string `json:"d"`
}

// Page resumes only an unchanged prefix of the same conversation. A stale or
// edited transcript explicitly resets instead of silently skipping messages.
// The runtime readers still bound source history to their most recent 8 MiB.
func Page(snapshot Snapshot, after string) Snapshot {
	all := snapshot.Messages
	start := 0
	valid := false
	var previous historyCursor
	if len(after) <= 2048 && after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		valid = err == nil && json.Unmarshal(raw, &previous) == nil && previous.Session == snapshot.Session
	}
	digest := Hash(snapshot.Session)
	digests := make([]string, len(all)+1)
	digests[0] = digest
	anchor := -1
	if valid && previous.ID == "" && previous.Digest == digest {
		anchor = 0
	}
	for i, m := range all {
		raw, _ := json.Marshal(m)
		digest = Hash(digest + string(raw))
		digests[i+1] = digest
		if valid && m.ID == previous.ID && digest == previous.Digest {
			anchor = i + 1
		}
	}
	snapshot.Reset = anchor < 0
	snapshot.HistoryGap = after != "" && snapshot.Reset && previous.Session == snapshot.Session
	if snapshot.Reset {
		recent := Limit(all)
		start = len(all) - len(recent)
	} else {
		start = anchor
	}
	end, size := start, 0
	for end < len(all) && end-start < 200 {
		n := len(all[end].Text)
		if size+n > 1<<20 && end > start {
			break
		}
		size += n
		end++
	}
	snapshot.Messages = append([]Message{}, all[start:end]...)
	for i, m := range snapshot.Messages {
		snapshot.Messages[i] = Limit([]Message{m})[0]
	}
	snapshot.More = end < len(all)
	raw, _ := json.Marshal(historyCursor{Session: snapshot.Session, Digest: digests[end], ID: func() string {
		if end > 0 {
			return all[end-1].ID
		}
		return ""
	}()})
	snapshot.Cursor = base64.RawURLEncoding.EncodeToString(raw)
	return snapshot
}
