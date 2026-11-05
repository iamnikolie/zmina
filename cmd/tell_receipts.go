package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/shift"
)

// Receipts for plain lines (zmina tell without --report-key). The crew that takes a line keeps a record of it
// (tells/in) and marks it queued for its dispatcher in state under message.TellMarker; the shift adds submitted
// and read. From the committed stages, reconcileTells sends each stage back once, as a signed tell_receipt, to a
// sender whose node authenticated tell_receipts. The sending crew keeps the lines it sent (tells/out) and the
// stages that came back: zmina message ls / show.

func tellStore(c *crew.Crew) message.TellStore {
	return message.TellStore{Dir: filepath.Join(c.StateDir(), "tells")}
}

func tells(s *shift.Shift) message.TellStore { return tellStore(s.Crew) }

// tellHeadMax is how much of a sent line its record keeps, in characters.
const tellHeadMax = 80

func tellHead(body string) string {
	r := []rune(strings.Join(strings.Fields(body), " "))
	if len(r) <= tellHeadMax {
		return string(r)
	}

	return string(r[:tellHeadMax-1]) + "…"
}

// recordTellOut keeps a plain line this node's crew from sends to, under id, before it leaves: the receipts that
// come back for it apply to this record. It returns false when the sending crew is not one other crews can
// answer (not registered here): no receipt could reach it.
func recordTellOut(from, to mesh.Address, id, body string, now time.Time) (bool, error) {
	c, err := localCrew(from.Crew)
	if err != nil {
		return false, nil
	}

	cr, err := crew.Load(c.Path)
	if err != nil {
		return false, nil
	}

	store := tellStore(cr)
	if err := store.PruneOut(now, message.TellKeep); err != nil {
		return false, err
	}

	return true, store.SaveOut(message.TellOut{ID: id, From: from.String(), To: to.String(), Head: tellHead(body),
		Sent: now.UTC()})
}

// recordTellIn marks a plain line just queued for the dispatcher (note is the signal as queued): its stage in
// state, and the record that addresses its receipts back to origin.
func recordTellIn(s *shift.Shift, origin, recipient mesh.Address, id, note, sigID string, sent time.Time) error {
	marker := message.TellMarker(id)
	// Its receipts follow the signal it was queued as, never a later line with the same text.
	s.BindMessage(marker, sigID)

	if s.State.MessageLines == nil {
		s.State.MessageLines = map[string]string{}
	}

	if s.State.MessageStages == nil {
		s.State.MessageStages = map[string]map[string]time.Time{}
	}

	s.State.MessageLines[marker] = note

	if s.State.MessageStages[marker] == nil {
		s.State.MessageStages[marker] = map[string]time.Time{}
	}

	if _, ok := s.State.MessageStages[marker]["queued_dispatcher"]; !ok {
		s.State.MessageStages[marker]["queued_dispatcher"] = s.Now().UTC()
	}

	store := tells(s)
	if _, err := store.LoadIn(id); err == nil {
		return nil
	}

	return store.SaveIn(message.TellIn{ID: id, Origin: origin.String(), Recipient: recipient.String(),
		Sent: sent.UTC(), At: s.Now().UTC()})
}

// forgetTell drops a plain line's marker from state.
func forgetTell(s *shift.Shift, marker string) { s.ForgetMessage(marker) }

// tellReceipts reports whether origin takes receipts: a registered crew of this node, or a crew of a node that
// authenticated tell_receipts in its last session with this one.
func tellReceipts(me *mesh.Node, book []mesh.Peer, origin mesh.Address) bool {
	if origin.Node == me.Name {
		_, err := localCrew(origin.Crew)

		return err == nil
	}

	for _, p := range book {
		if p.Name == origin.Node {
			return p.TellReceipts
		}
	}

	return false
}

// reconcileTells queues the receipts of the lines this crew took, from committed stages only, each stage once: the
// envelope is committed in the record before it is queued, and a crash between the two queues the same envelope
// again. A line read whose receipt is queued (or whose sender takes none) is forgotten; so is any after TellKeep.
func (a *app) reconcileTells(s *shift.Shift) error {
	store := tells(s)

	all, err := store.AllIn()
	if err != nil {
		return err
	}

	now := s.Now().UTC()

	var errs []error

	if len(all) > 0 {
		me, err := mesh.Open(meshDir())
		if err != nil {
			return err
		}

		book, err := me.Peers()
		if err != nil {
			return err
		}

		for _, t := range all {
			if err := a.reconcileTell(s, store, me, book, t, now); err != nil {
				errs = append(errs, fmt.Errorf("receipts for %s: %w", t.ID, err))
			}
		}
	}

	// A marker whose record is gone (forgotten, or a state saved without it) ages out.
	for marker, stages := range s.State.MessageStages {
		if !strings.HasPrefix(marker, "tell/") {
			continue
		}

		var newest time.Time
		for _, at := range stages {
			if at.After(newest) {
				newest = at
			}
		}

		if now.Sub(newest) > message.TellKeep {
			forgetTell(s, marker)
		}
	}

	return errors.Join(errs...)
}

func (a *app) reconcileTell(s *shift.Shift, store message.TellStore, me *mesh.Node, book []mesh.Peer,
	t message.TellIn, now time.Time,
) error {
	marker := message.TellMarker(t.ID)

	if now.Sub(t.At) > message.TellKeep {
		forgetTell(s, marker)

		return store.RemoveIn(t.ID)
	}

	origin, err := mesh.ParseAddress(t.Origin)
	if err != nil {
		return err
	}

	recipient, err := mesh.ParseAddress(t.Recipient)
	if err != nil {
		return err
	}

	committed := s.CommittedMessageStages[marker]
	receipts := tellReceipts(me, book, origin)

	if receipts {
		fresh := false

		for _, stage := range message.TellStages {
			at, ok := committed[stage]
			if _, sent := t.Intents[stage]; !ok || sent {
				continue
			}

			body, err := message.Encode(message.Wire{TellReceipt: &message.TellReceipt{ID: t.ID, Origin: t.Origin,
				Recipient: t.Recipient, Stage: stage, At: at.UTC()}})
			if err != nil {
				return err
			}

			env, err := me.NewEnvelope(recipient.Crew, origin, body, s.Now())
			if err != nil {
				return err
			}

			t.Intents[stage] = message.Intent{Envelope: env}
			fresh = true
		}

		// Commit intents before queuing them; replay uses the same signed envelope ID after a crash.
		if fresh {
			if err := store.SaveIn(t); err != nil {
				return err
			}
		}

		for _, stage := range message.TellStages {
			intent, ok := t.Intents[stage]
			if !ok || intent.Queued {
				continue
			}

			if err := me.Queue(intent.Envelope); err != nil {
				return err
			}

			intent.Queued = true
			t.Intents[stage] = intent

			if err := store.SaveIn(t); err != nil {
				return err
			}
		}
	}

	if _, read := committed["read"]; read && (!receipts || t.Intents["read"].Queued) {
		forgetTell(s, marker)

		return store.RemoveIn(t.ID)
	}

	return nil
}

// applyTellReceipt records a receipt that came to crew c from from (verified): a receipt for a line this crew
// holds no record of is taken and dropped, one that does not match the line is refused.
func (a *app) applyTellReceipt(c uiCrew, from mesh.Address, r message.TellReceipt) error {
	me, err := mesh.Open(meshDir())
	if err != nil {
		return err
	}

	local := mesh.Address{Node: me.Name, Crew: c.Key}.String()

	return a.forCrew(c.Path).withState(func(s *shift.Shift) error {
		err := tells(s).ApplyTellReceipt(from.String(), local, r, s.Now())
		if errors.Is(err, message.ErrUnknownTell) {
			return nil
		}

		if errors.Is(err, message.ErrBadTellReceipt) {
			return fmt.Errorf("%w: %v", mesh.ErrRefused, err)
		}

		return err
	})
}

// tellStageWords say a stage as the sender reads it.
var tellStageWords = map[string]string{
	"queued_dispatcher": "queued for its dispatcher",
	"submitted":         "in its dispatcher's conversation",
	"read":              "read by its dispatcher",
}

// tellSummary is where a sent line is, as its receipts say.
func tellSummary(t message.TellOut, peer mesh.Peer, node string) string {
	if t.Stage != "" {
		return fmt.Sprintf("%s %s", tellStageWords[t.Stage], t.Stages[t.Stage].Local().Format("15:04:05"))
	}

	to, _ := mesh.ParseAddress(t.To)
	if to.Node != node && !peer.TellReceipts {
		return "no receipts: " + to.Node + " has not said it sends them (an older zmina, or not met directly)"
	}

	return "no receipt yet"
}

// tellHint is the line plain tell adds when the far side sends receipts: the stage the line is at already (a
// receipt may come back in the same session), and where to follow it.
func (a *app) tellHint(from mesh.Address, id string) string {
	now := "none yet"

	if c, err := localCrew(from.Crew); err == nil {
		if cr, err := crew.Load(c.Path); err == nil {
			if t, err := tellStore(cr).LoadOut(id); err == nil && t.Stage != "" {
				now = tellStageWords[t.Stage]
			}
		}
	}

	return fmt.Sprintf("receipts: %s; zmina message show %s (queued, submitted, read as the far dispatcher takes it)\n",
		now, id)
}

// listTells prints the plain lines this crew sent, oldest first: kind tell, where each is, and its head.
func (a *app) listTells(s *shift.Shift) error {
	all, err := tells(s).AllOut()
	if err != nil {
		return err
	}

	node, book := a.tellBook()

	for _, t := range all {
		to, _ := mesh.ParseAddress(t.To)
		fmt.Fprintf(a.out, "%s · tell · %s · %s → %s · sent %s · %s\n", t.ID, tellSummary(t, peerIn(book, to.Node), node),
			t.From, t.To, t.Sent.Local().Format("2006-01-02 15:04:05"), t.Head)
	}

	return nil
}

// showTell prints one plain line sent and every stage its destination reported.
func (a *app) showTell(t message.TellOut) error {
	node, book := a.tellBook()
	to, _ := mesh.ParseAddress(t.To)

	fmt.Fprintf(a.out, "%s · tell · %s\n%s → %s\n%s\n", t.ID, tellSummary(t, peerIn(book, to.Node), node), t.From, t.To, t.Head)
	fmt.Fprintf(a.out, "sent: %s\n", t.Sent.Format(time.RFC3339))

	if t.Recipient != "" && t.Recipient != t.To {
		fmt.Fprintf(a.out, "taken by: %s\n", t.Recipient)
	}

	for _, stage := range message.TellStages {
		if at, ok := t.Stages[stage]; ok {
			fmt.Fprintf(a.out, "%s reported by destination: %s (receipt here %s)\n", stage, at.Format(time.RFC3339),
				t.Seen[stage].Format(time.RFC3339))
		}
	}

	return nil
}

// tellBook is this node's name and address book, empty when it is not a node.
func (a *app) tellBook() (string, []mesh.Peer) {
	me, err := mesh.Open(meshDir())
	if err != nil {
		return "", nil
	}

	book, _ := me.Peers()

	return me.Name, book
}

func peerIn(book []mesh.Peer, name string) mesh.Peer {
	for _, p := range book {
		if p.Name == name {
			return p
		}
	}

	return mesh.Peer{Name: name}
}
