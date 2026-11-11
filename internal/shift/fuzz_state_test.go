package shift

import (
	"encoding/json"
	"testing"
	"time"
)

// state.json is read by every command and every beat, whichever zmina wrote it: a file of the first format, one cut
// short, one edited by hand. It reads to a state that writes back and reads again, or is refused with an error. Run it
// for a while with
//
//	go test -fuzz=FuzzDecodeState -fuzztime=30s -run '^$' ./internal/shift
//
// Plain `go test` runs the seeds below and every input kept in testdata/fuzz as an ordinary test.

func FuzzDecodeState(f *testing.F) {
	st := NewState()
	r := st.Role("dev")
	r.Up, r.Started, r.Inbox = true, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), []string{"[dispatcher] T-1 do it"}
	r.Sent = &Sent{Line: "typed line", At: r.Started}
	r.Failed = []Failed{{Line: "lost", Error: "x", At: r.Started}}
	st.Role("reviewer")

	current, _ := json.Marshal(st)

	v1 := `{"started":{"dev":"2026-01-02T03:04:05Z"},"up":{"dev":true},"held":{"dev":false},"stuck":{"dev":3},` +
		`"notes":["a note"],"instances":{"dev#2":true},"last_session":{"dev":"sid"},"idle":{"dev":5},"quick_ends":{"dev":1},` +
		`"broken":{"dev":false},"inbox":{"dev":["line"]},"next_run":{"dev":"2026-01-03T00:00:00Z"},"expired":{"dev":"x"},` +
		`"next_spec":{"dev":"0 9 * * *"},"skip_noted":{"dev":"y"},"watch":{"w":{}}}`

	for _, s := range []string{string(current), v1, oldState, "", "   \n", "{}", "null", "[]", `{"version":2}`, `{"version":3}`,
		`{"version":-1}`, `{"version":2,"roles":{"dev":null}}`, `{"version":2,"roles":null,"watch":null}`,
		`{"version":2,"roles":{"dev":{"inbox":[1]}}}`, `{"up":{"dev":"yes"}}`, `{"version":2,"acks":{"S-1":null}}`,
		`{"version":2,"titles":{"a":"b"},"worktree_gc_at":"x"}`, "\xff"} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		got, _, err := decode(raw)
		if err != nil {
			return
		}

		if got == nil || got.Roles == nil || got.Watch == nil {
			t.Fatalf("a state that decoded is not initialised: %+v", got)
		}

		for name, r := range got.Roles {
			if r == nil {
				t.Fatalf("role %q decoded to nil", name)
			}
		}

		got.Role("someone-new")

		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("a state that decoded does not encode: %v", err)
		}

		back, _, err := decode(out)
		if err != nil {
			t.Fatalf("a state that was written does not read: %v\n%s", err, out)
		}

		if len(back.Roles) != len(got.Roles) {
			t.Fatalf("a state written and read again has %d roles, not %d", len(back.Roles), len(got.Roles))
		}
	})
}
