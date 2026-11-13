package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTag  = "[zmina:abcdefghijkl] "
	testBell = "[zmina] 3 lines wait for you (dev, ops on T-1): zmina inbox show in-0123456789ab"
	testChat = "[zmina chat 0123456789abcdef] go on"
	testMark = "[written 14:02, before your last line on T-1 (14:05)] "
)

// deliveryShapes are the texts a role's turn can start with, and what started it. The ones behind zmina's tag are what
// zmina delivers on a role's own channel; the others are what is typed at a prompt.
var deliveryShapes = []struct {
	name, text, want string
}{
	// One line behind the tag keeps its kind, as the same line typed does.
	{"tagged handoff", testTag + "[zmina T-1] Read handoff packet", TriggerHandoff},
	{"tagged scheduled handoff", testTag + "[zmina T-1, scheduled] run the report", TriggerSchedule},
	{"tagged remote line", testTag + `[zmina] remote acme/ops: "hi"`, TriggerRemote},
	{"tagged notice", testTag + "[zmina] dev idle 40m", TriggerNotice},
	{"tagged lines of several kinds, one to a line", testTag + "[zmina] dev idle 40m\n[zmina] remote acme/ops: \"hi\"", TriggerNotice},
	// A line with nothing of zmina's own in its words is still zmina's: it is behind the tag.
	{"tagged line another role queued", testTag + "[dispatcher] look at T-2", TriggerDelivery},
	{"tagged plain line", testTag + "Note from alex: ship it", TriggerDelivery},
	{"tagged line behind a crossing mark", testTag + testMark + "[dev] done", TriggerDelivery},
	{"tagged tag alone", "[zmina:abcdefghijkl]", TriggerDelivery},
	// Several lines in one numbered message are told by their parts.
	{"joined handoffs", testTag + "1. [zmina T-1] Read handoff packet\n2. [zmina T-2] Read handoff packet", TriggerHandoff},
	{"joined notices", testTag + "1. [zmina] dev idle 40m\n2. [zmina] ops stuck", TriggerNotice},
	{"joined kinds", testTag + "1. [zmina T-1] Read handoff packet\n2. [zmina] dev idle 40m", TriggerDelivery},
	{"joined lines of roles", testTag + "1. [dispatcher] look at T-2\n2. [ops] ready", TriggerDelivery},
	{"joined lines behind marks", testTag + "1. [dev] done\n2. " + testMark + "[ops] ready", TriggerDelivery},
	{"joined with a line spread over lines", testTag + "1. [dev] first\nsecond row\n2. [ops] ready", TriggerDelivery},
	{"joined with the owner's chat line", testTag + "1. [zmina] dev idle 40m\n2. " + testChat, TriggerOwner},
	{"joined with the owner's chat line first", testTag + "1. " + testChat + "\n2. [dispatcher] look at T-2", TriggerOwner},
	{"tagged chat line", testTag + testChat, TriggerOwner},
	{"joined eight lines", testTag + strings.Join([]string{"1. [a] x", "2. [a] x", "3. [a] x", "4. [a] x", "5. [a] x", "6. [a] x",
		"7. [a] x", "8. [a] x"}, "\n"), TriggerDelivery},
	// A doorbell stands for lines that wait in the inbox: what they are is not in its words.
	{"tagged doorbell", testTag + testBell, TriggerDelivery},
	{"tagged doorbell for one line", testTag + "[zmina] 1 line waits for you: zmina inbox show in-0123456789ab", TriggerDelivery},
	{"tagged doorbell, crossed lines", testTag + "[zmina] 2 lines wait for you (dev on T-1 · 1 crossed): zmina inbox show in-0123456789ab", TriggerDelivery},
	{"typed doorbell", testBell, TriggerDelivery},
	// Without the tag, a line is zmina's only by how it opens.
	{"typed handoff", "[zmina T-1] Read handoff packet", TriggerHandoff},
	{"typed notice", "[zmina] dev idle 40m", TriggerNotice},
	{"typed joined handoffs", "1. [zmina T-1] a\n2. [zmina T-2] b", TriggerHandoff},
	{"typed joined kinds", "1. [zmina T-1] a\n2. [zmina] b", TriggerDelivery},
	// The owner's is what nothing claims.
	{"owner's words", "fix the login", TriggerOwner},
	{"owner's chat line", testChat, TriggerOwner},
	{"owner's chat line, no id", "[zmina chat nothex] go on", TriggerOwner},
	{"owner's list", "1. buy milk\n2. call alex", TriggerOwner},
	{"owner's first point", "1. do this first", TriggerOwner},
	{"owner's list whose first point quotes zmina", "1. [zmina T-1] a", TriggerOwner},
	{"owner's list with one line of zmina's", "1. [zmina T-1] a\n2. buy milk", TriggerOwner},
	{"owner's list with the words of a line of zmina's after", "1. buy milk\n2. [zmina T-1] a", TriggerOwner},
	{"owner's list out of step", "1. [zmina T-1] a\n3. [zmina T-2] b", TriggerOwner},
	{"owner quotes zmina mid-text", "see [zmina T-1] and [zmina] notes", TriggerOwner},
	{"owner quotes a doorbell", "he said: " + testBell, TriggerOwner},
	{"owner quotes a tag", "the tag is [zmina:abcdefghijkl] now", TriggerOwner},
	{"owner's words after a newline", "hm\n[zmina] dev idle 40m", TriggerOwner},
	{"owner's short tag", "[zmina:abc] hi", TriggerOwner},
	{"owner's capital tag", "[zmina:ABCDEFGHIJKL] hi", TriggerOwner},
	{"owner's tag with no gap", "[zmina:abcdefghijkl]hi", TriggerOwner},
	{"owner's slash command", "/compact", TriggerOwner},
	{"tagged chat line with no id", testTag + "[zmina chat nothex] go on", TriggerDelivery},
}

func TestDeliveryTriggers(t *testing.T) {
	for _, c := range deliveryShapes {
		t.Run(c.name, func(t *testing.T) {
			got, from := TextTrigger(c.text)
			assert.Equal(t, c.want, got, c.text)
			assert.Empty(t, from)

			again, _ := textTrigger(c.text)
			assert.Equal(t, got, again, "the exported and the package's classifier are one")
		})
	}
}

// Peers and agents are told by their envelopes, ahead of everything else.
func TestDeliveryTriggersKeepEnvelopes(t *testing.T) {
	got, from := textTrigger(`<cross-session-message from="uds:x" from-name="zm-ops">1. [zmina T-1] a</cross-session-message>`)
	assert.Equal(t, TriggerPeer, got)
	assert.Equal(t, "zm-ops", from)

	got, _ = textTrigger("<task-notification>" + testTag + "</task-notification>")
	assert.Equal(t, TriggerAgent, got)
}

func TestNumberedParts(t *testing.T) {
	for text, want := range map[string][]string{
		"1. a\n2. b":                         {"a", "b"},
		"1. a\nb\n2. c\n3. d":                {"a\nb", "c", "d"},
		"1. a\n2. b\n3. 3. c":                {"a", "b", "3. c"},
		"1. a":                               nil,
		"2. a\n3. b":                         nil,
		"1. a\n3. b":                         nil,
		"1.a\n2.b":                           nil,
		"a\n1. a\n2. b":                      nil,
		"":                                   nil,
		"1. a\n2. b\n3. c\n4. d\n5. e\n6. f": {"a", "b", "c", "d", "e", "f"},
	} {
		assert.Equal(t, want, numberedParts(text), text)
	}
}

// The same lines reach a Claude role as a message another session posted: Claude Code writes it with the sender and
// its own words around it. What started its turn is the same as for a Codex role's prompt of the same text.
func TestPostedMessagesAreTold(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	type turn struct{ name, extra, text, want string }

	var turns []turn

	// What zmina posts to the inbox socket is a meta line from a peer; what is typed has no origin.
	posted := func(from string) string {
		return fmt.Sprintf(`,"isMeta":true,"origin":{"kind":"peer","from":%q,"verifiedPeerPid":7,"msg_id":"x"}`, from)
	}
	wrap := func(text string) string { return peerWrapHead + text + peerWrapTail + " — not typed by your user." }

	for _, c := range deliveryShapes {
		if strings.HasPrefix(c.text, testTag) {
			turns = append(turns, turn{c.name, posted("zmina"), wrap(c.text), c.want})
		} else {
			turns = append(turns, turn{c.name, "", c.text, c.want})
		}
	}

	// Another session's message is its own, whatever its text says; so is one of zmina's with no tag.
	turns = append(turns,
		turn{"other sender", posted("uds:other"), wrap(testTag + "[zmina T-1] Read handoff packet"), TriggerPeer},
		turn{"no tag", posted("zmina"), wrap("[zmina T-1] Read handoff packet"), TriggerPeer},
		turn{"wrapped message", posted("zmina"), wrap(`<cross-session-message from="uds:x" from-name="zm-ops">` +
			testTag + `[zmina T-1] a</cross-session-message>`), TriggerPeer},
	)

	var lines []string

	for i, c := range turns {
		content, _ := json.Marshal(c.text)
		at := time.Date(2026, 11, 9, 10, 0, 2*i, 0, time.UTC).Format(time.RFC3339)
		lines = append(lines,
			fmt.Sprintf(`{"type":"user","timestamp":%q%s,"message":{"role":"user","content":%s}}`, at, c.extra, content),
			fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":"m%d","model":"claude-opus-5-5","usage":{"input_tokens":1}}}`,
				time.Date(2026, 11, 9, 10, 0, 2*i+1, 0, time.UTC).Format(time.RFC3339), i))
	}

	path := filepath.Join(dir, "s1.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	got, err := Messages(Transcript{Path: path, ID: "s1"}, time.Time{})
	require.NoError(t, err)
	require.Len(t, got, len(turns))

	for i, m := range got {
		assert.Equal(t, turns[i].want, m.Trigger, turns[i].name)
		assert.Empty(t, m.From, turns[i].name)
	}
}

// A line queued into a turn that runs is an attachment, with its origin beside it. What zmina posted is its own, and
// the owner's chat line it posted is his: neither is another session's.
func TestActivityToldPostedAttachments(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	at := func(m int) string { return time.Date(2026, 11, 9, 10, m, 0, 0, time.UTC).Format(time.RFC3339) }
	queued := func(m int, from, prompt string) string {
		raw, _ := json.Marshal(prompt)

		return `{"type":"attachment","timestamp":"` + at(m) + `","attachment":{"type":"queued_command","commandMode":"prompt",` +
			`"isMeta":true,"origin":{"kind":"peer","from":"` + from + `"},"prompt":` + string(raw) + `}}`
	}

	write := func(lines ...string) Activity {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600))

		a, err := (&Registry{Namespace: ns}).Activity("s1")
		require.NoError(t, err)

		return a
	}

	a := write(
		queued(1, "zmina", testTag+"1. [zmina T-1] a\n2. [zmina] b"),
		queued(2, "zmina", testTag+testBell),
		queued(3, "uds:other", testTag+testChat),
		queued(4, "zmina", "[zmina T-1] untagged"),
	)
	assert.True(t, a.Owner.IsZero(), "zmina's lines and another session's are not the owner's")

	a = write(
		queued(1, "zmina", testTag+"[zmina] dev idle 40m"),
		queued(2, "zmina", testTag+"1. [zmina] dev idle 40m\n2. "+testChat),
	)
	assert.Equal(t, at(2), a.Owner.Format(time.RFC3339), "a joined message with the owner's chat line is his")
}
