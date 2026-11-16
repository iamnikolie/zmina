package crew

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenMove is a move that is open on this node: its journal, when the role is leaving this node, or the owner's record of it, when this
// node drives it.
type OpenMove struct {
	ID, Role, To string
	// Driven says it is the owner's record (moves/<role>.drive.json), not a journal.
	Driven bool
}

// OpenMoves lists the moves that are open on this node, by role. A file that does not read still counts: a move may be open, and the
// crew file is not changed until someone has looked.
func (l Layout) OpenMoves() []OpenMove {
	matches, _ := filepath.Glob(filepath.Join(l.Moves(), "*.json"))

	var out []OpenMove

	for _, m := range matches {
		base := strings.TrimSuffix(filepath.Base(m), ".json")

		// A take's marker is no journal and no record: it is the target's, and a crew file may be sent to it to mend the take.
		if strings.HasSuffix(base, ".taking") {
			continue
		}

		driven := strings.HasSuffix(base, ".drive")
		role := strings.TrimSuffix(base, ".drive")

		var j struct {
			ID string `json:"id"`
			To string `json:"to"`
		}

		if raw, err := os.ReadFile(m); err == nil {
			_ = json.Unmarshal(raw, &j)
		}

		out = append(out, OpenMove{ID: j.ID, Role: role, To: j.To, Driven: driven})
	}

	return out
}

// RefuseWhileMoving is the refusal of a change to the crew file at path while a move is open on this node: the move writes the file
// at its end and compares it on both nodes, so a push or a seat edit made meanwhile would leave the move unable to end. The message
// names the move and the way out.
func RefuseWhileMoving(path string) error {
	open := Layout{Dir: filepath.Dir(path)}.OpenMoves()
	if len(open) == 0 {
		return nil
	}

	m := open[0]

	who := fmt.Sprintf("a move of %s to %s is open on this node (%s)", m.Role, orWord(m.To, "another node"), orWord(m.ID, "its record does not read"))
	if m.Driven {
		who = fmt.Sprintf("this node drives the move %s of %s to %s", orWord(m.ID, "whose record does not read"), m.Role, orWord(m.To, "another node"))
	}

	to := orWord(m.To, "<node>")

	return fmt.Errorf("%s: the crew file is not changed until it is over, because the move writes it at its end and compares it on both nodes; "+
		"zmina move %s %s --resume %s finishes it, --abort %s calls it off; then change the file again", who, m.Role, to, orWord(m.ID, "<id>"),
		orWord(m.ID, "<id>"))
}

func orWord(s, word string) string {
	if s == "" {
		return word
	}

	return s
}
