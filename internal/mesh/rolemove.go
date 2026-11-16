package mesh

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
)

// What a move of a role carries over the wire, in bytes. The move package holds the same numbers for the bundle and the small
// bodies; a test says they agree.
const (
	// RoleMoveBodyMax is the most a role/move request carries as its body (a check, a begin, a commit).
	RoleMoveBodyMax = 64 << 10
	// RoleTakeMax is the most a role/take request carries: the bundle of the work, and an answer that carries one.
	RoleTakeMax = 1 << 20
)

// roleMoveActions are the steps of role/move. check asks what the node holds, for the owner to compare the two ends before anything
// is done; begin opens the move at the node the role leaves; poll moves it on there and asks how far it is (the node the role goes to
// answers a poll with whether it took the work); commit ends it there; abort calls it off.
var roleMoveActions = map[string]bool{"check": true, "begin": true, "poll": true, "commit": true, "abort": true}

// plainMove is the id of a move: what zmina makes (mv- and twelve hex digits), never a path.
var plainMove = regexp.MustCompile(`^mv-[0-9a-f]{12}$`)

// roleOp reports an op that is a step of a move.
func roleOp(op string) bool { return op == "role/move" || op == "role/take" }

// rolePayloadMax is how many bytes the op's request may carry.
func rolePayloadMax(op string) int {
	if op == "role/take" {
		return RoleTakeMax
	}

	return RoleMoveBodyMax
}

// MoveCall is a step of a move as the node it is for receives it, after the mesh has checked that the owner signed it for this node
// and this session, and that its names and sizes are what they claim to be.
type MoveCall struct {
	// Crew is the crew's key as nodes address it; Role the role that moves.
	Crew, Role string
	// Action is the step of role/move (roleMoveActions); empty for role/take.
	Action string
	// Move is the move's id ("" for a check); Base the sum of the crew file the owner means the node to hold ("" for none).
	Move, Base string
	// Force is that the owner chose to drop what a move does not carry.
	Force bool
	// Payload is the step's body: JSON the node reads, a bundle for a take.
	Payload []byte
	// By is who asked, for the node's history and log: "mesh <owner node>".
	By string
}

// CheckMoveCall refuses a step of a move whose names, id, action, base or size are not what they claim to be: no path, no option, no
// command is a crew, a role or a move id. The mesh runs it before a node's service sees a request, and the owner's node before it
// does a step on itself, so a step done here is judged as one that came over the wire.
func CheckMoveCall(op string, c MoveCall) error {
	switch {
	case !plainCrew.MatchString(c.Crew):
		return fmt.Errorf("crew %q: a crew's name, no path", c.Crew)
	case !plainRole.MatchString(c.Role):
		return fmt.Errorf("role %q: a role's name (a copy, dev#2, does not move)", c.Role)
	case c.Base != "" && !plainSum.MatchString(c.Base):
		return errors.New("base: the hash of the crew file, 64 hex digits, or nothing")
	case op == "role/move" && !roleMoveActions[c.Action]:
		return fmt.Errorf("action %q: check, begin, poll, commit or abort", c.Action)
	case op == "role/take" && c.Action != "":
		return fmt.Errorf("a take has no action, not %q", c.Action)
	case (op == "role/take" || c.Action != "check") && !plainMove.MatchString(c.Move):
		return fmt.Errorf("move %q: the id of a move, mv- and twelve hex digits", c.Move)
	case c.Action == "check" && c.Move != "" && !plainMove.MatchString(c.Move):
		return fmt.Errorf("move %q: the id of a move, mv- and twelve hex digits", c.Move)
	case op == "role/take" && len(c.Payload) == 0:
		return errors.New("a take carries the work: this one is empty")
	case len(c.Payload) > rolePayloadMax(op):
		return fmt.Errorf("%s carries at most %d bytes: this one has %d", op, rolePayloadMax(op), len(c.Payload))
	}

	return nil
}

// serveRoleOp is the node's side of a step of a move, once the request is known to be the owner's, bound to this node: it checks what
// the mesh can check without the crew (this connection said crew_sync, the names, the id, the sizes) and hands the rest to the
// node's service. A name or an id that is not plain never reaches it, so that no path, no option and no command can.
func (n *Node) serveRoleOp(c *conn, op string, req *TerminalRequest, s *TerminalServices) error {
	fail := func(err error) error { _ = c.send(wire{Op: "err", Err: err.Error()}); return err }

	service, result := s.RoleMove, "role/move-result"
	if op == "role/take" {
		service, result = s.RoleTake, "role/take-result"
	}

	call := MoveCall{Crew: req.Crew, Role: req.Role, Action: req.Action, Move: req.Move, Base: req.Base, Force: req.Force,
		Payload: req.Payload, By: "mesh " + c.peer.Name}

	switch {
	case !c.peer.CrewSync:
		return fail(fmt.Errorf("%s needs both nodes to say crew_sync: update zmina on the owner's node", op))
	case service == nil:
		return fail(fmt.Errorf("%s unavailable; update zmina on this node", op))
	}

	if err := CheckMoveCall(op, call); err != nil {
		return fail(err)
	}

	data, err := service(call)
	if err != nil {
		return fail(err)
	}

	if len(data) > RoleTakeMax {
		return fail(fmt.Errorf("the answer is %d bytes: at most %d", len(data), RoleTakeMax))
	}

	sum := sha256.Sum256(data)

	return c.send(wire{Op: result, Data: data, ResultSig: n.resultSig(c, result, roleResultStatus, sum[:])})
}

// roleResultStatus is the status a node signs with a step of a move it answers: the step's answer is the body, which the mesh does
// not read, so it is the hash of the body that is signed with it.
const roleResultStatus = "ok"

// RoleMove sends the owner's step of a move to a node and returns its answer: the owner's signature over the whole request, bound to
// that node and to the session, so that a relay that carries it can neither alter it nor send it again. It is refused before anything
// is sent when the peer did not say crew_sync in this session, so an older zmina is never sent an op it does not know.
func (n *Node) RoleMove(ctx context.Context, t Transport, node string, req TerminalRequest) ([]byte, error) {
	return n.roleStep(ctx, t, node, "role/move", "role/move-result", RoleMoveBodyMax, req)
}

// RoleTake sends the work of a move to the node the role goes to, in the same form as RoleMove, and returns what it says.
func (n *Node) RoleTake(ctx context.Context, t Transport, node string, req TerminalRequest) ([]byte, error) {
	return n.roleStep(ctx, t, node, "role/take", "role/take-result", RoleTakeMax, req)
}

func (n *Node) roleStep(ctx context.Context, t Transport, node, op, result string, max int, req TerminalRequest) ([]byte, error) {
	if len(req.Payload) > max {
		return nil, fmt.Errorf("%s carries at most %d bytes, not %d", op, max, len(req.Payload))
	}

	rw, c, err := n.terminalConnect(ctx, t, node)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rw.Close() }()

	if !c.peer.CrewSync || !c.peer.TerminalV2 {
		return nil, fmt.Errorf("%s did not say crew_sync in this session: a zmina from before zmina move; update it, then zmina node sync", node)
	}

	req.To = node

	if err = n.terminalRequest(c, op, req); err != nil {
		return nil, err
	}

	w, err := c.expect(result)
	if err != nil {
		return nil, err
	}

	if len(w.Data) > RoleTakeMax {
		return nil, fmt.Errorf("%s: its answer is %d bytes: at most %d", node, len(w.Data), RoleTakeMax)
	}

	// The answer is the node's own, signed for this session: a relay cannot answer a step it dropped, nor change what the node said of it.
	sum := sha256.Sum256(w.Data)
	if err := c.verifyResult(w, roleResultStatus, sum[:]); err != nil {
		return nil, fmt.Errorf("%s: %w", node, err)
	}

	return w.Data, nil
}
