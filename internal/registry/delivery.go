package registry

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/iamnikolie/zmina/internal/chat"
)

// PostedBy is the name zmina posts under to a Claude session's inbox socket: Claude Code records the message as a
// peer's, from this.
const PostedBy = "zmina"

var (
	// channelTag opens every line zmina delivers on a role's own channel (a Claude inbox socket, a Codex bridge's
	// inbox): zmina and twelve base32 characters that are one start's (shift.tagMark). No other sender knows them.
	channelTag = regexp.MustCompile(`^\[zmina:[a-z2-7]{12}\](?:\s+|$)`)
	// bellLine is a doorbell as shift.bellText writes it: the lines it stands for wait in zmina's inbox, so the text
	// of the turn says nothing of what they are.
	bellLine = regexp.MustCompile(`^\[zmina\] [0-9]+ lines? waits? for you(?: \(.*\))?: zmina inbox show in-[0-9a-f]{12}$`)
	// crossingMark is what a line carries in front of its words when it was written before a reader's newer line
	// (shift's crossing marks).
	crossingMark = regexp.MustCompile(`^\[written [^\]]*\]\s+`)
)

// cutTag takes zmina's channel tag off the front of a text; ok is false for a text that has none.
func cutTag(text string) (rest string, ok bool) {
	loc := channelTag.FindStringIndex(text)
	if loc == nil {
		return text, false
	}

	return text[loc[1]:], true
}

// tagged reports a text that opens with zmina's channel tag: zmina delivered it, whatever it says.
func tagged(text string) bool {
	_, ok := cutTag(text)

	return ok
}

// numberedParts reads a joined delivery: the lines of one post written "1. …" and "2. …", one to a line, numbered in
// step from 1 (shift.numbered). Nil for a text that is not one: a single part, or numbers out of step, is a text that
// merely starts with "1. ".
func numberedParts(text string) []string {
	var parts []string

	rest := text

	for n := 1; ; n++ {
		body, ok := strings.CutPrefix(rest, strconv.Itoa(n)+". ")
		if !ok {
			return nil
		}

		part, tail, more := strings.Cut(body, "\n"+strconv.Itoa(n+1)+". ")
		parts = append(parts, strings.TrimSpace(part))

		if !more {
			break
		}

		rest = strconv.Itoa(n+1) + ". " + tail
	}

	if len(parts) < 2 {
		return nil
	}

	return parts
}

// chatLine reports the owner's own message from Chat, as it stands once typed or posted: "[zmina chat <id>] …".
func chatLine(text string) bool {
	rest, ok := strings.CutPrefix(text, "[zmina chat ")
	if !ok {
		return false
	}

	id, _, ok := strings.Cut(rest, "]")

	return ok && chat.ValidID(id)
}

// lineTrigger is what one line says of itself by how it opens: the owner's chat line, a doorbell, and zmina's own
// task, schedule, remote and notice lines. "" for a line that opens as none of them.
func lineTrigger(line string) string {
	line = crossingMark.ReplaceAllString(strings.TrimSpace(line), "")

	switch {
	case chatLine(line):
		return TriggerOwner
	case bellLine.MatchString(line):
		return TriggerDelivery
	case strings.HasPrefix(line, "[zmina T-"):
		if head, _, _ := strings.Cut(line, "]"); strings.HasSuffix(head, ", scheduled") {
			return TriggerSchedule
		}

		return TriggerHandoff
	case strings.HasPrefix(line, "[zmina] remote "):
		return TriggerRemote
	case strings.HasPrefix(line, "[zmina]"):
		return TriggerNotice
	}

	return ""
}

// deliveryTrigger is the trigger of a text that is not a peer's or an agent's: zmina's own lines, one, joined, or as a
// doorbell, and else the owner's. A line behind zmina's tag is zmina's whatever it says, and what it says only tells
// which of its kinds it is; a text without the tag is zmina's only when every line of it opens as one of zmina's (a
// pasted "[zmina …" in the middle of a message, or a message that starts with "1. ", is the owner's). The owner's own
// chat line is the owner's wherever it stands, so a joined message that holds one is his turn: this is the usage
// accounting's reading of a text, and the session's activity does not take it for the owner writing (a delivered line is
// never his there, since any role can queue one that reads as his chat line). Parts of one kind are that kind; parts of
// several are a delivery.
func deliveryTrigger(text string) string {
	body, isTagged := cutTag(text)

	parts := numberedParts(body)
	if parts == nil {
		parts = []string{body}
	}

	kinds := make([]string, len(parts))

	for i, part := range parts {
		kinds[i] = lineTrigger(part)

		switch {
		case kinds[i] != "":
		case isTagged:
			kinds[i] = TriggerDelivery
		default:
			return TriggerOwner
		}
	}

	switch {
	case slices.Contains(kinds, TriggerOwner):
		return TriggerOwner
	case slices.Min(kinds) != slices.Max(kinds):
		return TriggerDelivery
	}

	return kinds[0]
}

// postedTrigger is the trigger of a message in a Claude transcript that zmina posted to the session's inbox socket:
// Claude Code records it as a peer's, from PostedBy, with its own words around the text (peerWrapHead, peerWrapTail),
// and the text starts with zmina's tag. ok is false for any other peer's message, which stays a peer's whatever its
// text says.
func postedTrigger(from, text string) (trigger string, ok bool) {
	if from != PostedBy {
		return "", false
	}

	if body, wrapped := strings.CutPrefix(text, peerWrapHead); wrapped {
		text = body
		if i := strings.LastIndex(text, peerWrapTail); i >= 0 {
			text = text[:i]
		}
	}

	text = strings.TrimSpace(text)
	if !tagged(text) {
		return "", false
	}

	return deliveryTrigger(text), true
}
