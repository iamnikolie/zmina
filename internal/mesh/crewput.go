package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// CrewFileMax is the most bytes of a crew file a push carries or a conflict returns: a crew file is hundreds of lines,
// and a node takes nothing bigger as the file that decides what it runs.
const CrewFileMax = 256 << 10

// Statuses of a push (CrewPutResult.Status).
const (
	// PutDone: the file was written (or the crew made), and kept in the crew file's history.
	PutDone = "put"
	// PutSame: the file there is already this file; nothing was written.
	PutSame = "same"
	// PutConflict: the file there is not the one the push was based on; nothing was written.
	PutConflict = "conflict"
)

// CrewPutResult is what a node says of a push to it.
type CrewPutResult struct {
	Status string `json:"status"`
	// Sum is the hash of the file there after the push (PutDone, PutSame), or now (PutConflict); Was the hash of the file
	// a PutDone replaced, "" when it made the crew.
	Sum string `json:"sum,omitempty"`
	Was string `json:"was,omitempty"`
	// Current is, on a conflict, the file there, for the owner's diff; empty when it is over CrewFileMax.
	Current []byte `json:"current,omitempty"`
	// Line is what the node says was done, in a line; Warnings are what loading the file there said of it.
	Line     string   `json:"line,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// CrewConflict is the error of a push whose base was not the file on the target: Have is the sum of the file there ("" when
// it has none), Base the sum the push was based on, Current the file there when it was small enough to be sent.
type CrewConflict struct {
	Node    string
	Have    string
	Base    string
	Current []byte
}

func (e *CrewConflict) Error() string {
	base := e.Base
	if base == "" {
		base = "none (the push was based on no file there)"
	}

	have := e.Have
	if have == "" {
		have = "none"
	}

	return fmt.Sprintf("%s holds a different file from the one this push was based on: its file is %s, the push was based on %s",
		e.Node, short(have), short(base))
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}

	return sum
}

// serveCrewPut is the target's side of a push, once the request is known to be the owner's, bound to this node: it checks
// what the mesh can check without the crew (this connection said crew_sync, the file's size, the names) and hands the
// rest to the node's service.
func (n *Node) serveCrewPut(c *conn, req *TerminalRequest, s *TerminalServices) error {
	fail := func(err error) error { _ = c.send(wire{Op: "err", Err: err.Error()}); return err }

	switch {
	case !c.peer.CrewSync:
		return fail(errors.New("crew/put needs both nodes to say crew_sync: update zmina on the owner's node"))
	case s.CrewPut == nil:
		return fail(errors.New("crew push unavailable; update zmina on this node"))
	case !plainCrew.MatchString(req.Crew):
		return fail(fmt.Errorf("crew %q: a crew's name, no path", req.Crew))
	case len(req.Payload) == 0:
		return fail(errors.New("a push carries a crew file: this one is empty"))
	case len(req.Payload) > CrewFileMax:
		return fail(fmt.Errorf("the crew file is %d bytes: a node takes at most %d", len(req.Payload), CrewFileMax))
	case req.Base != "" && !plainSum.MatchString(req.Base):
		return fail(errors.New("base: the hash of the file replaced, 64 hex digits, or nothing"))
	}

	res, err := s.CrewPut(req.Crew, req.Payload, req.Base, req.Create, "mesh "+c.peer.Name)
	if err != nil {
		return fail(err)
	}

	if len(res.Current) > CrewFileMax {
		res.Current = nil
	}

	data, err := json.Marshal(res)
	if err != nil {
		return fail(err)
	}

	return c.send(wire{Op: "crew/put-result", Data: data})
}

// CrewPut sends the owner's crew file to a peer, in place of the one it has, and returns what the peer says. The request is
// the owner's signature over the whole request (the file included) bound to that node, so a relay that carries it cannot
// alter it. It is refused before anything is sent when the peer did not say crew_sync in this session, so an older zmina
// is never sent an op it does not know. A file there that is not the one the push was based on comes back as a
// *CrewConflict.
func (n *Node) CrewPut(ctx context.Context, t Transport, node string, req TerminalRequest) (CrewPutResult, error) {
	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return CrewPutResult{}, err
	}

	defer func() { _ = rw.Close() }()

	if !c.peer.CrewSync || !c.peer.TerminalV2 {
		return CrewPutResult{}, fmt.Errorf("%s did not say crew_sync in this session: a zmina from before crew push; update it, "+
			"then zmina node sync", node)
	}

	if len(req.Payload) == 0 || len(req.Payload) > CrewFileMax {
		return CrewPutResult{}, fmt.Errorf("a crew file to push is 1 to %d bytes, not %d", CrewFileMax, len(req.Payload))
	}

	req.To = node

	if err = n.terminalRequest(c, "crew/put", req); err != nil {
		return CrewPutResult{}, err
	}

	w, err := c.expect("crew/put-result")
	if err != nil {
		return CrewPutResult{}, err
	}

	var res CrewPutResult
	if err := json.Unmarshal(w.Data, &res); err != nil {
		return CrewPutResult{}, fmt.Errorf("%s: its answer to the push does not read: %w", node, err)
	}

	if res.Status == PutConflict {
		return res, &CrewConflict{Node: node, Have: res.Sum, Base: req.Base, Current: res.Current}
	}

	return res, nil
}
