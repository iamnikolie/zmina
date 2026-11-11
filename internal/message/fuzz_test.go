package message

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// What a peer hands over (a wire body) and what a crew keeps (message and line records) is read from bytes nobody here
// wrote. Each target: no panic, and what is accepted is accepted again once it is written back. Run one for a while
// with
//
//	go test -fuzz=FuzzDecode -fuzztime=30s -run '^$' ./internal/message
//
// Plain `go test` runs the seeds below and every input kept in testdata/fuzz as an ordinary test.

func wireSeeds(f *testing.F) {
	r := report("First\nbody", 1)
	v2 := report("Typed publication", 2)
	v2.Version, v2.ContentHash = 2, Content(v2)
	v2.Revision = Publication(v2)

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := Receipt{ID: r.ID, Revision: r.Revision, Sequence: 1, Origin: r.From, Recipient: r.To, Stage: "read", At: at}
	tell := TellReceipt{ID: "alpha-20261105T113347-0a1b2c3d", Origin: "alpha/ops", Recipient: "beta/acme", Stage: "submitted", At: at}

	for _, w := range []Wire{{Kind: "report", Report: &r}, {Kind: "report", Report: &v2}, {Kind: "receipt", Receipt: &rec},
		{Kind: "tell_receipt", TellReceipt: &tell}} {
		if body, err := Encode(w); err == nil {
			f.Add(body)
		}
	}

	for _, s := range []string{"", "[zmina-report]", "[zmina-report-v2]{}", "[zmina-tell-receipt-v1]{}", "[zmina-report]{\"version\":1}",
		"[zmina-report]null", "[zmina-report][]", "[zmina-report]{\"version\":1,\"kind\":\"report\",\"report\":null}",
		"[zmina-report]{\"version\":1,\"kind\":\"receipt\",\"receipt\":{}}", "plain text", "\x00"} {
		f.Add(s)
	}
}

// FuzzDecode: a body is not a wire body (false), or is one and decodes or says why not; a wire that decodes is
// validated without a panic and encodes to a body that decodes again.
func FuzzDecode(f *testing.F) {
	wireSeeds(f)

	f.Fuzz(func(t *testing.T, body string) {
		w, wire, err := Decode(body)
		if !wire && err != nil {
			t.Fatalf("not a wire body, yet an error: %v", err)
		}

		if err != nil {
			return
		}

		if !wire {
			return
		}

		switch {
		case w.Report != nil:
			_ = w.Report.Validate()
			_ = Content(*w.Report)
			_ = Publication(*w.Report)
		case w.Receipt != nil:
			_ = StageRank(w.Receipt.Stage)
		case w.TellReceipt != nil:
			_ = w.TellReceipt.Validate()
		default:
			t.Fatalf("a wire accepted with nothing in it: %q", body)
		}

		again, err := Encode(w)
		if err != nil {
			t.Fatalf("an accepted wire does not encode: %v", err)
		}

		if _, wire, err := Decode(again); !wire || err != nil {
			t.Fatalf("an accepted wire does not decode again: %v\nbody: %q\nagain: %q", err, body, again)
		}
	})
}

// FuzzStoreAdd: whatever Decode accepts and Validate passes is stored, and stored again without a collision.
func FuzzStoreAdd(f *testing.F) {
	wireSeeds(f)

	f.Fuzz(func(t *testing.T, body string) {
		w, wire, err := Decode(body)
		if !wire || err != nil || w.Report == nil || w.Report.Validate() != nil {
			t.Skip()
		}

		s := Store{Dir: t.TempDir()}

		m, fresh, err := s.Add(*w.Report)
		if err != nil {
			t.Fatalf("a valid report is refused: %v", err)
		}

		if !fresh {
			t.Fatalf("a first report is not new")
		}

		if _, err := s.Load(m.ID); err != nil {
			t.Fatalf("a stored report does not load: %v", err)
		}

		if _, fresh, err := s.Add(*w.Report); err != nil || fresh {
			t.Fatalf("the same report again: fresh %v, error %v", fresh, err)
		}
	})
}

// FuzzStoreLoad: a message file of any bytes loads or is refused; one that loads has the id of its name and a latest
// revision that exists.
func FuzzStoreLoad(f *testing.F) {
	r := report("seed", 1)
	s := Store{Dir: f.TempDir()}

	if m, _, err := s.Add(r); err == nil {
		if raw, err := os.ReadFile(filepath.Join(s.Dir, m.ID+".json")); err == nil {
			f.Add(raw)
		}
	}

	for _, seed := range []string{"", "{}", "null", "[]", `{"version":1}`, `{"version":1,"id":"` + r.ID + `","revisions":{}}`,
		`{"version":1,"id":"` + r.ID + `","revisions":{"x":null},"latest":"x"}`} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		dir := t.TempDir()
		id := r.ID

		if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
			t.Skip(err)
		}

		m, err := Store{Dir: dir}.Load(id)
		if err != nil {
			return
		}

		if m.ID != id || m.Revisions[m.Latest] == nil {
			t.Fatalf("a loaded message that is not the one asked for or has no latest revision: %+v", m)
		}

		_ = Revisions(m)

		if _, err := json.Marshal(m); err != nil {
			t.Fatalf("a loaded message does not encode: %v", err)
		}
	})
}

// FuzzTellRecords: line records of any bytes, on either side, load or are skipped.
func FuzzTellRecords(f *testing.F) {
	const id = "alpha-20261105T113347-0a1b2c3d"

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in, _ := json.Marshal(TellIn{Version: 1, ID: id, Origin: "alpha/ops", Recipient: "beta/acme", At: at})
	out, _ := json.Marshal(TellOut{Version: 1, ID: id, From: "alpha/ops", To: "beta/acme", Head: "deploy", Sent: at})

	for _, seed := range [][]byte{in, out, []byte("{}"), []byte("null"), []byte(""), []byte(`{"id":"` + id + `","intents":{"x":{}}}`)} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		s := TellStore{Dir: t.TempDir()}

		for _, side := range []string{"in", "out"} {
			path := s.path(side, id)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Skip(err)
			}

			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Skip(err)
			}
		}

		if rec, err := s.LoadIn(id); err == nil && (rec.ID != id || rec.Intents == nil) {
			t.Fatalf("a loaded record of a line taken that is not whole: %+v", rec)
		}

		if rec, err := s.LoadOut(id); err == nil && rec.ID != id {
			t.Fatalf("a loaded record of a line sent with another id: %+v", rec)
		}

		if _, err := s.AllIn(); err != nil {
			t.Fatalf("the records of lines taken: %v", err)
		}

		if _, err := s.AllOut(); err != nil {
			t.Fatalf("the records of lines sent: %v", err)
		}

		r := TellReceipt{ID: id, Origin: "alpha/ops", Recipient: "beta/acme", Stage: "read", At: at}
		_ = s.ApplyTellReceipt("beta/acme", "alpha/ops", r, at)
	})
}
