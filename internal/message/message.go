// Package message persists logical report revisions separately from transport envelopes.
package message

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/record"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const Prefix = mesh.TypedReportPrefix

var hashID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var stages = []string{"queued_origin", "received", "queued_dispatcher", "submitted", "read", "relayed"}

type Report struct {
	ContentHash string      `json:"content_hash,omitempty"` // Empty on legacy content-addressed revisions.
	Version     int         `json:"version"`
	ID          string      `json:"id"`
	Key         string      `json:"key"`
	Revision    string      `json:"revision"`
	Sequence    uint64      `json:"sequence"`
	From        string      `json:"from"`
	To          string      `json:"to"`
	Task        string      `json:"task,omitempty"`
	Body        string      `json:"body"`
	Status      bool        `json:"status,omitempty"`
	Supersedes  string      `json:"supersedes,omitempty"`
	RecordKind  string      `json:"record_kind,omitempty"`
	Record      *record.Ref `json:"record,omitempty"`
	At          time.Time   `json:"at"`
}
type Receipt struct {
	ReportVersion int       `json:"report_version,omitempty"`
	ID            string    `json:"id"`
	Revision      string    `json:"revision"`
	Sequence      uint64    `json:"sequence"`
	Origin        string    `json:"origin"`
	Recipient     string    `json:"recipient"`
	Stage         string    `json:"stage"`
	At            time.Time `json:"at"`
}
type Wire struct {
	Version int      `json:"version"`
	Kind    string   `json:"kind"`
	Report  *Report  `json:"report,omitempty"`
	Receipt *Receipt `json:"receipt,omitempty"`
}
type Intent struct {
	Envelope mesh.Envelope `json:"envelope"`
	Queued   bool          `json:"queued"`
}
type Revision struct {
	LastSyncError   string               `json:"last_sync_error,omitempty"`
	LastSyncErrorAt time.Time            `json:"last_sync_error_at,omitzero"`
	RemoteObserved  map[string]time.Time `json:"remote_observed,omitempty"`
	Report          Report               `json:"report"`
	Stage           string               `json:"stage"`
	Observed        map[string]time.Time `json:"observed"`
	Count           int                  `json:"count"`
	Notified        bool                 `json:"notified,omitempty"`
	Superseded      bool                 `json:"superseded,omitempty"`
	Intents         map[string]Intent    `json:"intents,omitempty"`
	LastError       string               `json:"last_error,omitempty"`
}
type Message struct {
	Version   int                  `json:"version"`
	ID        string               `json:"id"`
	Latest    string               `json:"latest"`
	Revisions map[string]*Revision `json:"revisions"`
}
type Store struct{ Dir string }

func Hash(v any) string                    { raw, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(raw)) }
func Identity(from, to, key string) string { return Hash([]string{from, to, key}) }
func Content(r Report) string {
	return Hash(struct {
		Body, Task string
		Status     bool
		Kind       string
		Record     *record.Ref
	}{r.Body, r.Task, r.Status, r.RecordKind, r.Record})
}

// Publication identifies an ordered publication independently of its repeated content.
func Publication(r Report) string { return Hash([]any{r.ID, r.Sequence, r.ContentHash}) }
func StageRank(stage string) int {
	for i, s := range stages {
		if s == stage {
			return i
		}
	}
	return -1
}
func Advance(r *Revision, stage string, at time.Time) error {
	if StageRank(stage) < 0 {
		return fmt.Errorf("unknown receipt stage %q", stage)
	}
	if r.Observed == nil {
		r.Observed = map[string]time.Time{}
	}
	if _, ok := r.Observed[stage]; !ok {
		r.Observed[stage] = at.UTC()
	}
	if StageRank(stage) > StageRank(r.Stage) {
		r.Stage = stage
	}
	return nil
}
func Encode(w Wire) (string, error) {
	w.Version = 1
	prefix := Prefix
	if (w.Report != nil && (w.Report.Version == 2 || w.Report.ContentHash != "")) || (w.Receipt != nil && w.Receipt.ReportVersion >= 2) {
		w.Version = 2
		prefix = mesh.TypedPublicationPrefix
	}
	raw, err := json.Marshal(w)
	return prefix + string(raw), err
}
func Decode(body string) (Wire, bool, error) {
	prefix, version := Prefix, 1
	if strings.HasPrefix(body, mesh.TypedPublicationPrefix) {
		prefix, version = mesh.TypedPublicationPrefix, 2
	} else if !strings.HasPrefix(body, Prefix) {
		return Wire{}, false, nil
	}
	var w Wire
	err := json.Unmarshal([]byte(strings.TrimPrefix(body, prefix)), &w)
	if err != nil {
		return w, true, err
	}
	if w.Version != version {
		return w, true, fmt.Errorf("unsupported report payload version")
	}
	if (w.Kind == "report" && w.Report != nil && w.Receipt == nil) || (w.Kind == "receipt" && w.Receipt != nil && w.Report == nil) {
		return w, true, nil
	}
	return w, true, fmt.Errorf("invalid typed report payload")
}
func (r Report) Validate() error {
	if !utf8.ValidString(r.Body) {
		return fmt.Errorf("report body must be valid UTF-8")
	}
	for _, c := range r.Body {
		if unicode.IsControl(c) && c != '\n' && c != '\r' && c != '\t' {
			return fmt.Errorf("report contains a terminal control character")
		}
	}
	if (r.Version != 1 && r.Version != 2) || (r.Version == 2 && r.ContentHash == "") || !hashID.MatchString(r.ID) || !hashID.MatchString(r.Revision) || r.Sequence == 0 || r.Key == "" || len(r.Key) > 160 || strings.TrimSpace(r.Body) == "" {
		return fmt.Errorf("invalid report identity/content")
	}
	from, err := mesh.ParseAddress(r.From)
	if err != nil || from.Crew == "" {
		return fmt.Errorf("report needs explicit origin crew")
	}
	to, err := mesh.ParseAddress(r.To)
	if err != nil || to.Crew == "" {
		return fmt.Errorf("report needs explicit destination crew")
	}
	wantRevision := Content(r)
	if r.ContentHash != "" {
		if r.ContentHash != wantRevision {
			return fmt.Errorf("report content hash mismatch")
		}
		wantRevision = Publication(r)
	}
	if r.ID != Identity(r.From, r.To, r.Key) || r.Revision != wantRevision {
		return fmt.Errorf("report identity/content hash mismatch")
	}
	if r.Supersedes != "" && (!r.Status || !hashID.MatchString(r.Supersedes)) {
		return fmt.Errorf("invalid superseded revision")
	}
	if r.Task != "" && !regexp.MustCompile(`^T-[0-9]+$`).MatchString(r.Task) {
		return fmt.Errorf("invalid related task")
	}
	if r.Record != nil && ((r.RecordKind != "result" && r.RecordKind != "checkpoint") || !hashID.MatchString(r.Record.Revision)) {
		return fmt.Errorf("invalid related record reference")
	}
	return nil
}
func (s Store) Load(id string) (Message, error) {
	if !hashID.MatchString(id) {
		return Message{}, fmt.Errorf("invalid message ID")
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, id+".json"))
	if err != nil {
		return Message{}, err
	}
	var m Message
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Version != 1 || m.ID != id || m.Revisions == nil {
		return m, fmt.Errorf("invalid message record")
	}
	if m.Revisions[m.Latest] == nil {
		return m, fmt.Errorf("missing latest report revision")
	}
	for key, v := range m.Revisions {
		if v == nil || v.Report.ID != m.ID || v.Report.Revision != key {
			return m, fmt.Errorf("invalid report revision")
		}
		if err := v.Report.Validate(); err != nil {
			return m, err
		}
		if v.Intents == nil {
			v.Intents = map[string]Intent{}
		}
		if v.Observed == nil {
			v.Observed = map[string]time.Time{}
		}
		if StageRank(v.Stage) < 0 {
			return m, fmt.Errorf("invalid stored report stage")
		}
		// Older senders stored unscoped Sync.Problems as current errors. A received
		// source publication has delivery evidence; retain those errors only as history.
		if _, origin := v.Intents["report"]; origin && v.Report.From != v.Report.To && StageRank(v.Stage) >= StageRank("received") && !strings.HasPrefix(v.LastError, "transport: ") {
			archiveSyncError(v)
		}
	}
	return m, nil
}
func (s Store) Save(m Message) error {
	if !hashID.MatchString(m.ID) || m.Version != 1 {
		return fmt.Errorf("invalid message identity")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".message-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.Dir, m.ID+".json"))
}
func (s Store) All() ([]Message, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Message
	for _, p := range paths {
		m, err := s.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil {
			return out, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Add preserves every revision; only explicit status supersession may suppress an unread status notification.
func (s Store) Add(r Report) (Message, bool, error) {
	if err := r.Validate(); err != nil {
		return Message{}, false, err
	}
	m, err := s.Load(r.ID)
	if os.IsNotExist(err) {
		m = Message{Version: 1, ID: r.ID, Revisions: map[string]*Revision{}}
	} else if err != nil {
		return m, false, err
	}
	if old := m.Revisions[r.Revision]; old != nil {
		if old.Report.Sequence != r.Sequence {
			return m, false, fmt.Errorf("revision sequence mismatch")
		}
		old.Count++
		return m, false, s.Save(m)
	}
	for _, old := range m.Revisions {
		if old.Report.Sequence == r.Sequence {
			return m, false, fmt.Errorf("report sequence collision")
		}
	}
	m.Revisions[r.Revision] = &Revision{Report: r, Count: 1, Stage: "queued_origin", Observed: map[string]time.Time{}, Intents: map[string]Intent{}}
	if m.Latest == "" || m.Revisions[m.Latest].Report.Sequence < r.Sequence {
		m.Latest = r.Revision
	}
	for _, newer := range m.Revisions {
		if newer.Report.Status && newer.Report.Supersedes != "" {
			if old := m.Revisions[newer.Report.Supersedes]; old != nil && old.Report.Status && old.Report.Sequence < newer.Report.Sequence {
				old.Superseded = true
			}
		}
	}
	return m, true, s.Save(m)
}
func Revisions(m Message) []string {
	keys := make([]string, 0, len(m.Revisions))
	for k := range m.Revisions {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return m.Revisions[keys[i]].Report.Sequence < m.Revisions[keys[j]].Report.Sequence
	})
	return keys
}
func (s Store) ApplyReceipt(from, to string, r Receipt, now time.Time) error {
	m, err := s.Load(r.ID)
	if err != nil {
		return err
	}
	v := m.Revisions[r.Revision]
	if v == nil || v.Report.Sequence != r.Sequence || v.Report.From != to || v.Report.To != from || r.Origin != to || r.Recipient != from {
		return fmt.Errorf("receipt does not match original report recipient/revision")
	}
	if StageRank(r.Stage) < StageRank("received") {
		return fmt.Errorf("invalid remote receipt stage")
	}
	if v.RemoteObserved == nil {
		v.RemoteObserved = map[string]time.Time{}
	}
	if _, ok := v.RemoteObserved[r.Stage]; !ok {
		v.RemoteObserved[r.Stage] = r.At
	}
	if err := Advance(v, r.Stage, now); err != nil {
		return err
	}
	// This receipt is bound to the exact origin/destination/publication. It proves
	// arrival independently of transport ACKs or a periodic reconciliation pass.
	if v.Report.From != v.Report.To || strings.HasPrefix(v.LastError, "transport: ") {
		archiveSyncError(v)
	}
	return s.Save(m)
}

func archiveSyncError(v *Revision) {
	if v.LastError == "" {
		return
	}
	if v.LastSyncError == "" {
		v.LastSyncError = v.LastError
	}
	v.LastError = ""
}
