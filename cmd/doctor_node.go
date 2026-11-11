package cmd

import (
	"errors"
	"time"

	"github.com/iamnikolie/zmina/internal/mesh"
)

// checkNode is doctor's look at this machine's node: where its certificate stands (a warning from 30 days before it
// runs out, a failure after, a warning while it has no expiry), whether its key was revoked, the revocation list it
// holds and, on the owner's node, the peers' certificates that need renewing. A machine that is not a node says
// nothing.
func (a *app) checkNode(add func(name, level, format string, args ...any)) {
	me, err := openNode()

	switch {
	case errors.Is(err, mesh.ErrNoNode):
		return
	case err != nil:
		add("node", "warn", "%v", err)

		return
	}

	now := time.Now()

	level, text := certLine(me.Cert, now)
	add("node", level, "%s: certificate %s", me.Name, text)

	list, err := me.Revocations()
	if err != nil {
		add("node", "warn", "the revocation list does not read: %v", err)

		return
	}

	if list.Version > 0 {
		add("node", "ok", "revocation list version %d: %d keys revoked", list.Version, len(list.Revoked))
	}

	if list.Has(me.Cert.Key) {
		add("node", "fail", "this node's key was revoked by its owner: it opens no session")
	}

	if !me.IsOwner() {
		return
	}

	peers, _ := me.Peers()

	for _, p := range peers {
		if p.Cert == nil {
			continue
		}

		if state, _ := p.Cert.State(now); state == mesh.CertExpiring || state == mesh.CertExpired {
			level, text := certLine(*p.Cert, now)
			add("node", level, "%s: certificate %s", p.Name, text)
		}
	}
}
