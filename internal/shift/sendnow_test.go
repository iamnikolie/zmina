package shift

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SendNow gives a line at once or not at all, and queues nothing: a line that waits may be read long after what it answers is over. It is
// refused, as a NotNow that says why, when lines wait for the role, when it could not be typed (the same guards as send), and then
// nothing is typed, queued or logged.
func TestSendNowGivesALineAtOnceOrNotAtAll(t *testing.T) {
	cases := map[string]struct {
		set     func(t *testing.T, f fix)
		want    string
		partial bool
	}{
		"a line waits ahead of it": {
			set:  func(t *testing.T, f fix) { require.NoError(t, f.s.QueueFrom("designer", "an earlier line", Facts{})) },
			want: "lines wait for it ahead of this one",
		},
		"a line failed and waits": {
			set:  func(_ *testing.T, f fix) { f.s.State.Role("designer").Failed = []Failed{{Line: "an earlier line"}} },
			want: "lines wait for it ahead of this one",
		},
		"a draft": {
			set:     func(_ *testing.T, f fix) { f.tm.drafts["designer"] = "half a thought of the owner" },
			want:    "its input line holds a draft",
			partial: true,
		},
		"a dialog": {
			set:     func(_ *testing.T, f fix) { f.status("designer", "waiting", *f.now) },
			want:    "it waits on a dialog",
			partial: true,
		},
		"a role in the middle of a turn": {
			set:     func(_ *testing.T, f fix) { f.status("designer", "busy", *f.now) },
			want:    "it is not at its prompt (busy, or its start is not over)",
			partial: true,
		},
		"a line typed before that has not landed": {
			set:     func(_ *testing.T, f fix) { f.s.State.Role("designer").Sent = &Sent{Line: "earlier", At: *f.now} },
			want:    "a line typed into it has not landed yet",
			partial: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := typedFixture(t, crewYAML, "designer")
			tc.set(t, f)

			waiting := len(f.s.State.Role("designer").Inbox) + len(f.s.State.Role("designer").Failed)

			err := f.s.SendNow("designer", "[zmina] the owner's answer", "plugin pager")

			var notNow *NotNow
			require.ErrorAs(t, err, &notNow)
			assert.Equal(t, "designer", notNow.Role)
			assert.Equal(t, tc.want, notNow.Why)
			assert.Equal(t, "designer cannot take it just now: "+tc.want, err.Error())
			assert.Equal(t, tc.partial, errors.Is(err, ErrPartial), "what the look closed is saved with the refusal")

			assert.Empty(t, f.tm.typed["designer"], "nothing typed")
			assert.Empty(t, f.tm.keys["designer"], "no key sent")
			assert.Equal(t, waiting, len(f.s.State.Role("designer").Inbox)+len(f.s.State.Role("designer").Failed), "and nothing queued")
			assert.False(t, f.ev.has("designer", "sent"))
		})
	}

	t.Run("an idle role with an empty input line", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")

		require.NoError(t, f.s.SendNow("designer", "[zmina] the owner's answer", "plugin pager"))
		assert.Equal(t, []string{"[zmina] the owner's answer"}, f.tm.typed["designer"])
		assert.Empty(t, f.s.State.Role("designer").Inbox, "typed, never queued")
		assert.True(t, f.ev.has("designer", "sent"))
	})

	t.Run("a role that is not up", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)

		err := f.s.SendNow("designer", "[zmina] the owner's answer", "plugin pager")
		require.ErrorIs(t, err, ErrNotUp)
		assert.Empty(t, f.tm.typed["designer"])
	})

	t.Run("a role that is no role", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")

		require.Error(t, f.s.SendNow("nobody", "x", "plugin pager"))
	})
}
