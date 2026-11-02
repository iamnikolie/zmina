package chat

import (
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
