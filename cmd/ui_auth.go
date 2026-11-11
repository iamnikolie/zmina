package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// uiAuth is what a cockpit request is checked against. A browser signs in once, with the token on a page link, a
// one-time login code or a six-digit sign-in code, and then holds a session (uiSessions): its cookie carries the
// session's id, never the token. A script may present the token itself as a Bearer header, request by request. Every
// secret presented is counted by the limiter first. Every path into the cockpit goes through gate, the streams and
// the terminal socket included.
type uiAuth struct {
	token    string
	logins   *uiLogins // nil: no one-time login codes
	signin   *uiSignin // nil: no sign-in page
	sessions *uiSessions
	limiter  *uiLimiter
	// watch is how often a stream that is open checks that its session was not ended.
	watch time.Duration
}

// newUIAuth is the auth of a cockpit with this token; nil sessions are the file under ~/.zmina.
func newUIAuth(token string, logins *uiLogins, signin *uiSignin, sessions *uiSessions) *uiAuth {
	if sessions == nil {
		sessions = newUISessions(uiSessionsPath(), time.Now)
	}

	return &uiAuth{token: token, logins: logins, signin: signin, sessions: sessions, limiter: newUILimiter(time.Now),
		watch: 5 * time.Second}
}

// tokenOK compares a presented token with the cockpit's in constant time, whatever their lengths.
func (a *uiAuth) tokenOK(given string) bool {
	want, got := sha256.Sum256([]byte(a.token)), sha256.Sum256([]byte(given))

	return a.token != "" && subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// bearer is the token a request presents in its Authorization header.
func bearer(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}

	return strings.TrimSpace(h[len(prefix):]), true
}

func isPage(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html"))
}

// tooMany answers a try the limiter refused.
func tooMany(w http.ResponseWriter, wait time.Duration) {
	secs := int((wait + time.Second - 1) / time.Second)

	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "too many wrong tries: wait "+strconv.Itoa(secs)+" s", http.StatusTooManyRequests)
}

// startSession signs the browser in: a session begun, its id in the cookie. False when it could not be kept (and the
// request was answered).
func (a *uiAuth) startSession(w http.ResponseWriter, r *http.Request, allowHosts []string) bool {
	id, err := a.sessions.start()
	if err != nil {
		http.Error(w, "the session could not be kept", http.StatusInternalServerError)

		return false
	}

	setUICookie(w, r, id, allowHosts)

	return true
}

// gate says whether the request is signed in. A session cookie signs it in. So does the token as a Bearer header,
// for that request only. On a page, ?token= starts a session and sends the browser to the page without it
// (answered). Elsewhere ?token= means nothing. A browser whose cookie is no session gets it taken off. answered is
// true when gate has already replied.
func (a *uiAuth) gate(w http.ResponseWriter, r *http.Request, allowHosts []string) (sid string, ok, answered bool) {
	if c, err := r.Cookie(uiCookie); err == nil && a.sessions.valid(c.Value) {
		return c.Value, true, false
	}

	addr := clientAddr(r, allowHosts)

	if given, has := bearer(r); has {
		if wait := a.limiter.take(addr); wait > 0 {
			tooMany(w, wait)

			return "", false, true
		}

		if a.tokenOK(given) {
			a.limiter.refund(addr)

			return "", true, false
		}

		return "", false, false
	}

	if given := r.URL.Query().Get("token"); given != "" && isPage(r) {
		if wait := a.limiter.take(addr); wait > 0 {
			tooMany(w, wait)

			return "", false, true
		}

		if !a.tokenOK(given) {
			return "", false, false
		}

		a.limiter.refund(addr)

		if a.startSession(w, r, allowHosts) {
			redirectWithout(w, r, "token")
		}

		return "", false, true
	}

	return "", false, false
}

// redirectWithout sends the browser to the page it asked for, without the query parameter that carried a secret, so
// the secret leaves the address bar and the history. Always a path on this cockpit: "//host/x" or "/\\host/x" would
// send the browser to another site, and so would a control character a browser strips ("/\t/host/x"): the path is
// encoded, a tab goes out as %09.
func redirectWithout(w http.ResponseWriter, r *http.Request, param string) {
	q := r.URL.Query()
	q.Del(param)

	target := (&url.URL{Path: "/" + strings.TrimLeft(r.URL.Path, "/\\"), RawQuery: q.Encode()}).String()

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// dropStaleCookie takes off a browser the cookie it sends, when it names no session: the token an earlier cockpit put
// there, or a session that ended. Signing in sets the cookie again.
func dropStaleCookie(w http.ResponseWriter, r *http.Request, allowHosts []string) {
	if _, err := r.Cookie(uiCookie); err != nil {
		return
	}

	http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		Expires: time.Unix(0, 0), Secure: secureUIRequest(r, allowHosts), SameSite: http.SameSiteStrictMode})
}

// streams are the requests that stay open: the page's stream and a terminal.
func streams(path string) bool { return path == "/api/stream" || path == "/api/terminal/connect" }

// watched is r, for a stream, with a context that ends when the session of sid is ended: a session ended with zmina ui
// sessions or sign-out closes what it has open. Other requests are short and return as they are.
func (a *uiAuth) watched(r *http.Request, sid string) (*http.Request, context.CancelFunc) {
	if sid == "" || !streams(r.URL.Path) || a.watch <= 0 {
		return r, func() {}
	}

	ctx, cancel := context.WithCancel(r.Context())

	go func() {
		tick := time.NewTicker(a.watch)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if !a.sessions.standing(sid) {
					cancel()

					return
				}
			}
		}
	}()

	return r.WithContext(ctx), cancel
}
