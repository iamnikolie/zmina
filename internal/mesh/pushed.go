package mesh

import (
	"errors"
	"fmt"
	"os"
)

// pushedFile is what the owner's node keeps of the crew files it sent: per node and crew, the sum of the file it last pushed there or
// last read there and confirmed (a push that was written or found already in place, a move that left both nodes on one file). It is a
// file of the mesh directory (pushed.json), never part of a crew's state, and nothing decides by it but which file the next push is
// based on.
const pushedFile = "pushed.json"

// PushedSum is the sum of the crew file this node last pushed to node, or last confirmed it held, "" when it has no record: the file
// the next push to that node is based on unless the owner says another (compare and swap).
func (n *Node) PushedSum(node, crew string) string {
	m := map[string]map[string]string{}
	if err := readJSON(n.path(pushedFile), &m); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ""
	}

	sum := m[node][crew]
	if !plainSum.MatchString(sum) {
		return ""
	}

	return sum
}

// MarkPushed records that node holds, as far as this node knows, the file of that sum for the crew.
func (n *Node) MarkPushed(node, crew, sum string) error {
	if !ValidName(node) || !plainCrew.MatchString(crew) || !plainSum.MatchString(sum) {
		return fmt.Errorf("pushed: %q %q %q is no node, crew and sum", node, crew, sum)
	}

	return n.locked(func() error {
		m := map[string]map[string]string{}
		if err := readJSON(n.path(pushedFile), &m); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A file that does not read is no record: a new one is made, and the next push to each node asks for --base.
			m = map[string]map[string]string{}
		}

		if m[node] == nil {
			m[node] = map[string]string{}
		}

		m[node][crew] = sum

		return writeJSON(n.path(pushedFile), m)
	})
}
