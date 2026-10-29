package ui

import (
	"fmt"
	"hash/fnv"
)

// Tints is how many project tints the page has: --p1 … --p5.
const Tints = 5

// Tint is a project's tint on the page, p1 … p5: the one its crew file pins (projects: <name>: tint:), else one
// picked by the FNV-1a hash of its name. A work without a project has none.
func Tint(project string, pinned map[string]string) string {
	if project == "" {
		return ""
	}

	if p, ok := pinned[project]; ok && p != "" {
		return p
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(project))

	return fmt.Sprintf("p%d", h.Sum32()%Tints+1)
}
