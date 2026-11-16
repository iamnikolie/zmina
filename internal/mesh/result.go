package mesh

import (
	"crypto/ed25519"
	"errors"
	"fmt"
)

// resultMessage is what a node signs of its answer to an op of boundCaller: the answer's op, the challenge of the session the request
// came in, the status, and a hash of what the answer carries besides it. The caller checks it with the key the node proved in its
// hello, so an answer cannot be made up or changed by a relay that carries the session, nor taken from another session.
func resultMessage(op string, challenge []byte, status string, digest []byte) []byte {
	return []byte(fmt.Sprintf("zmina-result\x00%s\x00%x\x00%s\x00%x", op, challenge, status, digest))
}

// resultSig signs the answer this node gives on c.
func (n *Node) resultSig(c *conn, op, status string, digest []byte) []byte {
	return ed25519.Sign(n.Key, resultMessage(op, c.challenge, status, digest))
}

// verifyResult checks an answer w the far node gave on c against the status and hash of what it carries.
func (c *conn) verifyResult(w wire, status string, digest []byte) error {
	if len(c.serverKey) != ed25519.PublicKeySize || !ed25519.Verify(c.serverKey, resultMessage(w.Op, c.challenge, status, digest), w.ResultSig) {
		return errors.New("its answer is not signed by it for this session, so it is not taken for its answer (a relay, or a zmina older " +
			"than the signed answers)")
	}

	return nil
}
