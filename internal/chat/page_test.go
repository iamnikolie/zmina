package chat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestHistoryCursorCatchesUpAcrossSeveralPages(t *testing.T) {
	snapshot := Snapshot{Session: "one", Messages: []Message{}}
	first := Page(snapshot, "")
	require.True(t, first.Reset)
	require.NotEmpty(t, first.Cursor)
	for i := 0; i < 465; i++ {
		snapshot.Messages = append(snapshot.Messages, Message{ID: fmt.Sprint(i), Role: "assistant", Text: fmt.Sprint(i)})
	}
	var received []Message
	cursor := first.Cursor
	for {
		next := Page(snapshot, cursor)
		require.False(t, next.Reset)
		require.LessOrEqual(t, len(next.Messages), 200)
		received = append(received, next.Messages...)
		cursor = next.Cursor
		if !next.More {
			break
		}
	}
	require.Equal(t, snapshot.Messages, received)
	require.Empty(t, Page(snapshot, cursor).Messages)
}

func TestHistoryCursorResetsOnEditsTruncationOrSessionChange(t *testing.T) {
	original := Snapshot{Session: "one", Messages: []Message{{ID: "a", Text: "first"}, {ID: "b", Text: "second"}}}
	cursor := Page(original, "").Cursor
	edited := Snapshot{Session: "one", Messages: []Message{{ID: "a", Text: "edited"}, {ID: "b", Text: "second"}}}
	require.True(t, Page(edited, cursor).HistoryGap)
	require.True(t, Page(Snapshot{Session: "one", Messages: original.Messages[1:]}, cursor).HistoryGap)
	changed := Page(Snapshot{Session: "two", Messages: original.Messages}, cursor)
	require.True(t, changed.Reset)
	require.False(t, changed.HistoryGap)
	require.True(t, Page(original, "invalid").Reset)
}

func TestHistoryPagesBoundLargeUnicodeMessages(t *testing.T) {
	original := Snapshot{Session: "one", Messages: []Message{}}
	cursor := Page(original, "").Cursor
	large := strings.Repeat("Текст", (2<<20)/len("Текст")+1)
	original.Messages = []Message{{ID: "a", Text: large}, {ID: "b", Text: "after"}}
	first := Page(original, cursor)
	require.Len(t, first.Messages, 1)
	require.True(t, first.More)
	second := Page(original, first.Cursor)
	require.False(t, second.Reset)
	require.Equal(t, "after", second.Messages[0].Text)
}

// A runtime reader keeps only a transcript's recent tail: as the conversation grows its oldest messages leave the
// window. The client has them already, so that is no gap; an edit among the recent ones still is.
func TestHistoryCursorResumesAsTheOldestMessagesLeaveTheWindow(t *testing.T) {
	messages := func(from, to int) []Message {
		out := []Message{}
		for i := from; i < to; i++ {
			out = append(out, Message{ID: fmt.Sprint(i), Role: "assistant", Text: fmt.Sprint("reply ", i)})
		}
		return out
	}
	cursor := Page(Snapshot{Session: "one", Messages: messages(0, 120)}, "").Cursor

	next := Page(Snapshot{Session: "one", Messages: messages(30, 125)}, cursor)
	require.False(t, next.Reset)
	require.False(t, next.HistoryGap)
	require.Equal(t, messages(120, 125), next.Messages, "only what is new")

	again := Page(Snapshot{Session: "one", Messages: messages(40, 126)}, next.Cursor)
	require.False(t, again.HistoryGap)
	require.Equal(t, messages(125, 126), again.Messages)

	edited := messages(40, 126)
	edited[len(edited)-3].Text = "edited"
	require.True(t, Page(Snapshot{Session: "one", Messages: edited}, again.Cursor).HistoryGap, "an edit among the recent messages")
	require.True(t, Page(Snapshot{Session: "one", Messages: messages(126, 130)}, again.Cursor).HistoryGap, "the cursor's message is gone: messages may be missing")
}

// A cursor from a zmina before the window still resumes on the whole prefix.
func TestHistoryCursorWithoutWindowStillResumes(t *testing.T) {
	snapshot := Snapshot{Session: "one", Messages: []Message{{ID: "a", Text: "first"}, {ID: "b", Text: "second"}}}
	var c historyCursor
	raw, err := base64.RawURLEncoding.DecodeString(Page(snapshot, "").Cursor)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &c))
	c.Window = ""
	raw, _ = json.Marshal(c)
	snapshot.Messages = append(snapshot.Messages, Message{ID: "c", Text: "third"})
	next := Page(snapshot, base64.RawURLEncoding.EncodeToString(raw))
	require.False(t, next.Reset)
	require.Equal(t, "third", next.Messages[0].Text)
}

// A /compact zmina types shows as typed until Claude Code records it as a command, which takes the typed line away:
// a cursor on that line would read as edited history and reset the owner's view. The cursor stays before it.
func TestHistoryCursorStaysBeforeAProvisionalEnd(t *testing.T) {
	before := []Message{{ID: "a", Role: "assistant", Text: "reply"}}
	typed := append(append([]Message{}, before...), Message{ID: "typed", Role: "user", Text: "/compact", Provisional: true})
	first := Page(Snapshot{Session: "one", Messages: typed}, "")
	require.Equal(t, typed, first.Messages, "the typed line is shown")

	again := Page(Snapshot{Session: "one", Messages: typed}, first.Cursor)
	require.False(t, again.Reset)
	require.Equal(t, typed[1:], again.Messages, "sent again while it stands")

	recorded := append(append([]Message{}, before...), Message{ID: "cmd", Role: "event", Event: &Event{Kind: "command", Summary: "Context compacted"}})
	next := Page(Snapshot{Session: "one", Messages: recorded}, again.Cursor)
	require.False(t, next.Reset)
	require.False(t, next.HistoryGap)
	require.Equal(t, recorded[1:], next.Messages, "its replacement")

	more := append(recorded, Message{ID: "b", Role: "assistant", Text: "after"})
	last := Page(Snapshot{Session: "one", Messages: more}, next.Cursor)
	require.False(t, last.HistoryGap)
	require.Equal(t, more[2:], last.Messages)
}
