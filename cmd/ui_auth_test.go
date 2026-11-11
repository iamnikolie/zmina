package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSessions is a session store in a file of the test's own, never the home's.
func testSessions(t *testing.T) *uiSessions {
	t.Helper()

	return newUISessions(filepath.Join(t.TempDir(), "ui-sessions.json"), time.Now)
}

// uiTestAuth is the auth of a handler under test: sessions of its own, the real clock.
func uiTestAuth(t *testing.T, token string, logins *uiLogins, signin *uiSignin) *uiAuth {
	t.Helper()

	return newUIAuth(token, logins, signin, testSessions(t))
}

// testGuard is guardLogins over a token with no codes.
func testGuard(t *testing.T, next http.Handler, bound string, loopback bool, token string, allowHosts ...string) http.Handler {
	t.Helper()

	return guardLogins(next, bound, loopback, uiTestAuth(t, token, nil, nil), allowHosts...)
}

// signInCookie signs a browser in through the ?token= link of the page at url and returns the cookie it is given.
func signInCookie(t *testing.T, h http.Handler, url, token string) *http.Cookie {
	t.Helper()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url+"?token="+token, nil))
	require.Equal(t, http.StatusSeeOther, w.Code, "the link signs in and sends the browser on")

	for _, c := range w.Result().Cookies() {
		if c.Name == uiCookie {
			return c
		}
	}

	require.FailNow(t, "no session cookie")

	return nil
}

const authToken = "example-token-1234"

// authGuard is a guard over a token with codes, a clock the test moves, and a handler that tells what reached it.
func authGuard(t *testing.T, signin *uiSignin) (http.Handler, *uiAuth, *uiClock, *[]string) {
	t.Helper()

	clock := newUIClock()
	auth := newUIAuth(authToken, newUILogins(), signin, newUISessions(filepath.Join(t.TempDir(), "ui-sessions.json"), clock.now))
	auth.limiter = newUILimiter(clock.now)

	var (
		mu      sync.Mutex
		reached []string
	)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = append(reached, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	return guardLogins(next, "127.0.0.1:7518", true, auth), auth, clock, &reached
}

func authCall(h http.Handler, method, path string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:7518"+path, nil)
	r.RemoteAddr = "192.0.2.50:4000"
	r.Header.Set("X-Zmina", "1")

	if mod != nil {
		mod(r)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

// TestOldCookieThatCarriesTheTokenIsNotAcceptedAndIsTakenOff: devices sign in once more after this change.
func TestOldCookieThatCarriesTheTokenIsNotAcceptedAndIsTakenOff(t *testing.T) {
	h, _, _, reached := authGuard(t, nil)

	for _, path := range []string{"/", "/api/state", "/api/stream", "/api/terminal/connect"} {
		w := authCall(h, http.MethodGet, path, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: uiCookie, Value: authToken}) })
		assert.Equal(t, http.StatusUnauthorized, w.Code, path)

		var gone *http.Cookie

		for _, c := range w.Result().Cookies() {
			if c.Name == uiCookie {
				gone = c
			}
		}

		require.NotNil(t, gone, "%s: the browser is told to drop the cookie that held the token", path)
		assert.Empty(t, gone.Value)
		assert.Negative(t, gone.MaxAge)
	}

	assert.Empty(t, *reached)
}

// TestTokenOnAPageStartsASessionAndNeverGoesInACookie covers the one sign-in with the token a browser makes.
func TestTokenOnAPageStartsASessionAndNeverGoesInACookie(t *testing.T) {
	h, auth, _, reached := authGuard(t, nil)

	// Only a page link signs in: not a script's path, not a write.
	assert.Equal(t, http.StatusUnauthorized, authCall(h, http.MethodGet, "/api/state?token="+authToken, nil).Code)
	assert.Equal(t, http.StatusUnauthorized, authCall(h, http.MethodPost, "/api/todo?token="+authToken, nil).Code)
	assert.Empty(t, *reached)

	w := authCall(h, http.MethodGet, "/chat.html?tab=2&token="+authToken, nil)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/chat.html?tab=2", w.Header().Get("Location"))

	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.NotContains(t, cookies[0].Value, authToken)
	assert.True(t, auth.sessions.valid(cookies[0].Value))

	w = authCall(h, http.MethodGet, "/api/state", func(r *http.Request) { r.AddCookie(cookies[0]) })
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Result().Cookies(), "no cookie renewed: the server keeps the session")

	// A write still needs the page's own header.
	w = authCall(h, http.MethodPost, "/api/todo", func(r *http.Request) { r.Header.Del("X-Zmina"); r.AddCookie(cookies[0]) })
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestWrongTokensAreLimitedWhatEverTheWayIn: a wrong token on a page link, in a Bearer header or as a login code
// counts toward one limit; past it even the right token is refused, until the wait is over.
func TestWrongTokensAreLimitedWhatEverTheWayIn(t *testing.T) {
	h, _, clock, reached := authGuard(t, nil)

	wrong := []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return authCall(h, http.MethodGet, "/?token=guess", nil) },
		func() *httptest.ResponseRecorder {
			return authCall(h, http.MethodGet, "/api/state", func(r *http.Request) { r.Header.Set("Authorization", "Bearer guess") })
		},
		func() *httptest.ResponseRecorder { return authCall(h, http.MethodGet, "/?login=guess", nil) },
	}

	for i := range uiFreeFails {
		w := wrong[i%len(wrong)]()
		assert.NotEqual(t, http.StatusTooManyRequests, w.Code, "try %d", i+1)
		assert.NotEqual(t, http.StatusOK, w.Code)
	}

	for _, try := range wrong {
		w := try()
		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, "5", w.Header().Get("Retry-After"))
	}

	good := func() *httptest.ResponseRecorder {
		return authCall(h, http.MethodGet, "/api/state", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+authToken) })
	}

	assert.Equal(t, http.StatusTooManyRequests, good().Code, "the right token waits too, and is not looked at")
	assert.Empty(t, *reached)

	// Another address is not held up.
	assert.Equal(t, http.StatusOK, authCall(h, http.MethodGet, "/api/state", func(r *http.Request) {
		r.RemoteAddr = "192.0.2.51:4000"
		r.Header.Set("Authorization", "Bearer "+authToken)
	}).Code)

	clock.add(5 * time.Second)
	assert.Equal(t, http.StatusOK, good().Code)
	assert.Equal(t, http.StatusOK, good().Code, "a right token clears the address")
}

// TestWrongSignInCodesAreLimitedToo: the six digits are guessable, so the limiter counts them as well.
func TestWrongSignInCodesAreLimitedToo(t *testing.T) {
	signin := newUISignin("", func(string) {})
	h, _, clock, _ := authGuard(t, signin)

	code, _, err := signin.connect()
	require.NoError(t, err)

	verify := func(code string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7518/api/signin/verify", strings.NewReader(`{"code":"`+code+`"}`))
		r.RemoteAddr = "192.0.2.50:4000"
		r.Header.Set("X-Zmina", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		return w
	}

	// Each wrong try is also one of the code's own five; a new code each time keeps the test on the limiter.
	for i := range uiFreeFails {
		assert.Equal(t, http.StatusUnauthorized, verify(wrongCode(code)).Code, "try %d", i+1)
	}

	w := verify(code)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "the right code waits too")
	assert.Equal(t, "5", w.Header().Get("Retry-After"))

	clock.add(5 * time.Second)

	w = verify(code)
	require.Equal(t, http.StatusUnauthorized, w.Code, "the code was spent by its own five wrong tries")
}

func wrongCode(code string) string {
	if code == "000000" {
		return "111111"
	}

	return "000000"
}

// TestEndedSessionIsRefusedAndItsStreamsAreClosed: a session ended with zmina ui sessions (or sign-out) stops
// serving at once, and what it has open — the page's stream, a terminal — is closed within the watch interval.
func TestEndedSessionIsRefusedAndItsStreamsAreClosed(t *testing.T) {
	clock := newUIClock()
	auth := newUIAuth(authToken, nil, nil, newUISessions(filepath.Join(t.TempDir(), "ui-sessions.json"), clock.now))
	auth.watch = 5 * time.Millisecond

	opened := make(chan struct{})
	closed := make(chan struct{})

	h := guardLogins(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stream" {
			close(opened)
			<-r.Context().Done()
			close(closed)
		}
	}), "127.0.0.1:7518", true, auth)

	cookie := signInCookie(t, h, "http://127.0.0.1:7518/", authToken)

	go authCall(h, http.MethodGet, "/api/stream", func(r *http.Request) { r.AddCookie(cookie) })

	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not open")
	}

	select {
	case <-closed:
		t.Fatal("the stream closed while its session stands")
	case <-time.After(50 * time.Millisecond):
	}

	other := newUISessions(auth.sessions.path, clock.now)
	n, err := other.revokeAll()
	require.NoError(t, err)
	require.Equal(t, 1, n)

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream of an ended session stayed open")
	}

	assert.Equal(t, http.StatusUnauthorized, authCall(h, http.MethodGet, "/api/state", func(r *http.Request) { r.AddCookie(cookie) }).Code)
}

// TestSignOutEndsTheSessionAndDropsTheCookie goes through the real handler: POST /api/signout.
func TestSignOutEndsTheSessionAndDropsTheCookie(t *testing.T) {
	uiHome(t)

	a := &app{out: io.Discard, err: io.Discard, uiSessions: testSessions(t)}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = a.uiHandler(srv.Listener.Addr().String(), true, authToken)
	srv.Start()
	t.Cleanup(srv.Close)

	login, err := noRedirect().Get(srv.URL + "/?token=" + authToken)
	require.NoError(t, err)
	_ = login.Body.Close()
	require.Len(t, login.Cookies(), 1)

	cookie := login.Cookies()[0]

	do := func(method, path string, withHeader bool) *http.Response {
		req, err := http.NewRequest(method, srv.URL+path, nil)
		require.NoError(t, err)

		if withHeader {
			req.Header.Set("X-Zmina", "1")
		}

		req.AddCookie(cookie)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		return resp
	}

	require.Equal(t, http.StatusOK, do(http.MethodGet, "/api/ping", false).StatusCode)
	assert.Equal(t, http.StatusForbidden, do(http.MethodPost, "/api/signout", false).StatusCode, "a write needs the page's header")
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/api/ping", false).StatusCode)

	out := do(http.MethodPost, "/api/signout", true)
	require.Equal(t, http.StatusOK, out.StatusCode)

	var dropped bool

	for _, c := range out.Cookies() {
		dropped = dropped || (c.Name == uiCookie && c.Value == "" && c.MaxAge < 0)
	}

	assert.True(t, dropped, "the cookie is taken off the browser")
	assert.Equal(t, http.StatusUnauthorized, do(http.MethodGet, "/api/ping", false).StatusCode, "the session is gone")
}

// TestSessionsSurviveARestartOfTheCockpit: the file is the store, so a new handler over it knows the cookie.
func TestSessionsSurviveARestartOfTheCockpit(t *testing.T) {
	sessions := testSessions(t)
	first := guardLogins(http.NotFoundHandler(), "127.0.0.1:7518", true, newUIAuth(authToken, nil, nil, sessions))
	cookie := signInCookie(t, first, "http://127.0.0.1:7518/", authToken)

	again := newUISessions(sessions.path, time.Now)
	second := guardLogins(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		"127.0.0.1:7518", true, newUIAuth("another-token-1234", nil, nil, again))

	assert.Equal(t, http.StatusOK, authCall(second, http.MethodGet, "/api/state", func(r *http.Request) { r.AddCookie(cookie) }).Code)
}
