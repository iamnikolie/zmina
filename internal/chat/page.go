package chat

import (
	"encoding/base64"
	"encoding/json"
)

type historyCursor struct {
	Session string `json:"s"`
	ID      string `json:"i"`
	Digest  string `json:"d"`
	// Window is the digest of the last messages up to the cursor's (cursorWindow of them): the runtime readers read
	// only a transcript's recent tail, so its oldest messages drop out as it grows; a cursor that holds this resumes
	// across that, where the whole prefix's Digest would not.
	Window string `json:"w,omitempty"`
}

// cursorWindow is how many messages up to a cursor's must be unchanged for it to resume.
const cursorWindow = 50

// Page resumes only after an unchanged recent run of the same conversation: the cursor's message and the
// cursorWindow-1 before it. A stale or edited transcript explicitly resets instead of silently skipping messages.
// The runtime readers bound source history to their most recent 8 MiB, so older messages leave the window as the
// transcript grows: that is not a gap, the client has them.
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
	each := make([]string, len(all))
	for i, m := range all {
		raw, _ := json.Marshal(m)
		each[i] = Hash(string(raw))
		digest = Hash(digest + string(raw))
		digests[i+1] = digest
		if valid && m.ID == previous.ID && digest == previous.Digest {
			anchor = i + 1
		}
	}
	// The recent run up to the nth message (n counted from 1), on the session's own seed.
	window := func(n int) string {
		w := Hash(snapshot.Session)
		for _, d := range each[max(n-cursorWindow, 0):n] {
			w = Hash(w + d)
		}
		return w
	}
	if anchor < 0 && valid && previous.Window != "" && previous.ID != "" {
		for i := len(all) - 1; i >= 0; i-- {
			if all[i].ID == previous.ID {
				if window(i+1) == previous.Window {
					anchor = i + 1
				}
				break
			}
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
	// The cursor stands after the last settled message: a provisional end (a typed /compact Claude Code is about to
	// record as a command) may yet go or change, and a cursor on it would read as edited history.
	settled := end
	if !snapshot.More {
		for settled > start && all[settled-1].Provisional {
			settled--
		}
	}
	next := historyCursor{Session: snapshot.Session, Digest: digests[settled]}
	if settled > 0 {
		next.ID, next.Window = all[settled-1].ID, window(settled)
	}
	raw, _ := json.Marshal(next)
	snapshot.Cursor = base64.RawURLEncoding.EncodeToString(raw)
	return snapshot
}
