package mesh

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// CrewHealth is how one crew of a node stands, as the node tells its peers in its signed hello. Ages are seconds on
// the telling node's clock, so a skewed clock does not change them.
type CrewHealth struct {
	Crew string `json:"crew"`
	// TickAge is how long ago its ticker last beat; negative when it never did.
	TickAge int64 `json:"tick_age"`
	// Stopped is how long its shift has been stopped; 0 while it is not.
	Stopped int64 `json:"stopped,omitempty"`
	Retired bool  `json:"retired,omitempty"`
	// MovedTo is the crew the owner says it went on as (zmina retire --moved-to).
	MovedTo string `json:"moved_to,omitempty"`
}

// tickerGone is how long a ticker may be silent before a crew counts as not running: past a slow beat or two.
const tickerGone = 5 * time.Minute

// Running reports a crew whose dispatcher will get a line: its ticker beats, its shift is not stopped or retired.
func (h CrewHealth) Running() bool {
	return !h.Retired && h.Stopped == 0 && h.TickAge >= 0 && time.Duration(h.TickAge)*time.Second <= tickerGone
}

// Why says in words why it is not running; "running" when it is.
func (h CrewHealth) Why() string {
	switch {
	case h.Retired && h.MovedTo != "":
		return "retired, moved to " + h.MovedTo
	case h.Retired:
		return "retired"
	case h.Stopped > 0:
		return "stopped " + spanWords(h.Stopped) + " ago"
	case h.TickAge < 0:
		return "its ticker never beat"
	case !h.Running():
		return "no ticker beat for " + spanWords(h.TickAge)
	}

	return "running"
}

func spanWords(seconds int64) string {
	d := time.Duration(seconds) * time.Second

	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}

	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// crewsMax is how many crews of one node are told or kept.
const crewsMax = 64

// crewData is this node's crews as they go in a hello.
func (n *Node) crewData() []byte {
	if n.Health == nil {
		return nil
	}

	crews := n.Health()
	if len(crews) > crewsMax {
		crews = crews[:crewsMax]
	}

	data, err := json.Marshal(crews)
	if err != nil {
		return nil
	}

	return data
}

// crewsMessage is what a node signs over its crews' health. It signs the bytes it sends, so a field a newer zmina
// adds still verifies here.
func crewsMessage(side string, nonce, crews []byte) []byte {
	return []byte(fmt.Sprintf("zmina-crews-v1\x00%s\x00%x\x00%s", side, nonce, crews))
}

// plainCrew and plainAddress are what a crew name and a moved_to told by a peer may be: such words reach a
// dispatcher's boot line and a sender's terminal, so they carry nothing but a name.
var (
	plainCrew    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	plainAddress = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// readCrewHealth reads what a peer signed: nil when it does not read, a crew with no plain name left out.
func readCrewHealth(data []byte) []CrewHealth {
	if len(data) == 0 {
		return nil
	}

	var in []CrewHealth
	if err := json.Unmarshal(data, &in); err != nil {
		return nil
	}

	out := make([]CrewHealth, 0, len(in))

	for _, h := range in {
		if len(out) == crewsMax {
			break
		}

		if !plainCrew.MatchString(h.Crew) {
			continue
		}

		if !plainAddress.MatchString(h.MovedTo) {
			h.MovedTo = ""
		}

		out = append(out, h)
	}

	return out
}
