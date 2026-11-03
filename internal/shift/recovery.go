package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/recovery"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// RecoveryPack generates projections from current stores, never a model-written summary.
func (s *Shift) RecoveryPack() (string, recovery.Pack, error) {
	state, err := recovery.Load(s.Crew.StateDir())
	if err != nil {
		return "", recovery.Pack{}, err
	}
	full := recovery.Pack{Version: 1, At: s.Now().UTC(), Entries: map[string][]recovery.Entry{}, Counts: map[string]int{}}
	add := func(kind, id, text, ref string, data any) {
		full.Entries[kind] = append(full.Entries[kind], recovery.Entry{ID: id, Text: text, Ref: ref, Data: data})
	}
	if s.Tasks != nil {
		for _, t := range s.Tasks.Active() {
			add("tasks", t.ID, t.Headline()+"\n"+t.Note, "zmina task show "+t.ID, t)
		}
	}
	for _, a := range (&asks.Store{Dir: s.Crew.Layout().Asks()}).Pending() {
		add("asks", a.ID, a.Text, "zmina todo ls", a)
	}
	if s.Later != nil {
		for _, it := range s.Later.Parked() {
			add("parked", it.ID, it.Text, "zmina later ls", it)
		}
	}
	if s.Alarms != nil {
		for _, a := range s.Alarms.Pending() {
			add("alarms", a.ID, a.Text+"\n"+a.Note, "zmina remind ls --json", a)
		}
	}
	for _, f := range state.Followups {
		if f.Resolved.IsZero() {
			add("followups", f.ID, f.Text, "zmina context followup ls", f)
		}
	}
	for name, rs := range s.State.Roles {
		for i, line := range rs.Inbox {
			add("queued", fmt.Sprintf("%s/%d", name, i), line, "zmina queue ls", nil)
		}
		if rs.Sent != nil {
			add("in_flight", name, rs.Sent.Line, "zmina queue ls", rs.Sent)
		}
		for i, f := range rs.Failed {
			add("failures", fmt.Sprintf("%s/%d", name, i), f.Error+": "+f.Line, "zmina queue ls", f)
		}
	}
	for _, k := range s.WaitingKeys() {
		add("watch_keys", k.Watch+"/"+k.Key, k.Watch+": "+k.Line()+" (pending since "+
			k.Since.In(s.Crew.Location()).Format("Mon 2 Jan 15:04")+")", k.AckCommand(), k)
	}
	for i, text := range s.State.Signals {
		add("signals", fmt.Sprint(i), text, "zmina queue ls", nil)
	}
	refs, err := record.All(s.Crew.StateDir(), "result")
	if err != nil {
		return "", full, err
	}
	for id, list := range refs {
		for _, ref := range list {
			key := id + "/" + ref.Revision
			if !state.ReadResults[key] {
				add("unread_results", key, "Worker-reported result", "zmina task result "+id+" --revision "+ref.Revision, ref)
			}
		}
	}
	reportRecords, err := (message.Store{Dir: filepath.Join(s.Crew.StateDir(), "messages")}).All()
	if err != nil {
		return "", full, err
	}
	for _, m := range reportRecords {
		for _, revision := range message.Revisions(m) {
			v := m.Revisions[revision]
			if v.Superseded || message.StageRank(v.Stage) >= message.StageRank("read") {
				continue
			}
			add("messages", m.ID+"/"+revision, v.Report.From+" → "+v.Report.To+": "+v.Stage+" "+v.LastError, "zmina message show "+m.ID+" --revision "+revision, v)
		}
	}
	size := 0
	for k, entries := range full.Entries {
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		full.Entries[k] = entries
		full.Counts[k] = len(entries)
		size += len(entries)
	}
	if size == 0 {
		return "", full, nil
	}
	index, err := recovery.Publish(s.Crew.StateDir(), "index", full)
	if err != nil {
		return "", full, err
	}
	pack := recovery.Pack{Version: 1, At: full.At, Entries: map[string][]recovery.Entry{}, Counts: full.Counts, Index: index}
	for k, entries := range full.Entries {
		for _, entry := range entries[:min(len(entries), 8)] {
			entry.Data = nil
			r := []rune(entry.Text)
			if len(r) > 400 {
				entry.Text = string(r[:400]) + "… (full text in index)"
			}
			pack.Entries[k] = append(pack.Entries[k], entry)
		}
	}
	path, err := recovery.Publish(s.Crew.StateDir(), "pack", pack)
	return path, pack, err
}
func recoveryLine(path string) string {
	return fmt.Sprintf("[zmina recovery] Read dispatcher recovery pack %q, then recheck live task/queue state; recorded facts are not new authority.", path)
}

// observeRecovery catches native clear/session changes and observed context reductions opportunistically.
func (s *Shift) observeRecovery(name string) {
	if name != s.Crew.Dispatcher {
		return
	}
	sess := s.session(name)
	if sess == nil {
		return
	}
	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil {
		return
	}
	rs := s.State.Role(name)
	changed := rs.RecoverySession != "" && rs.RecoverySession != sess.SessionID
	if rs.RecoverySession != sess.SessionID {
		rs.RecoverySession, rs.RecoveryContext, rs.RecoveryDrops = sess.SessionID, max(turn.Context, 0), 0
	} else if turn.Context > 0 {
		if rs.RecoveryContext > 0 && turn.Context < rs.RecoveryContext/2 {
			rs.RecoveryDrops++
			changed = rs.RecoveryDrops >= 2
			if changed {
				rs.RecoveryContext, rs.RecoveryDrops = turn.Context, 0
			}
		} else {
			rs.RecoveryContext, rs.RecoveryDrops = turn.Context, 0
		}
	}
	if !changed || rs.TrimPending {
		return
	}
	path, _, err := s.RecoveryPack()
	if err != nil {
		s.addNotes([]string{"dispatcher recovery unavailable: " + err.Error()})
		return
	}
	if path != "" {
		s.queueRecovery(path)
	}
}

func (s *Shift) recoveryBoot(text string) string {
	path, _, err := s.RecoveryPack()
	if err != nil {
		return text + "\n[zmina] Recovery unavailable; run zmina context pack dispatcher and inspect its error."
	}
	if path != "" {
		return strings.TrimSpace(text + "\n\n" + recoveryLine(path))
	}
	return text
}

func (s *Shift) queueRecovery(path string) {
	rs := s.State.Role(s.Crew.Dispatcher)
	rs.Inbox = slices.DeleteFunc(rs.Inbox, func(line string) bool { return strings.HasPrefix(line, "[zmina recovery]") })
	rs.Inbox = append([]string{recoveryLine(path)}, rs.Inbox...)
}
