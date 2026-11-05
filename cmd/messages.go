package cmd

import (
	"errors"
	"fmt"
	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// reportID is a report's id: a line sent has its envelope id.
var reportID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func messages(s *shift.Shift) message.Store {
	return message.Store{Dir: filepath.Join(s.Crew.StateDir(), "messages")}
}
func reportMarker(r message.Report) string { return r.ID + "/" + r.Revision }
func reportInbox(r message.Report) string {
	return "in-" + message.Hash([]string{r.ID, r.Revision})[:12]
}
func validateReportPeer(p mesh.Peer, size int, versions ...int) error {
	version := 1
	if len(versions) > 0 {
		version = versions[0]
	}
	if version >= 2 && !p.ReportsV2 {
		return fmt.Errorf("%s has not authenticated reports_v2 support; update and sync the recipient for publication reports; plain tell remains available", p.Name)
	}

	if !p.ReportsV1 {
		return fmt.Errorf("%s has not authenticated reports_v1 support; sync directly with the node or update its zmina; plain tell remains available", p.Name)
	}
	if size > p.Takes() || size > mesh.BodyMax {
		return fmt.Errorf("typed report is %d bytes, over the negotiated limit %d; shorten it or use an artifact already reachable by the recipient", size, min(p.Takes(), mesh.BodyMax))
	}
	return nil
}
func (a *app) tellReport(cmd *cobra.Command, args []string, key, taskID, file string, status bool) error {
	to, err := mesh.ParseAddress(args[0])
	if err != nil {
		return err
	}
	if to.Crew == "" {
		return fmt.Errorf("typed reports require an explicit node/crew destination")
	}
	me, err := mesh.Open(meshDir())
	if err != nil {
		return err
	}
	body := strings.Join(args[1:], " ")
	if file != "" {
		if body != "" {
			return fmt.Errorf("choose text or --file")
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(f, mesh.BodyMax+1))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(raw) > mesh.BodyMax {
			return fmt.Errorf("typed report body exceeds %d bytes; use a reachable artifact", mesh.BodyMax)
		}
		body = string(raw)
	}
	from := mesh.Address{Node: me.Name, Crew: a.crewKey()}
	origin, err := localCrew(from.Crew)
	if err != nil {
		return fmt.Errorf("typed report origin must be a registered local crew: %w", err)
	}
	if to.Node != me.Name && !peerOf(me, to.Node).ReportsV2 {
		_ = me.Sync(cmd.Context(), mesh.Exec{}, []mesh.Discovery{mesh.Book{Node: me}}, a.deliverFrom(), to.Node, peerTimeout, 0, time.Now)
	}
	var id, publication string
	err = a.forCrew(origin.Path).withState(func(s *shift.Shift) error {
		store := messages(s)
		r := message.Report{Version: 2, Key: key, From: from.String(), To: to.String(), Body: body, Status: status, At: s.Now().UTC(), Sequence: 1}
		if taskID != "" {
			t, err := s.Tasks.Get(taskID)
			if err != nil {
				return err
			}
			r.Task = t.ID
			r.Record, r.RecordKind = t.Result, "result"
			if r.Record == nil {
				r.Record, r.RecordKind = t.Checkpoint, "checkpoint"
			}
			if r.Record == nil {
				r.RecordKind = ""
			}
		}
		r.ID = message.Identity(r.From, r.To, key)
		r.ContentHash = message.Content(r)
		id = r.ID
		if old, err := store.Load(r.ID); err == nil {
			if v := old.Revisions[old.Latest]; message.Content(v.Report) == r.ContentHash {
				r = v.Report
			} else {
				r.Sequence = old.Revisions[old.Latest].Report.Sequence + 1
				if status && old.Revisions[old.Latest].Report.Status {
					r.Supersedes = old.Latest
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if r.Revision == "" {
			r.Revision = message.Publication(r)
		}
		publication = r.Revision
		wire, err := message.Encode(message.Wire{Kind: "report", Report: &r})
		if err != nil {
			return err
		}
		peer := peerOf(me, to.Node)
		if to.Node == me.Name {
			peer = mesh.Peer{Name: me.Name, BodyMax: mesh.BodyMax, ReportsV1: true, ReportsV2: true}
		}
		if err := validateReportPeer(peer, len(wire), mesh.ReportWireVersion(wire)); err != nil {
			return err
		}
		m, _, err := store.Add(r)
		if err != nil {
			return err
		}
		v := m.Revisions[r.Revision]
		if _, ok := v.Intents["report"]; !ok {
			env, err := me.NewEnvelope(from.Crew, to, wire, s.Now())
			if err != nil {
				return err
			}
			v.Intents["report"] = message.Intent{Envelope: env}
			if err := store.Save(m); err != nil {
				return err
			}
		}
		if err := message.Advance(v, "queued_origin", s.Now()); err != nil {
			return err
		}
		if err := store.Save(m); err != nil {
			return err
		}
		return a.reconcileMessages(s)
	})
	if err != nil {
		return err
	}
	if to.Node != me.Name {
		sessions := me.Sync(cmd.Context(), mesh.Exec{}, []mesh.Discovery{mesh.Book{Node: me}}, a.deliverFrom(), to.Node, peerTimeout, 0, time.Now)
		var problems []string
		for _, session := range sessions {
			if session.Err != nil {
				problems = append(problems, session.Err.Error())
			}
			problems = append(problems, session.Problems...)
		}
		if err := a.forCrew(origin.Path).withState(func(s *shift.Shift) error {
			store := messages(s)
			m, err := store.Load(id)
			if err != nil {
				return err
			}
			v := m.Revisions[publication]
			if len(problems) > 0 {
				v.LastSyncError = strings.Join(problems, "; ")
				v.LastSyncErrorAt = s.Now().UTC()
			}
			// Session failures describe an attempt, not the current state of this
			// envelope. Only durable envelope diagnostics are presented as pending.
			v.LastError = ""
			if err := refreshReportTransport(me, v); err != nil {
				return err
			}
			return store.Save(m)
		}); err != nil {
			return err
		}
	}

	if err := a.pumpLocalReports(); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "report %s queued; zmina message show %s reports observed stages, not assumed delivery\n", id, id)
	return nil
}

// deliverReport is called only after envelope verification, or for a self-signed local envelope.
func (a *app) deliverReport(c uiCrew, from mesh.Address, envelopeID, body string, sent time.Time) (bool, error) {
	w, handled, err := message.Decode(body)
	if !handled {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("%w: %v", mesh.ErrRefused, err)
	}
	if w.Kind == "tell_receipt" {
		return true, a.applyTellReceipt(c, from, *w.TellReceipt)
	}
	me, err := mesh.Open(meshDir())
	if err != nil {
		return true, err
	}
	to := mesh.Address{Node: me.Name, Crew: c.Key}.String()
	err = a.forCrew(c.Path).withState(func(s *shift.Shift) error {
		store := messages(s)
		if w.Kind == "receipt" {
			if err := store.ApplyReceipt(from.String(), to, *w.Receipt, s.Now()); err != nil {
				return fmt.Errorf("%w: %v", mesh.ErrRefused, err)
			}
			return nil
		}
		r := *w.Report
		if r.From != from.String() || r.To != to {
			return fmt.Errorf("%w: report sender/recipient mismatch", mesh.ErrRefused)
		}
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%w: %v", mesh.ErrRefused, err)
		}
		m, _, err := store.Add(r)
		if err != nil {
			return err
		}
		v := m.Revisions[r.Revision]
		if err := s.Inbox.Put(inbox.Entry{ID: reportInbox(r), From: remoteSource(from), Remote: true, Body: r.Body}); err != nil {
			return err
		}
		if err := message.Advance(v, "received", s.Now()); err != nil {
			return err
		}
		if err := store.Save(m); err != nil {
			return err
		}
		// Queue saturation is retried from durable report storage; do not discard the verified body.
		if err := a.reconcileMessages(s); err != nil {
			return err
		}
		s.Deliver()
		return a.reconcileMessages(s)
	})
	_ = envelopeID
	_ = sent
	if err == nil {
		err = a.forCrew(c.Path).withState(a.reconcileMessages)
	}
	return true, err
}

// refreshReportTransport exposes envelope-specific observations without inferring delivery from an ACK.
func refreshReportTransport(me *mesh.Node, v *message.Revision) error {
	var pending []string
	for _, intent := range v.Intents {
		problem, err := me.PendingError(intent.Envelope.ID)
		if err != nil {
			return err
		}
		if problem != "" {
			pending = append(pending, problem)
		}
	}
	slices.Sort(pending)
	if len(pending) > 0 {
		v.LastError = "transport: " + strings.Join(pending, "; ")
	} else if strings.HasPrefix(v.LastError, "transport: ") {
		v.LastError = ""
	}
	return nil
}

// reconcileMessages performs only local record/outbox work while the crew lock is held: report records, and the
// receipts of plain lines this crew took.
func (a *app) reconcileMessages(s *shift.Shift) error {
	return errors.Join(a.reconcileReports(s), a.reconcileTells(s))
}

func (a *app) reconcileReports(s *shift.Shift) error {
	store := messages(s)
	all, err := store.All()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	me, err := mesh.Open(meshDir())
	if err != nil {
		return err
	}
	crewKey := ""
	for _, c := range uiCrews("") {
		if c.Path == s.Crew.Path {
			crewKey = c.Key
			break
		}
	}
	if crewKey == "" {
		return fmt.Errorf("report crew is no longer registered")
	}
	local := mesh.Address{Node: me.Name, Crew: crewKey}.String()
	for _, m := range all {
		for _, revision := range message.Revisions(m) {
			v := m.Revisions[revision]
			if err := refreshReportTransport(me, v); err != nil {
				return err
			}
			r := v.Report
			if r.From == local {
				if _, ok := v.Intents["report"]; !ok {
					body, err := message.Encode(message.Wire{Kind: "report", Report: &r})
					if err != nil {
						return err
					}
					to, _ := mesh.ParseAddress(r.To)
					env, err := me.NewEnvelope(crewKey, to, body, s.Now())
					if err != nil {
						return err
					}
					v.Intents["report"] = message.Intent{Envelope: env}
				}
				if err := message.Advance(v, "queued_origin", s.Now()); err != nil {
					return err
				}
			}
			if r.To == local {
				// Recover a crash after report persistence but before inbox/queue persistence.
				if _, received := v.Observed["received"]; !received {
					source, _ := mesh.ParseAddress(r.From)
					if err := s.Inbox.Put(inbox.Entry{ID: reportInbox(r), From: remoteSource(source), Remote: true, Body: r.Body}); err != nil {
						return err
					}
					if err := message.Advance(v, "received", s.Now()); err != nil {
						return err
					}
				}
				if s.State.MessageStages == nil {
					s.State.MessageStages = map[string]map[string]time.Time{}
				}
				marker := reportMarker(r)
				if v.Superseded {
					removeReportNotice(s, marker)
				}
				if s.State.MessageStages[marker] == nil {
					s.State.MessageStages[marker] = map[string]time.Time{}
				}
				if _, queued := s.State.MessageStages[marker]["queued_dispatcher"]; !queued && !v.Superseded && message.StageRank(v.Stage) < message.StageRank("read") {
					src, _ := mesh.ParseAddress(r.From)
					locator := fmt.Sprintf("Report %s stored as data; read zmina --crew %s message show %s --revision %s; explicitly mark message read after consuming it", reportMarker(r), quoteCommandArg(s.Crew.Path), r.ID, r.Revision)
					text, _ := remoteText(locator, reportInbox(r))
					if note, sigID, _, err := s.NoteSentAs(remoteSource(src), text, r.At); err != nil {
						v.LastError = err.Error()
					} else {
						s.State.MessageStages[marker]["queued_dispatcher"] = s.Now().UTC()
						if s.State.MessageLines == nil {
							s.State.MessageLines = map[string]string{}
						}
						s.State.MessageLines[marker] = note
						s.BindMessage(marker, sigID)
						v.LastError = ""
					}
				}
				sent := s.State.Role(s.Crew.Dispatcher).Sent
				if sent != nil && strings.Contains(sent.Line, reportMarker(r)) {
					if _, ok := s.State.MessageStages[marker]["submitted"]; !ok {
						s.State.MessageStages[marker]["submitted"] = s.Now().UTC()
					}
				}
				for stage, at := range s.CommittedMessageStages[marker] {
					if err := message.Advance(v, stage, at); err != nil {
						return err
					}
				}
				_, v.Notified = s.CommittedMessageStages[marker]["queued_dispatcher"]

				for _, stage := range []string{"received", "queued_dispatcher", "submitted", "read", "relayed"} {
					at, ok := v.Observed[stage]
					if !ok {
						continue
					}
					intentKey := "receipt/" + stage
					if _, ok := v.Intents[intentKey]; !ok {
						reportVersion := 1
						if r.ContentHash != "" {
							reportVersion = 2
						}
						rec := message.Receipt{ReportVersion: reportVersion, ID: r.ID, Revision: r.Revision, Sequence: r.Sequence, Origin: r.From, Recipient: r.To, Stage: stage, At: at}
						body, err := message.Encode(message.Wire{Kind: "receipt", Receipt: &rec})
						if err != nil {
							return err
						}
						dest, _ := mesh.ParseAddress(r.From)
						env, err := me.NewEnvelope(crewKey, dest, body, s.Now())
						if err != nil {
							return err
						}
						v.Intents[intentKey] = message.Intent{Envelope: env}
					}
				}
			}
			// Commit intents before queuing them; replay uses the same signed envelope ID after a crash.
			if err := store.Save(m); err != nil {
				return err
			}
			for key, intent := range v.Intents {
				if intent.Queued {
					continue
				}
				if err := me.Queue(intent.Envelope); err != nil {
					return err
				}
				intent.Queued = true
				v.Intents[key] = intent
				if err := store.Save(m); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *app) messageCommand() *cobra.Command {
	c := &cobra.Command{Use: "message", Short: "Inspect end-to-end typed report observations, explicit read/relay receipts, and the receipts of plain lines sent"}
	var revision string
	var asJSON bool
	show := &cobra.Command{Use: "show <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.readState(func(s *shift.Shift) error {
			if t, err := tells(s).LoadOut(args[0]); err == nil {
				if asJSON {
					return a.printJSON(t)
				}

				return a.showTell(t)
			} else if os.IsNotExist(err) && !reportID.MatchString(args[0]) {
				return fmt.Errorf("no line sent or report %s in this crew (zmina message ls)", args[0])
			}
			m, err := messages(s).Load(args[0])
			if err != nil {
				return err
			}
			if me, err := mesh.Open(meshDir()); err == nil {
				for _, v := range m.Revisions {
					if err := refreshReportTransport(me, v); err != nil {
						return err
					}
				}
			}
			if asJSON {
				return a.printJSON(m)
			}
			rev := revision
			if rev == "" {
				rev = m.Latest
			}
			v := m.Revisions[rev]
			if v == nil {
				return fmt.Errorf("unknown report revision")
			}
			fmt.Fprintf(a.out, "%s · revision %s · %s · %d observations\n%s → %s\n%s\n", m.ID, rev, v.Stage, v.Count, v.Report.From, v.Report.To, v.Report.Body)
			fmt.Fprintf(a.out, "source time: %s\n", v.Report.At.Format(time.RFC3339))
			for _, stage := range []string{"queued_origin", "received", "queued_dispatcher", "submitted", "read", "relayed"} {
				if at, ok := v.Observed[stage]; ok {
					fmt.Fprintf(a.out, "%s observed locally: %s\n", stage, at.Format(time.RFC3339))
				}
				if at, ok := v.RemoteObserved[stage]; ok {
					fmt.Fprintf(a.out, "%s reported by destination: %s\n", stage, at.Format(time.RFC3339))
				}
			}
			if v.LastError != "" {
				fmt.Fprintln(a.out, "pending:", v.LastError)
			}
			if message.StageRank(v.Stage) < message.StageRank("received") && v.LastSyncError != "" {
				fmt.Fprintln(a.out, "last failed sync (historical):", v.LastSyncError)
			}
			return nil
		})
	}}
	show.Flags().StringVar(&revision, "revision", "", "specific report revision (default: latest)")
	show.Flags().BoolVar(&asJSON, "json", false, "all revisions, counters, observations and transport intents")
	ls := &cobra.Command{Use: "ls", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		return a.readState(func(s *shift.Shift) error {
			all, err := messages(s).All()
			if err != nil {
				return err
			}
			for _, m := range all {
				v := m.Revisions[m.Latest]
				fmt.Fprintf(a.out, "%s · %s · %s → %s\n", m.ID, v.Stage, v.Report.From, v.Report.To)
			}
			return a.listTells(s)
		})
	}}
	c.AddCommand(show, ls)
	for _, action := range []string{"read", "relayed", "retry"} {
		action := action
		var rev string
		sub := &cobra.Command{Use: action + " <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			err := a.withState(func(s *shift.Shift) error {
				store := messages(s)
				m, err := store.Load(args[0])
				if err != nil {
					return err
				}
				hash := rev
				if hash == "" {
					hash = m.Latest
				}
				v := m.Revisions[hash]
				if v == nil {
					return fmt.Errorf("unknown report revision")
				}
				me, err := mesh.Open(meshDir())
				if err != nil {
					return err
				}
				local := mesh.Address{Node: me.Name, Crew: a.crewKey()}.String()
				if action == "retry" {
					if v.Report.From != local {
						return fmt.Errorf("retry belongs to report origin")
					}
					to, _ := mesh.ParseAddress(v.Report.To)
					body, err := message.Encode(message.Wire{Kind: "report", Report: &v.Report})
					if err != nil {
						return err
					}
					p := peerOf(me, to.Node)
					if to.Node == me.Name {
						p = mesh.Peer{Name: me.Name, BodyMax: mesh.BodyMax, ReportsV1: true, ReportsV2: true}
					}
					if err := validateReportPeer(p, len(body), mesh.ReportWireVersion(body)); err != nil {
						return err
					}
					env, err := me.NewEnvelope(a.crewKey(), to, body, s.Now())
					if err != nil {
						return err
					}
					v.Intents["report"] = message.Intent{Envelope: env}
				} else {
					if v.Report.To != local {
						return fmt.Errorf("read/relay receipt belongs to destination crew")
					}
					if s.By != "hand" && s.By != s.Crew.Dispatcher {
						return fmt.Errorf("only the destination dispatcher can record read/relay")
					}
					if action == "relayed" && message.StageRank(v.Stage) < message.StageRank("read") {
						return fmt.Errorf("mark the report read before explicitly recording owner relay")
					}
					if s.State.MessageStages == nil {
						s.State.MessageStages = map[string]map[string]time.Time{}
					}
					marker := reportMarker(v.Report)
					if s.State.MessageStages[marker] == nil {
						s.State.MessageStages[marker] = map[string]time.Time{}
					}
					removeReportNotice(s, marker)
					if _, ok := s.State.MessageStages[marker][action]; !ok {
						s.State.MessageStages[marker][action] = s.Now().UTC()
					}
				}
				if err := store.Save(m); err != nil {
					return err
				}
				return a.reconcileMessages(s)
			})
			if err != nil {
				return err
			}
			if err = a.withState(a.reconcileMessages); err != nil {
				return err
			}
			return a.pumpLocalReports()
		}}
		sub.Flags().StringVar(&rev, "revision", "", "specific report revision (default: latest)")
		c.AddCommand(sub)
	}
	return c
}

// pumpLocalReports consumes self-addressed envelopes without taking another crew lock under a crew lock.
func (a *app) pumpLocalReports() error {
	me, err := mesh.Open(meshDir())
	if err != nil {
		return err
	}
	for pass := 0; pass < 8; pass++ {
		out, err := me.Outbox()
		if err != nil {
			return err
		}
		progress := false
		for _, e := range out {
			to, err := mesh.ParseAddress(e.To)
			if err != nil || to.Node != me.Name {
				continue
			}
			_, typed, err := message.Decode(e.Body)
			if !typed {
				continue
			}
			if err != nil {
				return err
			}
			ids, err := me.DeliverLocal(e, a.deliverFrom(), time.Now())
			me.Unqueue(ids)
			if err != nil {
				return err
			}
			progress = true
		}
		if !progress {
			return nil
		}
	}
	return nil
}

func quoteCommandArg(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }

func removeReportNotice(s *shift.Shift, marker string) {
	line, id := s.State.MessageLines[marker], s.State.MessageIDs[marker]
	if line == "" && id == "" {
		return
	}
	for _, sig := range slices.Clone(s.State.Signals) {
		// The notice is known by the signal it was queued as; a notice queued before ids were bound, by its text
		// (it names the report's id and revision).
		if id != "" && shift.QueuedSignalID(sig) == id || id == "" && shift.SignalBody(sig) == line {
			s.DropSignal(sig)
		}
	}
	delete(s.State.MessageLines, marker)
	delete(s.State.MessageIDs, marker)
}
