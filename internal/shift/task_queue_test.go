package shift

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClosedPacketCannotBeRetriedOrPressEnterOnNewWork(t *testing.T) {
	for _, ownDraft := range []bool{true, false} {
		t.Run(map[bool]string{true: "own draft", false: "human draft"}[ownDraft], func(t *testing.T) {
			f := acceptanceFixture(t)
			line := f.tk.list[0].Delivery.Line
			rs := f.s.State.Role("designer")
			rs.Sent = &Sent{Line: line, At: *f.now}
			rs.Failed = []Failed{{Line: line, Error: "unconfirmed"}, {Line: "ordinary failure", Error: "refused"}}
			rs.Attempts = 2
			f.tk.list[0].State = task.Done
			draft := "Human is composing"
			if ownDraft {
				draft = line
			}
			f.tm.drafts["designer"] = draft
			require.NoError(t, f.s.Queue("designer", "next note"))
			f.status("designer", "busy", *f.now)
			*f.now = f.now.Add(10 * time.Minute)
			assert.Empty(t, f.s.FlushInbox("designer"))
			assert.Equal(t, []string{"next note"}, rs.Inbox)
			assert.Zero(t, rs.Attempts)
			require.Len(t, rs.Failed, 1)
			assert.Equal(t, "ordinary failure", rs.Failed[0].Line)
			assert.Nil(t, rs.Sent)
			assert.Empty(t, f.tm.typed["designer"])
			assert.Empty(t, f.tm.keys["designer"])
			assert.Equal(t, draft, f.tm.drafts["designer"])
			require.Len(t, f.s.State.Notes, 1)
			assert.Contains(t, f.s.State.Notes[0], "input left untouched")
		})
	}
}

func TestTaskPacketPruningPreservesUnknownTasksAndOrdinaryMessages(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "unreadable"}[corrupt], func(t *testing.T) {
			f := acceptanceFixture(t)
			old := f.tk.list[0].Delivery.Line
			f.tk.list[0].State = task.Done
			store := &task.Store{Dir: t.TempDir()}
			require.NoError(t, store.Put(f.tk.list[0]))
			if corrupt {
				require.NoError(t, os.WriteFile(filepath.Join(store.Dir, "T-2.json"), []byte("broken"), 0600))
			}
			f.s.Tasks = store
			unknown := "[zmina T-2] Read handoff packet unknown.json"
			ordinary := "Task T-1 is done; review next change"
			f.s.State.Role("designer").Inbox = []string{old, unknown, ordinary}
			f.s.pruneTaskInbox("designer")
			assert.Equal(t, []string{unknown, ordinary}, f.s.State.Role("designer").Inbox)
		})
	}
}

func TestSupersededAndReassignedPacketsArePruned(t *testing.T) {
	for _, change := range []string{"superseded", "reassigned"} {
		t.Run(change, func(t *testing.T) {
			f := acceptanceFixture(t)
			if change == "superseded" {
				f.tk.list[0].Delivery.Line = "new packet"
			} else {
				f.tk.list[0].Role = "harvester"
			}
			f.s.pruneTaskInbox("designer")
			assert.Empty(t, f.s.State.Role("designer").Inbox)
		})
	}
}

func TestPruningPreservesSharedPrefixAndEditedOwnerDrafts(t *testing.T) {
	for _, kind := range []string{"current packet plus owner text", "former packet plus owner text", "multiline owner edit", "truncated prefix", "exact visible former packet", "unreadable prompt", "prompt read error"} {
		t.Run(kind, func(t *testing.T) {
			f := acceptanceFixture(t)
			former := f.tk.list[0].Delivery.Line
			rs := f.s.State.Role("designer")
			rs.Sent = &Sent{Line: former, At: *f.now}
			f.tk.list[0].Note = "Current review scope"
			*f.now = f.now.Add(time.Second)
			f.tk.list[0].Updated = f.now.UTC()
			_, err := f.s.HandTask("designer", f.tk.list[0])
			require.NoError(t, err)
			current := f.tk.list[0].Delivery.Line
			require.NotEqual(t, former, current)
			require.Equal(t, former[:40], current[:40])
			draft := current + " Also check the migration before proceeding."
			switch kind {
			case "former packet plus owner text":
				draft = former + " Keep my additional constraint."
			case "multiline owner edit":
				draft = former + "\nKeep my additional constraint."
			case "truncated prefix":
				draft = former[:40]
			case "exact visible former packet":
				draft = former // Prompt exposes only the first visible line.
			case "unreadable prompt":
				draft = "-"
			}
			f.tm.drafts["designer"] = draft
			if kind == "prompt read error" {
				f.s.Tmux = unreadableTaskInputMux{f.tm}
			}
			f.s.pruneTaskInbox("designer")
			assert.Equal(t, draft, f.tm.drafts["designer"])
			assert.Empty(t, f.tm.keys["designer"], "pruning has no authority to edit visible input")
			assert.Nil(t, rs.Sent)
			assert.Equal(t, []string{current}, rs.Inbox)
			require.Len(t, f.s.State.Notes, 1)
			assert.Contains(t, f.s.State.Notes[0], "input left untouched")
			assert.Contains(t, f.s.State.Notes[0], "zmina --crew "+shellWord(f.s.Crew.Path)+" peek 'designer'")
			f.status("designer", "idle", *f.now)
			rs.Started = f.now.Add(-time.Hour)
			for i := 0; i < 2; i++ {
				assert.Empty(t, f.s.FlushInbox("designer"))
			}
			assert.Empty(t, f.tm.typed["designer"], "retiring Sent must not append queued work to the preserved draft")
			assert.Empty(t, f.tm.keys["designer"])
			assert.Equal(t, draft, f.tm.drafts["designer"])
			assert.Equal(t, []string{current}, rs.Inbox)
			assert.Len(t, f.s.State.Notes, 1, "inspection warning is not repeated each tick")
			f.tm.drafts["designer"] = "" // The owner explicitly clears or sends their draft.
			f.s.Tmux = f.tm
			assert.Empty(t, f.s.currentNotes(), "resolved inspection warning does not become stale noise")
			assert.NotEmpty(t, f.s.FlushInbox("designer"))
			assert.Equal(t, []string{current}, f.tm.typed["designer"])
		})
	}
}

func TestIdleInboxDeliveryPreservesUnrelatedDraft(t *testing.T) {
	f := acceptanceFixture(t)
	f.status("designer", "idle", *f.now)
	f.s.State.Role("designer").Started = f.now.Add(-time.Hour)
	f.tm.drafts["designer"] = "Human is still composing"
	assert.Empty(t, f.s.FlushInbox("designer"))
	assert.Empty(t, f.tm.typed["designer"])
	assert.Empty(t, f.tm.keys["designer"])
	assert.Equal(t, "Human is still composing", f.tm.drafts["designer"])
	assert.Len(t, f.s.State.Role("designer").Inbox, 1)
}

// Unknown screen state cannot authorize input mutation or delivery.
type unreadableTaskInputMux struct{ *fakeTmux }

func (unreadableTaskInputMux) Prompt(string) (string, bool, error) {
	return "", false, errors.New("screen unavailable")
}

func TestPruningRetiresEmptyInputWithoutInspectionNoise(t *testing.T) {
	f := acceptanceFixture(t)
	f.s.State.Role("designer").Sent = &Sent{Line: f.tk.list[0].Delivery.Line, At: *f.now}
	f.tk.list[0].State = task.Done
	f.s.pruneTaskInbox("designer")
	assert.Nil(t, f.s.State.Role("designer").Sent)
	assert.Empty(t, f.s.State.Role("designer").Inbox)
	assert.Empty(t, f.s.State.Notes)
	assert.Empty(t, f.tm.keys["designer"])
	assert.Empty(t, f.tm.typed["designer"])
}
