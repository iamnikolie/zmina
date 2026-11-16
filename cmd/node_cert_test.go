package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

const certTestDay = 24 * time.Hour

// zmIn is zm with something on stdin.
func zmIn(t *testing.T, crewFile, stdin string, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	root := NewRoot(&out, io.Discard)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--crew", crewFile}, args...))
	err := root.Execute()

	return out.String(), err
}

// certAs gives this machine's node a certificate issued ago in the past, for life.
func certAs(t *testing.T, ago, life time.Duration) mesh.Cert {
	t.Helper()

	c, err := mesh.Renew(meshDir(), time.Now().Add(-ago), life)
	require.NoError(t, err)

	return c
}

// A node's own certificate and the revocation list show in zmina node ls, and the way the owner reads them in doctor.
func TestNodeLsAndDoctorSayWhereTheCertificateStands(t *testing.T) {
	def, _ := nodeHome(t)

	check := func() (level, detail string) {
		var lines []check
		a := &app{crew: def, out: io.Discard, err: io.Discard}
		a.checkNode(func(name, level, format string, args ...any) {
			lines = append(lines, checkLine(name, level, format, args...))
		})
		require.NotEmpty(t, lines)

		return lines[0].Level, lines[0].Detail
	}

	level, detail := check()
	assert.Equal(t, "ok", level)
	assert.Regexp(t, `^laptop: certificate expires \d{4}-\d\d-\d\d \(in 365 days\)$`, detail)

	// 25 days to go: the warning window.
	certAs(t, 340*certTestDay, 365*certTestDay)
	level, detail = check()
	assert.Equal(t, "warn", level)
	assert.Contains(t, detail, "(in 25 days): on the owner's node run zmina node renew laptop")
	out, _, err := zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "  certificate [warn]: expires ")
	assert.Contains(t, out, "zmina node renew laptop")

	// Past it: a failure, and what to run.
	certAs(t, 400*certTestDay, 365*certTestDay)
	level, detail = check()
	assert.Equal(t, "fail", level)
	assert.Contains(t, detail, "expired ")
	assert.Contains(t, detail, "35 days ago")
	assert.Contains(t, detail, "zmina node renew laptop")

	// One issued before certificates expired: it counts and warns that it never runs out.
	certAs(t, 10*certTestDay, 0)
	level, detail = check()
	assert.Equal(t, "warn", level)
	assert.Contains(t, detail, "no expiry (issued before certificates expired)")
	out, _, err = zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "  certificate [warn]: no expiry (issued before certificates expired)")

	out, _, err = zm(t, def, "node", "ls", "--json")
	require.NoError(t, err)

	var v nodeView
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	require.NotNil(t, v.Cert)
	assert.Equal(t, "none", v.Cert.State)
	assert.Nil(t, v.Revoked)
}

// checkLine is a doctor line, built the way diagnose builds it.
func checkLine(name, level, format string, args ...any) check {
	return check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)}
}

// zmina node renew on the owner's node, for itself: the same key and name, a fresh expiry.
func TestNodeRenewOnThisNode(t *testing.T) {
	def, _ := nodeHome(t)
	was := certAs(t, 300*certTestDay, 365*certTestDay)

	out, _, err := zm(t, def, "node", "renew")
	require.NoError(t, err)
	assert.Regexp(t, `^node laptop · certificate renewed, expires \d{4}-\d\d-\d\d \(in 365 days\)\n$`, out)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	assert.True(t, me.Cert.Key.Equal(was.Key))
	assert.True(t, me.Cert.Newer(was))
	state, _ := me.Cert.State(time.Now())
	assert.Equal(t, mesh.CertValid, state)

	out, _, err = zm(t, def, "node", "renew", "--days", "0")
	require.NoError(t, err)
	assert.Equal(t, "node laptop · certificate renewed, no expiry\n", out)

	_, _, err = zm(t, def, "node", "renew", "--days", "-1")
	require.ErrorContains(t, err, "--days is 0 or more")
}

// Certificates are issued with a life by sign and by enroll's way; --days sets it, 0 leaves none.
func TestNodeSignDays(t *testing.T) {
	def, _ := nodeHome(t)

	far := t.TempDir()
	req, err := mesh.Init(far, "box")
	require.NoError(t, err)
	raw, _ := json.Marshal(req)

	for _, c := range []struct {
		args []string
		want func(g mesh.Grant)
	}{
		{nil, func(g mesh.Grant) { assert.NotEmpty(t, g.Cert.Expires) }},
		{[]string{"--days", "30"}, func(g mesh.Grant) {
			at, has, err := g.Cert.Expiry()
			require.NoError(t, err)
			require.True(t, has)
			assert.InDelta(t, 30*24, time.Until(at).Hours(), 1)
		}},
		{[]string{"--days", "0"}, func(g mesh.Grant) { assert.Empty(t, g.Cert.Expires) }},
	} {
		out, err := zmIn(t, def, string(raw), append([]string{"node", "sign"}, c.args...)...)
		require.NoError(t, err)

		var g mesh.Grant
		require.NoError(t, json.Unmarshal([]byte(out), &g))
		require.NoError(t, g.Cert.Verify(g.Owner))
		c.want(g)
	}

	_, err = zmIn(t, def, string(raw), "node", "sign", "--days", "-5")
	require.ErrorContains(t, err, "--days is 0 or more")
}

// enroll ends with the first session with the new node, so the node has said crew_sync and a crew push is not refused
// for want of a session. A session that cannot be held is a warning, not a failure: the node is certified and in the
// book, and the owner is told to run zmina node sync.
func TestNodeEnrollMeetsTheNode(t *testing.T) {
	for _, c := range []struct {
		name  string
		serve string // what the far node's shell does when asked to serve a session
		ok    bool
	}{
		{"reachable", ":", true},
		{"serving fails", "echo 'no session today' >&2; exit 1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			def, _ := nodeHome(t)

			homeB := t.TempDir()
			script := filepath.Join(t.TempDir(), "far-zmina")
			require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"+
				"if [ \"$1 $2\" = 'node serve' ]; then "+c.serve+"; fi\n"+
				"HOME='"+homeB+"' ZMINA_TELL_RECEIPTS_PROCESS=1 exec '"+os.Args[0]+
				"' -test.run='^TestTellReceiptsHelperProcess$' -- \"$@\"\n"), 0o700))

			out, errOut, err := zm(t, def, "node", "enroll", "box", "--cmd", script)
			require.NoError(t, err, "enroll has succeeded whatever the first session did")
			assert.Contains(t, out, "node box · certified, a peer at ")

			me, err := mesh.Open(meshDir())
			require.NoError(t, err)

			book, err := me.Peers()
			require.NoError(t, err)
			require.Len(t, book, 1)
			require.Equal(t, "box", book[0].Name)

			if c.ok {
				assert.Contains(t, out, "box: ok · ")
				assert.Empty(t, errOut)
				assert.False(t, book[0].OK.IsZero(), "the session is in the book")
				assert.Empty(t, crewSyncRefusal(book[0]), "a push to it is not refused")

				return
			}

			assert.NotContains(t, out, "box: ok")
			assert.Equal(t, 1, strings.Count(errOut, "\n"), "one line: %q", errOut)
			assert.Contains(t, errOut, "zmina: warning: no first session with box yet (")
			assert.Contains(t, errOut, "no session today")
			assert.Contains(t, errOut, "run zmina node sync")
			assert.NotEmpty(t, crewSyncRefusal(book[0]))
		})
	}
}

// renew for another node goes the way enroll went: its zmina is asked for its request through the book's address, the
// owner signs, the far side accepts. It keeps the key; an address that reaches another key is refused and nothing is
// signed; a node without the owner key says to renew from the owner's.
func TestNodeRenewOfAPeer(t *testing.T) {
	def, _ := nodeHome(t)
	homeA := os.Getenv("HOME")

	homeB := t.TempDir()
	boxMesh := filepath.Join(homeB, ".zmina", "mesh")
	r, err := mesh.Init(boxMesh, "box")
	require.NoError(t, err)
	g, err := mesh.SignRequestFor(filepath.Join(homeA, ".zmina", "mesh"), r, time.Now().Add(-400*certTestDay), 365*certTestDay)
	require.NoError(t, err)
	require.NoError(t, mesh.AcceptAt(boxMesh, g, time.Now().Add(-400*certTestDay)))

	script := func(home string) string {
		path := filepath.Join(t.TempDir(), "far-zmina")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nHOME='"+home+"' ZMINA_TELL_RECEIPTS_PROCESS=1 exec '"+
			os.Args[0]+"' -test.run='^TestTellReceiptsHelperProcess$' -- \"$@\"\n"), 0o700))

		return path
	}

	// Not met yet: nothing to check the key against.
	_, _, err = zm(t, def, "node", "peer", "add", "box", "--cmd", script(homeB))
	require.NoError(t, err)
	_, _, err = zm(t, def, "node", "renew", "box")
	require.ErrorContains(t, err, "no certificate of box in this node's book")

	// The owner met it once: the book holds its (now expired) certificate.
	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		for i := range ps {
			if ps[i].Name == "box" {
				ps[i].Cert = &g.Cert
			}
		}

		return ps
	}))

	out, _, err := zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "box · hand · at ", "the book's entry")
	assert.Contains(t, out, " · certificate expired (zmina node renew box)")

	// Another machine at that address, with a key of its own: refused, nothing signed.
	homeC := t.TempDir()
	_, err = mesh.Init(filepath.Join(homeC, ".zmina", "mesh"), "box")
	require.NoError(t, err)
	_, _, err = zm(t, def, "node", "peer", "add", "box", "--cmd", script(homeC))
	require.NoError(t, err)
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		for i := range ps {
			if ps[i].Name == "box" {
				ps[i].Addrs = []mesh.Addr{{Cmd: script(homeC)}}
			}
		}

		return ps
	}))
	_, _, err = zm(t, def, "node", "renew", "box")
	require.ErrorContains(t, err, "not box with the key this owner certified")

	// The right machine: renewed, same key, a new expiry; the owner's book holds the new certificate.
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		for i := range ps {
			if ps[i].Name == "box" {
				ps[i].Addrs = []mesh.Addr{{Cmd: script(homeB)}}
			}
		}

		return ps
	}))
	out, _, err = zm(t, def, "node", "renew", "box")
	require.NoError(t, err)
	assert.Regexp(t, `^node box · certificate renewed, expires \d{4}-\d\d-\d\d \(in 365 days\)\n$`, out)

	box, err := mesh.Open(boxMesh)
	require.NoError(t, err)
	assert.True(t, box.Cert.Key.Equal(g.Cert.Key))
	assert.True(t, box.Cert.Newer(g.Cert))
	require.NoError(t, box.Cert.Verify(me.Owner))
	require.NoError(t, box.Cert.Expired(time.Now()))

	book, err := me.Peers()
	require.NoError(t, err)
	for _, p := range book {
		if p.Name == "box" {
			assert.Equal(t, box.Cert.At, p.Cert.At)
		}
	}

	out, _, err = zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "certificate expired")

	// No address here: renewed by hand.
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		for i := range ps {
			if ps[i].Name == "box" {
				ps[i].Addrs = nil
			}
		}

		return ps
	}))
	_, _, err = zm(t, def, "node", "renew", "box")
	require.ErrorContains(t, err, "has no address here")

	// A node without the owner key says where to renew.
	t.Setenv("HOME", homeB)
	_, _, err = zm(t, filepath.Join(homeB, ".zmina", "crew.yaml"), "node", "renew")
	require.ErrorContains(t, err, "does not hold the owner key: renew from the owner's node (zmina node renew box there)")
}

// zmina node revoke: the key of the certificate the book holds goes on the list; the list shows in node ls and doctor;
// revoking twice, an unknown node or this node says why not.
func TestNodeRevoke(t *testing.T) {
	def, _ := nodeHome(t)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	far := t.TempDir()
	r, err := mesh.Init(far, "box")
	require.NoError(t, err)
	g, err := mesh.SignRequest(meshDir(), r, time.Now())
	require.NoError(t, err)
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		return append(ps, mesh.Peer{Name: "box", Cert: &g.Cert, Source: "hand", Addrs: []mesh.Addr{{Cmd: "false"}}})
	}))

	out, _, err := zm(t, def, "node", "revoke", "box")
	require.NoError(t, err)
	assert.Contains(t, out, "revoked box (key "+keyWord(g.Cert.Key)+"…) · revocation list version 1, 1 keys.")

	out, _, err = zm(t, def, "node", "revoke", "box")
	require.NoError(t, err)
	assert.Equal(t, "box is on the revocation list already (version 1, 1 keys)\n", out)

	out, _, err = zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "  revoked (list version 1): box (key "+keyWord(g.Cert.Key)+"…)\n")
	assert.Contains(t, out, " · key revoked")

	out, _, err = zm(t, def, "node", "ls", "--json")
	require.NoError(t, err)

	var v nodeView
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	require.NotNil(t, v.Revoked)
	assert.EqualValues(t, 1, v.Revoked.Version)

	_, _, err = zm(t, def, "node", "revoke", "nobody")
	require.ErrorContains(t, err, "has not met that node")
	_, _, err = zm(t, def, "node", "revoke", "laptop")
	require.ErrorContains(t, err, "cannot revoke itself")

	var lines []check
	a := &app{crew: def, out: io.Discard, err: io.Discard}
	a.checkNode(func(name, level, format string, args ...any) {
		lines = append(lines, checkLine(name, level, format, args...))
	})

	var seen []string
	for _, l := range lines {
		seen = append(seen, l.Detail)
	}

	assert.Contains(t, seen, "revocation list version 1: 1 keys revoked")
}

// The ticker says, at most once a day, that a certificate is near its end or past it: an event and a health note on
// this crew; nothing while it is fine. On the owner's node the peers' certificates are said too.
func TestCertBeatSaysItOncePerDay(t *testing.T) {
	def, _ := nodeHome(t)
	a := &app{crew: def, out: io.Discard, err: io.Discard}
	c, err := crew.Load(def)
	require.NoError(t, err)

	heard := func() ([]events.Event, []string) {
		evs, err := eventLog(c).Read(events.Filter{})
		require.NoError(t, err)

		var notes []string

		st, err := shift.Read(c.StateDir())
		if err == nil {
			notes = st.Notes
		}

		var certs []events.Event

		for _, e := range evs {
			if e.Event == "node-cert" {
				certs = append(certs, e)
			}
		}

		return certs, notes
	}

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	now := time.Now()

	a.certBeat(me, now)
	evs, notes := heard()
	assert.Empty(t, evs, "a fresh certificate says nothing")
	assert.Empty(t, notes)

	// 25 days from its end.
	certAs(t, 340*certTestDay, 365*certTestDay)
	me, err = mesh.Open(meshDir())
	require.NoError(t, err)

	a.certBeat(me, now)
	evs, notes = heard()
	require.Len(t, evs, 1)
	assert.Contains(t, evs[0].Detail, "this node's certificate expires ")
	assert.Contains(t, evs[0].Detail, "(in 25 days): on the owner's node run zmina node renew laptop")
	require.Len(t, notes, 1)
	assert.True(t, strings.HasPrefix(notes[0], "mesh: this node's certificate expires "), notes[0])

	a.certBeat(me, now.Add(time.Hour))
	a.certBeat(me, now.Add(23*time.Hour))
	evs, _ = heard()
	assert.Len(t, evs, 1, "once a day, however many beats")

	a.certBeat(me, now.Add(25*time.Hour))
	evs, _ = heard()
	assert.Len(t, evs, 2, "tomorrow it is said again")

	// A peer's certificate, past its end: the owner's node says so too.
	far := t.TempDir()
	r, err := mesh.Init(far, "box")
	require.NoError(t, err)
	g, err := mesh.SignRequestFor(meshDir(), r, now.Add(-400*certTestDay), 365*certTestDay)
	require.NoError(t, err)
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		return append(ps, mesh.Peer{Name: "box", Cert: &g.Cert, Source: "hand"})
	}))

	a.certBeat(me, now.Add(50*time.Hour))
	evs, _ = heard()
	require.Len(t, evs, 3)
	assert.Contains(t, evs[2].Detail, "the certificate of box expired ")
	assert.Contains(t, evs[2].Detail, "zmina node renew box")
}

// zmina doctor carries the node's lines on a machine that is a node, and none on one that is not.
func TestDoctorShowsTheNodeOnlyOnANode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZMINA_ROLE", "")

	file := filepath.Join(home, ".zmina", "crew.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	require.NoError(t, os.WriteFile(file, []byte("namespace: "+filepath.Join(home, "ns")+"\ndir: "+home+
		"\nclaude: /usr/bin/true\nremote_control: false\nroles:\n  dispatcher:\n    boot: hello\n"), 0o600))

	nodeLines := func() []check {
		var out []check

		for _, c := range (&app{crew: file, out: io.Discard, err: io.Discard}).diagnose() {
			if c.Name == "node" {
				out = append(out, c)
			}
		}

		return out
	}

	assert.Empty(t, nodeLines(), "not a node: nothing")

	_, _, err := zm(t, file, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	lines := nodeLines()
	require.Len(t, lines, 1)
	assert.Equal(t, "ok", lines[0].Level)
	assert.Contains(t, lines[0].Detail, "laptop: certificate expires ")
}

// A certificate with an expiry is not issued while a peer this node met says no terminal_v2: a zmina from before
// expiries would refuse its holder. The old form (--days 0) still goes, and a peer not met is not asked about.
func TestExpiringCertificatesWaitForOlderPeers(t *testing.T) {
	def, _ := nodeHome(t)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	peer := func(name string, met, v2 bool) mesh.Peer {
		r, err := mesh.Init(t.TempDir(), name)
		require.NoError(t, err)

		g, err := mesh.SignRequest(meshDir(), r, time.Now())
		require.NoError(t, err)

		p := mesh.Peer{Name: name, Cert: &g.Cert, Source: "hand", TerminalV2: v2}
		if met {
			p.OK = time.Now()
		}

		return p
	}

	older := peer("older", true, false)
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		return append(ps, older, peer("newer", true, true), peer("unmet", false, false))
	}))

	_, _, err = zm(t, def, "node", "renew")
	require.ErrorContains(t, err, "older did not say terminal_v2 in its last session")
	assert.NotContains(t, err.Error(), "newer")
	assert.NotContains(t, err.Error(), "unmet")
	assert.ErrorContains(t, err, "--days 0")

	req, _ := json.Marshal(mesh.Request{Name: "far", Key: older.Cert.Key})
	_, err = zmIn(t, def, string(req), "node", "sign")
	require.ErrorContains(t, err, "older did not say terminal_v2")

	_, _, err = zm(t, def, "node", "enroll", "far", "--cmd", "false")
	require.ErrorContains(t, err, "older did not say terminal_v2")

	out, _, err := zm(t, def, "node", "renew", "--days", "0")
	require.NoError(t, err)
	assert.Equal(t, "node laptop · certificate renewed, no expiry\n", out)

	// It updated: the next session said so.
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		for i := range ps {
			if ps[i].Name == "older" {
				ps[i].TerminalV2 = true
			}
		}

		return ps
	}))
	_, _, err = zm(t, def, "node", "renew")
	require.NoError(t, err)
}
