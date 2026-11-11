package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

const certDay = 24 * time.Hour

// daysFlag adds --days, a certificate's life: 365 unless the owner says another, 0 for none.
func daysFlag(c *cobra.Command, days *int) {
	c.Flags().IntVar(days, "days", int(mesh.DefaultCertLife/certDay), "how many days the certificate counts; 0: no expiry, "+
		"which every zmina reads (for a mesh where some node does not read expiries yet)")
}

// certLife is --days as a duration; a negative number is refused.
func certLife(days int) (time.Duration, error) {
	if days < 0 {
		return 0, fmt.Errorf("--days is 0 or more, not %d", days)
	}

	return time.Duration(days) * certDay, nil
}

// olderPeers are the peers this node has met whose last session did not say terminal_v2: a zmina from before
// expiries, which cannot read a certificate with one, refuses its holder and drops its envelopes.
func olderPeers(me *mesh.Node) []string {
	book, _ := me.Peers()

	var out []string

	for _, p := range book {
		if p.Cert != nil && !p.OK.IsZero() && !p.TerminalV2 {
			out = append(out, p.Name)
		}
	}

	return out
}

// checkLife refuses to issue a certificate with an expiry while a peer this node has met is of an older zmina: the
// owner's own certificate, a new node's and a renewed one all go to every peer in a hello.
func checkLife(me *mesh.Node, life time.Duration) error {
	if life == 0 {
		return nil
	}

	if old := olderPeers(me); len(old) > 0 {
		return fmt.Errorf("%s did not say terminal_v2 in its last session (a zmina from before expiries, or no session since this "+
			"one was installed: zmina node sync): it cannot read a certificate with an expiry and would refuse its holder. "+
			"Update it, or zmina node peer rm it if it is gone, or issue the old form with --days 0", strings.Join(old, ", "))
	}

	return nil
}

// certView is a certificate's standing, as zmina node ls --json says it.
type certView struct {
	Name    string `json:"name"`
	Expires string `json:"expires,omitempty"`
	// State is valid, expiring (within 30 days), expired, or none (issued with no expiry: it counts, and warns).
	State    string `json:"state"`
	DaysLeft *int   `json:"days_left,omitempty"`
}

func viewCert(c mesh.Cert, now time.Time) certView {
	state, at := c.State(now)
	v := certView{Name: c.Name, State: [...]string{"none", "valid", "expiring", "expired"}[state]}

	if !at.IsZero() {
		v.Expires = at.UTC().Format(time.DateOnly)

		days := int(math.Round(at.Sub(now).Hours() / 24))
		v.DaysLeft = &days
	}

	return v
}

// certLine says where a certificate stands, and what to run when it should be renewed. level is ok, warn or fail.
func certLine(c mesh.Cert, now time.Time) (level, text string) {
	state, at := c.State(now)
	renew := "on the owner's node run zmina node renew " + c.Name

	switch state {
	case mesh.CertValid:
		return "ok", fmt.Sprintf("expires %s (%s)", at.UTC().Format(time.DateOnly), inDays(at, now))
	case mesh.CertExpiring:
		return "warn", fmt.Sprintf("expires %s (%s): %s", at.UTC().Format(time.DateOnly), inDays(at, now), renew)
	case mesh.CertExpired:
		if at.IsZero() {
			return "fail", "its expiry does not read: " + renew
		}

		return "fail", fmt.Sprintf("expired %s (%s): it opens no session: %s", at.UTC().Format(time.DateOnly), inDays(at, now), renew)
	}

	return "warn", "no expiry (issued before certificates expired): it counts, and never runs out; once every node runs a " +
		"zmina that reads expiries, " + renew
}

// inDays is how far off a time is, in whole days (to the nearest), in words.
func inDays(at, now time.Time) string {
	days := int(math.Round(at.Sub(now).Hours() / 24))

	switch {
	case days == 0:
		return "today"
	case days == 1:
		return "in 1 day"
	case days == -1:
		return "1 day ago"
	case days < 0:
		return fmt.Sprintf("%d days ago", -days)
	}

	return fmt.Sprintf("in %d days", days)
}

// keyWord is a key as people name it: its first twelve hex digits.
func keyWord(k []byte) string { return fmt.Sprintf("%x", k[:min(6, len(k))]) }

func (a *app) nodeRenew() *cobra.Command {
	var days int

	c := &cobra.Command{
		Use:   "renew [<node>] [--days <n>]",
		Short: "On the owner's node: certify a node again (this one by default) with a fresh expiry, the way enroll certified it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			life, err := certLife(days)
			if err != nil {
				return err
			}

			me, err := openNode()
			if err != nil {
				return err
			}

			name := me.Name
			if len(args) == 1 {
				name = args[0]
			}

			if !me.IsOwner() {
				return fmt.Errorf("this node does not hold the owner key: renew from the owner's node (zmina node renew %s there), "+
					"or by hand: zmina node init --json here | zmina node sign | zmina node accept", name)
			}

			if err := checkLife(me, life); err != nil {
				return err
			}

			var cert mesh.Cert

			if name == me.Name {
				if cert, err = mesh.Renew(meshDir(), time.Now(), life); err != nil {
					return err
				}
			} else if cert, err = renewPeer(cmd.Context(), me, name, life); err != nil {
				return err
			}

			fmt.Fprintf(a.out, "node %s · certificate renewed, %s\n", name, validWords(cert, time.Now()))

			return nil
		},
	}

	daysFlag(c, &days)

	return c
}

// validWords is how long a fresh certificate counts, for the line that says it was made.
func validWords(c mesh.Cert, now time.Time) string {
	if at, has, _ := c.Expiry(); has {
		return fmt.Sprintf("expires %s (%s)", at.UTC().Format(time.DateOnly), inDays(at, now))
	}

	return "no expiry"
}

// renewPeer certifies the node called name again, the way enroll did the first time: ask its zmina for its request
// through the address the book holds, sign it here, have it accepted there. The key the far side answers with must be
// the one this owner certified: a renewal moves a node's date, never its key.
func renewPeer(ctx context.Context, me *mesh.Node, name string, life time.Duration) (mesh.Cert, error) {
	book, err := me.Peers()
	if err != nil {
		return mesh.Cert{}, err
	}

	i := slices.IndexFunc(book, func(p mesh.Peer) bool { return p.Name == name })
	if i < 0 || book[i].Cert == nil {
		return mesh.Cert{}, fmt.Errorf("no certificate of %s in this node's book: it has not met that node (zmina node sync)", name)
	}

	if len(book[i].Addrs) == 0 {
		return mesh.Cert{}, fmt.Errorf("%s has no address here (it calls in, or is reached through another node): on it run "+
			"zmina node init --json, sign that here with zmina node sign, and zmina node accept there", name)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var errs []error

	for _, addr := range book[i].Addrs {
		cert, err := renewAt(ctx, me, addr, book[i], life)
		if err == nil {
			return cert, nil
		}

		errs = append(errs, err)
	}

	return mesh.Cert{}, errors.Join(errs...)
}

func renewAt(ctx context.Context, me *mesh.Node, addr mesh.Addr, peer mesh.Peer, life time.Duration) (mesh.Cert, error) {
	out, err := runPeer(ctx, addr, nil, "node", "init", "--name", peer.Name, "--json")
	if err != nil {
		return mesh.Cert{}, err
	}

	var r mesh.Request
	if err := json.Unmarshal(out, &r); err != nil {
		return mesh.Cert{}, fmt.Errorf("its zmina answered %q: is it new enough (zmina node)?", strings.TrimSpace(string(out)))
	}

	if r.Name != peer.Name || !r.Key.Equal(peer.Cert.Key) {
		return mesh.Cert{}, fmt.Errorf("%s: the zmina there is not %s with the key this owner certified: nothing was signed", addr, peer.Name)
	}

	g, err := mesh.SignRequestFor(meshDir(), r, time.Now(), life)
	if err != nil {
		return mesh.Cert{}, err
	}

	raw, _ := json.Marshal(g)
	if _, err := runPeer(ctx, addr, raw, "node", "accept"); err != nil {
		return mesh.Cert{}, err
	}

	// What the book holds is what the owner knows of it: the new certificate, so this node does not warn of the old.
	_ = me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		for j := range ps {
			if ps[j].Name == peer.Name && (ps[j].Cert == nil || g.Cert.Newer(*ps[j].Cert)) {
				c := g.Cert
				ps[j].Cert = &c
			}
		}

		return ps
	})

	return g.Cert, nil
}

func (a *app) nodeRevoke() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <node>",
		Short: "On the owner's node: revoke the key of a node's certificate; every node that meets the list refuses it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			me, err := openNode()
			if err != nil {
				return err
			}

			list, added, err := me.Revoke(args[0], time.Now())
			if err != nil {
				return err
			}

			if !added {
				fmt.Fprintf(a.out, "%s is on the revocation list already (version %d, %d keys)\n", args[0], list.Version, len(list.Revoked))

				return nil
			}

			key := list.Revoked[len(list.Revoked)-1].Key

			fmt.Fprintf(a.out, "revoked %s (key %s…) · revocation list version %d, %d keys. It goes to every node in the "+
				"hello of its next session: a node that has it refuses that key. zmina node peer rm %s stops this node calling it\n",
				args[0], keyWord(key), list.Version, len(list.Revoked), args[0])

			return nil
		},
	}
}

// certBeat says, at most once a day, that this node's certificate has run out or runs out within 30 days — and, on the
// owner's node, the same of the certificates it holds for its peers, since only it can renew them. The words go to the
// event log (zmina log) and, as a health note, to this crew's dispatcher; the ticker's line shows them too.
func (a *app) certBeat(me *mesh.Node, now time.Time) {
	var lines []string

	if state, _ := me.Cert.State(now); state == mesh.CertExpiring || state == mesh.CertExpired {
		_, text := certLine(me.Cert, now)
		lines = append(lines, "this node's certificate "+text)
	}

	if me.IsOwner() {
		book, _ := me.Peers()

		for _, p := range book {
			if p.Cert == nil {
				continue
			}

			if state, _ := p.Cert.State(now); state == mesh.CertExpiring || state == mesh.CertExpired {
				_, text := certLine(*p.Cert, now)
				lines = append(lines, "the certificate of "+p.Name+" "+text)
			}
		}
	}

	if len(lines) == 0 || !me.NoticeDue("cert-expiry", now, certDay) {
		return
	}

	note := "mesh: " + strings.Join(lines, "; ")

	_ = a.withState(func(s *shift.Shift) error {
		s.Log("", "node-cert", "", strings.Join(lines, "; "))
		s.AddNotes([]string{note})

		return nil
	})

	a.printTick([]string{note})
}
