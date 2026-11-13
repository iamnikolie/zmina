package shift

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/registry"
)

// A role's turn is told, in the usage accounting, from the text zmina gave it. The texts are the ones this package
// writes (a tag, a numbered message, a doorbell), so a change to how they are written cannot leave the accounting
// reading them as the owner's.
func TestDeliveredTextsAreNotTheOwner(t *testing.T) {
	tag := tagMark("abcdefghijkl") + " "
	body := inbox.IDOf("bell dev\x00" + numbered([]string{"[dev] a", "[ops] b"}))
	mark := &crossMark{text: crossOpen + "14:02, before your last line on T-1 (14:05)" + crossClose}

	for _, c := range []struct {
		name, text, want string
	}{
		{"a line", tag + "[zmina T-1] Read handoff packet", registry.TriggerHandoff},
		{"a line of a role", tag + "[dispatcher] look at T-2", registry.TriggerDelivery},
		{"a line behind its mark", tag + mark.withMark("[dev] done"), registry.TriggerDelivery},
		{"joined notices", tag + numbered([]string{"[zmina] dev idle", "[zmina] ops stuck"}), registry.TriggerNotice},
		{"joined lines", tag + numbered([]string{"[dispatcher] look at T-2", "[zmina] dev idle"}), registry.TriggerDelivery},
		{"joined lines behind marks", tag + numberedMarked([]string{"[dev] a", "[ops] b"}, []*crossMark{nil, mark}),
			registry.TriggerDelivery},
		{"joined lines with the owner's chat line", tag + numbered([]string{"[zmina] dev idle",
			"[zmina chat 0123456789abcdef] go on"}), registry.TriggerOwner},
		{"a doorbell", tag + bellText(3, []string{"dev", "ops"}, []string{"T-1"}, 0, body), registry.TriggerDelivery},
		{"a doorbell for one line", tag + bellText(1, nil, nil, 0, body), registry.TriggerDelivery},
		{"a doorbell with crossed lines", tag + bellText(2, []string{"dev"}, nil, 1, body), registry.TriggerDelivery},
		{"a doorbell with only crossed lines", tag + bellText(2, nil, nil, 2, body), registry.TriggerDelivery},
		{"a doorbell typed", bellText(2, []string{"dev"}, []string{"T-1"}, 0, body), registry.TriggerDelivery},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := registry.TextTrigger(c.text)
			assert.Equal(t, c.want, got, c.text)
		})
	}

	assert.True(t, IsBell(bellText(3, []string{"dev"}, nil, 0, body)))
}
