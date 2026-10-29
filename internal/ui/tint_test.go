package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTint(t *testing.T) {
	assert.Empty(t, Tint("", nil), "no project, no tint")
	assert.Equal(t, Tint("zmina", nil), Tint("zmina", nil), "stable")
	assert.Regexp(t, `^p[1-5]$`, Tint("zmina", nil))
	assert.Equal(t, "p3", Tint("zmina", map[string]string{"zmina": "p3"}), "a pinned tint wins")

	seen := map[string]bool{}
	for _, p := range []string{"zmina", "kreida", "linc", "agentic-os", "acme", "site", "a", "b"} {
		seen[Tint(p, nil)] = true
	}

	assert.Greater(t, len(seen), 2, "names spread over the tints")
}
