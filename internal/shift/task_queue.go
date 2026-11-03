package shift

import (
	"fmt"
	"slices"
	"strings"
)

// staleTaskPacket only discards recognizable locators whose current ledger record proves them obsolete.
// Missing/unreadable tasks and ordinary messages remain queued; never guess from an ID mentioned in prose.
func (s *Shift) staleTaskPacket(name, line string) bool {
	if s.Tasks == nil || !strings.HasPrefix(line, "[zmina T-") {
		return false
	}
	id, _, ok := strings.Cut(strings.TrimPrefix(line, "[zmina "), "] Read handoff packet ")
	if !ok {
		return false
	}
	t, err := s.Tasks.Get(id)
	if err != nil {
		return false
	}
	return !t.State.Active() || t.Role != name ||
		(t.Delivery != nil && t.Delivery.Line != "" && t.Delivery.Line != line)
}

func (s *Shift) pruneTaskInbox(name string) {
	rs := s.State.Role(name)
	before := len(rs.Inbox) + len(rs.Failed)
	head := ""
	if len(rs.Inbox) > 0 {
		head = rs.Inbox[0]
	}
	rs.Inbox = slices.DeleteFunc(rs.Inbox, func(line string) bool { return s.staleTaskPacket(name, line) })
	if len(rs.Inbox) == 0 || rs.Inbox[0] != head {
		rs.Attempts = 0
	}
	rs.Failed = slices.DeleteFunc(rs.Failed, func(f Failed) bool { return s.staleTaskPacket(name, f.Line) })
	removed := before - len(rs.Inbox) - len(rs.Failed)
	if rs.Sent != nil && s.staleTaskPacket(name, rs.Sent.Line) {
		// Prompt exposes only a visible line, not the complete editable input. Even an exact match may
		// hide an owner edit on a wrapped/continuation line, so pruning never authorizes terminal keys.
		if draft, ok, err := s.Tmux.Prompt(name); err != nil || !ok || draft != "" {
			text := fmt.Sprintf("%s: obsolete task packet retired; input left untouched; inspect with zmina --crew %s peek %s, then send or clear the draft manually", name, shellWord(s.Crew.Path), shellWord(name))
			s.addNotes([]string{text})
			meta := s.State.NoticeMeta[text]
			meta.Kind, meta.Role = "task-input", name
			s.State.NoticeMeta[text] = meta
		}
		rs.Sent = nil
		removed++
	}
	if removed > 0 {
		s.log(name, "task-packet-pruned", "", fmt.Sprintf("%d obsolete locators", removed))
	}
}
