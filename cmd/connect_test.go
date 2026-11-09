package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"rsc.io/qr"
)

// lockedLog collects a cockpit's log lines across its handler goroutines.
type lockedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lockedLog) add(line string) { l.mu.Lock(); l.lines = append(l.lines, line); l.mu.Unlock() }

func (l *lockedLog) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return strings.Join(l.lines, "\n")
}

// connectCockpit is a running cockpit with a token and no notifier, recorded in ui.json as this machine's.
func connectCockpit(t *testing.T, token string) (*httptest.Server, *app, *lockedLog) {
	t.Helper()

	logs := &lockedLog{}
	a := &app{out: io.Discard, err: io.Discard, uiLogins: newUILogins()}
	if token != "" {
		a.uiSignin = newUISignin("", logs.add)
	}

	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = a.uiHandler(srv.Listener.Addr().String(), true, token)
	srv.Start()
	t.Cleanup(srv.Close)
	require.NoError(t, writeUIState(srv.URL+"/", a.uiLogins))

	return srv, a, logs
}

func runConnect(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var out, errw strings.Builder

	root := NewRoot(&out, &errw)
	root.SetArgs(append([]string{"connect"}, args...))
	err := root.Execute()

	return out.String(), errw.String(), err
}

// The tailnet and link-local addresses the tests use, built so no address of a real network sits in the source.
var (
	tailA     = net.IPv4(100, 101, 1, 2).String()
	tailB     = net.IPv4(100, 101, 9, 9).String()
	linkLocal = net.IPv4(169, 254, 3, 4).String()
)

var connectCode = regexp.MustCompile(`(?m)^Code: (\d{6})$`)

func verify(t *testing.T, url, code string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url+"/api/signin/verify", strings.NewReader(`{"code":"`+code+`"}`))
	require.NoError(t, err)
	req.Header.Set("X-Zmina", "1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	return resp
}

// TestConnectPrintsACodeThatSignsInOnce: zmina connect prints the cockpit's address and a code made by the running
// cockpit; the code signs a browser in once, and is in no log line.
func TestConnectPrintsACodeThatSignsInOnce(t *testing.T) {
	uiHome(t)

	srv, _, logs := connectCockpit(t, "example-token")

	out, _, err := runConnect(t)
	require.NoError(t, err)
	assert.Contains(t, out, srv.URL+"/")
	assert.Contains(t, out, "this machine")

	m := connectCode.FindStringSubmatch(out)
	require.Len(t, m, 2, out)
	code := m[1]
	assert.True(t, strings.HasSuffix(out, "\nCode: "+code+"\n"), "the code is the last line: %q", out)

	assert.NotContains(t, logs.all(), code, "the code is never logged")
	assert.Contains(t, logs.all(), "code made for zmina connect")

	raw, err := os.ReadFile(uiStatePath())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), code, "the code is in no file")

	assert.Equal(t, http.StatusUnauthorized, verify(t, srv.URL, wrong(code)).StatusCode)

	good := verify(t, srv.URL, code)
	require.Equal(t, http.StatusOK, good.StatusCode)
	require.Len(t, good.Cookies(), 1)
	assert.Equal(t, "example-token", good.Cookies()[0].Value)

	assert.Equal(t, http.StatusUnauthorized, verify(t, srv.URL, code).StatusCode, "single use")

	_, _, err = runConnect(t)
	require.Error(t, err, "one a minute")
	assert.Contains(t, err.Error(), "wait")
}

func TestConnectWithNoCockpitSaysHowToStartOne(t *testing.T) {
	uiHome(t)

	_, _, err := runConnect(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no zmina ui runs on this machine")
	assert.Contains(t, err.Error(), "ZMINA_UI_TOKEN=")

	// Recorded, but gone.
	srv, _, _ := connectCockpit(t, "example-token")
	srv.Close()

	_, _, err = runConnect(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not answer")
}

func TestConnectToACockpitWithNoToken(t *testing.T) {
	uiHome(t)

	connectCockpit(t, "")

	_, _, err := runConnect(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no token")
}

// TestConnectNeedsTheSecretAndThisMachine: the connect call gives a code only to the holder of the handoff secret,
// asking from this machine; a guess, or the right secret from elsewhere, gets nothing.
func TestConnectNeedsTheSecretAndThisMachine(t *testing.T) {
	logins := newUILogins()
	s := newUISignin("", func(string) {})
	h := guardLogins(http.NotFoundHandler(), tailA+":7517", false, "example-token", logins, s,
		"zmina.example.ts.net")

	var extra func(r *http.Request)

	call := func(secret, remote, local string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://"+tailA+":7517/api/signin/connect", nil)
		r.Header.Set("X-Zmina", "1")
		r.Header.Set("X-Zmina-Handoff", secret)
		if extra != nil {
			extra(r)
		}
		r.RemoteAddr = remote
		if local != "" {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
				&net.TCPAddr{IP: net.ParseIP(local), Port: 7517}))
		}

		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		return w
	}

	assert.Equal(t, http.StatusForbidden, call("guess", "127.0.0.1:5000", "").Code)
	assert.Equal(t, http.StatusForbidden, call("", "127.0.0.1:5000", "").Code)
	assert.Equal(t, http.StatusForbidden, call(logins.handoff, tailB+":5000", tailA).Code,
		"another machine on the tailnet")

	// Through a local reverse proxy every request arrives over loopback: a forwarding header or a proxy's Host
	// says it came from elsewhere.
	// A proxied request that looks local and has no secret, or a wrong one: a bare 403, no code.
	extra = func(r *http.Request) { r.Header.Set("X-Forwarded-For", tailB) }
	for _, secret := range []string{"", "guess"} {
		w := call(secret, "127.0.0.1:5000", "")
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, "forbidden\n", w.Body.String(), "no detail on a miss")
	}

	assert.Equal(t, http.StatusForbidden, call(logins.handoff, "127.0.0.1:5000", "").Code, "forwarded")
	extra = func(r *http.Request) { r.Header.Set("Forwarded", "for="+tailB) }
	assert.Equal(t, http.StatusForbidden, call(logins.handoff, "127.0.0.1:5000", "").Code, "forwarded")
	extra = func(r *http.Request) { r.Host = "zmina.example.ts.net" }
	assert.Equal(t, http.StatusForbidden, call(logins.handoff, "127.0.0.1:5000", "").Code, "an --allow-host host")
	extra = nil

	// The handoff secret mints a login code only from this machine too.
	hr := httptest.NewRequest(http.MethodPost, "http://"+tailA+":7517/api/handoff", nil)
	hr.Header.Set("X-Zmina", "1")
	hr.Header.Set("X-Zmina-Handoff", logins.handoff)
	hr.RemoteAddr = tailB + ":5000"
	hw := httptest.NewRecorder()
	h.ServeHTTP(hw, hr)
	assert.Equal(t, http.StatusForbidden, hw.Code, "the secret from another machine")

	assert.Empty(t, s.term.code, "no code was made")

	w := call(logins.handoff, tailA+":5000", tailA)
	require.Equal(t, http.StatusOK, w.Code, "this machine over its own tailnet address")
	assert.Regexp(t, `"code":"\d{6}"`, w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	r := httptest.NewRequest(http.MethodGet, "http://"+tailA+":7517/api/signin/connect", nil)
	r.Header.Set("X-Zmina-Handoff", logins.handoff)
	r.RemoteAddr = "127.0.0.1:5000"
	g := httptest.NewRecorder()
	h.ServeHTTP(g, r)
	assert.Equal(t, http.StatusForbidden, g.Code, "POST with X-Zmina only")
}

// TestSigninSlotsAreApart: a code the page asks the notifier for never replaces the one zmina connect printed, and
// the other way round; a wrong code counts on both; a right one spends only its own.
func TestSigninSlotsAreApart(t *testing.T) {
	f := newSigninFix()

	term, _, err := f.s.connect()
	require.NoError(t, err)
	_, err = f.s.request(context.Background())
	require.NoError(t, err)
	mail := f.code(t)

	if term != mail {
		assert.True(t, f.s.check(mail))
		assert.True(t, f.s.check(term), "the page's code left the connect code live")
	}

	f.now = f.now.Add(signinGap)
	term, _, err = f.s.connect()
	require.NoError(t, err)
	_, err = f.s.request(context.Background())
	require.NoError(t, err)
	mail = f.code(t)

	bad := "000000"
	for bad == term || bad == mail {
		bad = string(rune(bad[0]+1)) + bad[1:]
	}

	for range signinTries {
		assert.False(t, f.s.check(bad))
	}

	assert.False(t, f.s.check(term), "five wrong tries end the connect code")
	assert.False(t, f.s.check(mail), "and the notifier's")
}

func TestSigninWithNoNotifier(t *testing.T) {
	s := newUISignin("", func(string) {})
	h := guardLogins(http.NotFoundHandler(), "127.0.0.1:7518", true, "example-token", newUILogins(), s)

	do := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:7518"+path, nil)
		r.Header.Set("X-Zmina", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		return w
	}

	page := do(http.MethodGet, "/")
	assert.Equal(t, http.StatusUnauthorized, page.Code)
	assert.Contains(t, page.Body.String(), "zmina connect", "the sign-in page, with the way to a code")

	info := do(http.MethodGet, "/api/signin")
	require.Equal(t, http.StatusOK, info.Code)
	assert.JSONEq(t, `{"send":false}`, info.Body.String())

	assert.Equal(t, http.StatusNotFound, do(http.MethodPost, "/api/signin/code").Code, "nothing to send a code with")

	withSend := newSigninFix()
	h = guardLogins(http.NotFoundHandler(), "127.0.0.1:7518", true, "example-token", nil, withSend.s)
	info = do(http.MethodGet, "/api/signin")
	assert.JSONEq(t, `{"send":true}`, info.Body.String())
}

func TestCockpitAddrs(t *testing.T) {
	was := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = was })

	interfaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range []string{"127.0.0.1/8", "192.0.2.5/24", linkLocal + "/16", tailA + "/32", "fe80::1/64"} {
			ip, n, err := net.ParseCIDR(c)
			require.NoError(t, err)
			n.IP = ip
			out = append(out, n)
		}

		return out, nil
	}

	assert.Equal(t, []cockpitAddr{
		{URL: "http://" + tailA + ":7517/", Kind: addrTailnet},
		{URL: "http://192.0.2.5:7517/", Kind: addrNetwork},
		{URL: "https://zmina.example.ts.net/", Kind: addrProxy},
		{URL: "http://127.0.0.1:7517/", Kind: addrLocal},
	}, cockpitAddrs("0.0.0.0:7517", []string{"zmina.example.ts.net"}))

	assert.Equal(t, []cockpitAddr{{URL: "http://" + tailA + ":7517/", Kind: addrTailnet}},
		cockpitAddrs(tailA+":7517", nil))
	assert.Equal(t, []cockpitAddr{{URL: "http://127.0.0.1:7517/", Kind: addrLocal}},
		cockpitAddrs("127.0.0.1:7517", nil))
	assert.Contains(t, startUIHint(), "--addr "+tailA+":7517")
}

func TestQRBlockDrawsTheAddress(t *testing.T) {
	q, err := qrBlock("http://" + tailA + ":7517/")
	require.NoError(t, err)

	code, err := qr.Encode("http://"+tailA+":7517/", qr.M)
	require.NoError(t, err)

	// Each cell is the upper half block in the top module's colour on the bottom one's: 0 black, 7 white.
	cell := regexp.MustCompile(`\x1b\[3(\d);4(\d)m▀`)
	lines := strings.Split(strings.TrimSuffix(q, "\n"), "\n")
	n := code.Size + 4
	require.Len(t, lines, (n+1)/2)

	black := func(x, y int) bool {
		x, y = x-2, y-2

		return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
	}

	for row, line := range lines {
		cells := cell.FindAllStringSubmatch(line, -1)
		require.Len(t, cells, n)

		for x, c := range cells {
			assert.Equal(t, black(x, 2*row), c[1] == "0", "module %d,%d", x, 2*row)
			assert.Equal(t, black(x, 2*row+1), c[2] == "0", "module %d,%d", x, 2*row+1)
		}
	}
}

// The handoff secret goes only to a recorded cockpit that still runs, at an address of this machine.
func TestRecordedCockpitMustBeHereAndAlive(t *testing.T) {
	uiHome(t)

	was := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = was })
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(tailA), Mask: net.CIDRMask(32, 32)}}, nil
	}

	write := func(url string, pid int) {
		raw, err := json.Marshal(uiRunning{URL: url, PID: pid, Handoff: "secret"})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(uiStatePath(), raw, 0o600))
	}

	require.NoError(t, os.MkdirAll(filepath.Dir(uiStatePath()), 0o700))

	write("http://"+tailA+":7517/", os.Getpid())
	_, ok := recordedCockpit()
	assert.True(t, ok, "this machine's tailnet address")

	write("http://127.0.0.1:7517/", os.Getpid())
	_, ok = recordedCockpit()
	assert.True(t, ok)

	write("http://192.0.2.40:7517/", os.Getpid())
	_, ok = recordedCockpit()
	assert.False(t, ok, "an address this machine no longer holds")

	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	write("http://127.0.0.1:7517/", cmd.Process.Pid)
	_, ok = recordedCockpit()
	assert.False(t, ok, "a cockpit no longer running")

	_, _, err := runConnect(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no zmina ui runs on this machine")
}
