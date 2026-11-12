package shift

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/peer"
)

// zmina send without --when-idle types straight into the pane when the line cannot be posted. It types where the
// queue would, and nowhere else: a dialog, a draft, a screen whose input line cannot be read, a role between its start
// and its boot's end, and a line typed before and not landed yet all end in an error that names the way out.
func TestSendDoesNotTypeIntoADialogOrOverADraft(t *testing.T) {
	cases := map[string]struct {
		set  func(f fix)
		want string
	}{
		"a draft of the owner": {
			set:  func(f fix) { f.tm.drafts["designer"] = "half a thought of the owner" },
			want: "holds a draft",
		},
		"a dialog": {
			set:  func(f fix) { f.status("designer", "waiting", *f.now) },
			want: "waits on a dialog",
		},
		"an input line that is not on the screen": {
			set:  func(f fix) { f.tm.drafts["designer"] = "-" },
			want: "cannot be read",
		},
		"a role in the middle of a turn": {
			set:  func(f fix) { f.status("designer", "busy", *f.now) },
			want: "not at its prompt",
		},
		"a line typed before that has not landed": {
			set:  func(f fix) { f.s.State.Role("designer").Sent = &Sent{Line: "earlier", At: *f.now} },
			want: "has not landed",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := typedFixture(t, crewYAML, "designer")
			tc.set(f)
			draft := f.tm.drafts["designer"]

			err := f.s.Send("designer", "look at T-1")

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "--when-idle", "the error names the way out")
			assert.Empty(t, f.tm.typed["designer"], "nothing typed")
			assert.Empty(t, f.tm.keys["designer"], "no key sent")
			assert.Equal(t, draft, f.tm.drafts["designer"], "what sat in the input line stays")
			assert.False(t, f.ev.has("designer", "sent"))
		})
	}

	t.Run("an idle role with an empty input line", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")

		require.NoError(t, f.s.Send("designer", "look at T-1"))
		assert.Equal(t, []string{"look at T-1"}, f.tm.typed["designer"])
		assert.True(t, f.ev.has("designer", "sent"))
	})

	t.Run("what zmina itself left of a broken line is not a draft", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		f.s.State.Role("designer").Partial = "[zmina T-1] read the packet and tell me"
		f.tm.drafts["designer"] = "[zmina T-1] read the packet"

		require.NoError(t, f.s.Send("designer", "look at T-1"))
		assert.Equal(t, []string{"C-e", "C-u"}, f.tm.keys["designer"], "its own start is cleared first")
		assert.Equal(t, []string{"look at T-1"}, f.tm.typed["designer"])
	})
}

// A line posted to the inbox socket needs no empty input line; one that cannot be posted is typed, and then with the
// same guards.
func TestSendThatCannotBePostedIsTypedOnlyIntoAnEmptyInputLine(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	p.err = fmt.Errorf("%w: dial: no such file", peer.ErrUnavailable)
	f.tm.drafts["designer"] = "half a thought of the owner"

	err := f.s.Send("designer", "look at T-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--when-idle")
	assert.Empty(t, f.tm.typed["designer"])
	assert.Equal(t, "half a thought of the owner", f.tm.drafts["designer"])

	delete(f.tm.drafts, "designer")
	require.NoError(t, f.s.Send("designer", "look at T-1"))
	assert.Equal(t, []string{"look at T-1"}, f.tm.typed["designer"])
}

// The dispatcher's typed path agrees with the queue's: an input line that cannot be read is not an empty one.
func TestDispatcherLineIsNotTypedWhenItsInputLineCannotBeRead(t *testing.T) {
	f := fixtureWith(t, dispatcherOnly)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	f.tm.drafts["dispatcher"] = "-"

	_, err := f.s.Note("mr-watch", "!35 merged", false)
	require.NoError(t, err)

	assert.Empty(t, f.s.deliver(*f.now))
	assert.Empty(t, f.tm.typed["dispatcher"], "nothing typed into a screen with no input line")
	assert.Equal(t, []string{"mr-watch: !35 merged"}, f.s.State.Signals, "it waits")

	delete(f.tm.drafts, "dispatcher")
	assert.NotEmpty(t, f.s.deliver(*f.now))
	assert.Len(t, f.tm.typed["dispatcher"], 1)
}
