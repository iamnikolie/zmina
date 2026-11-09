package cmd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// One cockpit per machine. A running zmina ui writes ui.json (0600, next to the crews): its address, its pid and a
// handoff secret. A second zmina ui reads it, asks the running one for a one-time login code with that secret,
// and opens the page with ?login=<code>: the code works once, for a minute, so neither the token nor anything
// that lasts is left in a shell's history, a process's arguments or the browser's.

// uiLoginTTL is how long a login code works.
const uiLoginTTL = time.Minute

func uiStatePath() string { return crew.ExpandHome("~/.zmina/ui.json") }

type uiRunning struct {
	URL     string `json:"url"`
	PID     int    `json:"pid"`
	Handoff string `json:"handoff"`
}

// uiLogins hands out one-time login codes: to whoever shows the handoff secret, and to the server itself.
type uiLogins struct {
	handoff string

	mu    sync.Mutex
	codes map[string]time.Time
	now   func() time.Time
}

func newUILogins() *uiLogins {
	return &uiLogins{handoff: randomHex(32), codes: map[string]time.Time{}, now: time.Now}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

func (l *uiLogins) code() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	for c, until := range l.codes {
		if now.After(until) {
			delete(l.codes, c)
		}
	}

	c := randomHex(16)
	l.codes[c] = now.Add(uiLoginTTL)

	return c
}

// mint is a code for the holder of the handoff secret, else false.
func (l *uiLogins) mint(secret string) (string, bool) {
	if l.handoff == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(l.handoff)) != 1 {
		return "", false
	}

	return l.code(), true
}

// take spends a code: true once, within its minute.
func (l *uiLogins) take(code string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	until, ok := l.codes[code]
	delete(l.codes, code)

	return ok && !l.now().After(until)
}

// writeUIState records the running cockpit; dropUIState removes it while it is still this process's.
func writeUIState(url string, l *uiLogins) error {
	raw, err := json.Marshal(uiRunning{URL: url, PID: os.Getpid(), Handoff: l.handoff})
	if err != nil {
		return err
	}

	path := uiStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

func dropUIState() {
	var r uiRunning

	raw, err := os.ReadFile(uiStatePath())
	if err == nil && json.Unmarshal(raw, &r) == nil && r.PID == os.Getpid() {
		_ = os.Remove(uiStatePath())
	}
}

// runningCockpit is the address to open when a cockpit already runs on this machine: its page with a one-time
// login when it needs one. ok is false when none answers.
func runningCockpit() (string, bool) {
	r, ok := recordedCockpit()
	if !ok {
		return "", false
	}

	return cockpitHandoff(r.URL, r.Handoff, "")
}

// runningCockpitAt is runningCockpit for an explicit --addr: the handoff secret goes only to the cockpit recorded
// at that very address; any other one there is asked with the token. ok is false when neither signs in.
func runningCockpitAt(addr, token string) (string, bool) {
	url := "http://" + addr + "/"

	secret := ""
	if r, ok := recordedCockpit(); ok && r.URL == url {
		secret = r.Handoff
	}

	if secret == "" && token == "" {
		return "", false
	}

	return cockpitHandoff(url, secret, token)
}

// uiProbe is the client for the calls that carry the handoff secret or the token (as the cookie) to a cockpit: it
// follows no redirect, so neither reaches another address; a 3xx is not that cockpit's answer and fails as one.
func uiProbe() *http.Client {
	return &http.Client{Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func recordedCockpit() (uiRunning, bool) {
	var r uiRunning

	raw, err := os.ReadFile(uiStatePath())
	if err != nil || json.Unmarshal(raw, &r) != nil || r.URL == "" || r.Handoff == "" {
		return uiRunning{}, false
	}

	// The handoff secret goes only to a cockpit still running, at an address of this machine.
	if !alive(r.PID) || !localURL(r.URL) {
		return uiRunning{}, false
	}

	return r, true
}

// cockpitHandoff asks the cockpit at url for a one-time login code, showing the handoff secret or the token
// (as the page's cookie, never in the address).
func cockpitHandoff(url, secret, token string) (string, bool) {
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(url, "/")+"/api/handoff", nil)
	if err != nil {
		return "", false
	}

	req.Header.Set("X-Zmina", "1")

	if secret != "" {
		req.Header.Set("X-Zmina-Handoff", secret)
	}

	if token != "" {
		req.AddCookie(&http.Cookie{Name: uiCookie, Value: token})
	}

	resp, err := uiProbe().Do(req)
	if err != nil {
		return "", false
	}

	defer func() { _ = resp.Body.Close() }()

	var got struct {
		Code  string `json:"code"`
		Login bool   `json:"login"`
	}

	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil {
		return "", false
	}

	if got.Login {
		return url + "?login=" + got.Code, true
	}

	return url, true
}

// serveLogin answers the handoff call and spends ?login= codes; it reports whether it answered the request.
func serveLogin(w http.ResponseWriter, r *http.Request, l *uiLogins, token string, allowHosts []string) bool {
	if l == nil {
		return false
	}

	if r.URL.Path == "/api/handoff" {
		if r.Method != http.MethodPost || r.Header.Get("X-Zmina") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)

			return true
		}

		// The secret is taken only from this machine, as by zmina connect.
		secret := r.Header.Get("X-Zmina-Handoff")
		if !fromThisMachine(r, allowHosts) {
			secret = ""
		}

		code, ok := l.mint(secret)
		if !ok && token != "" {
			// A zmina ui that knows the token (--addr of a cockpit it did not record) signs in the same way.
			if c, err := r.Cookie(uiCookie); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1 {
				code, ok = l.code(), true
			}
		}

		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)

			return true
		}

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"code": code, "login": token != ""})

		return true
	}

	code := r.URL.Query().Get("login")
	if r.Method != http.MethodGet || code == "" {
		return false
	}

	if l.take(code) && token != "" {
		setUICookie(w, r, token, allowHosts)
	}

	// The code leaves the address bar and the history either way.
	q := r.URL.Query()
	q.Del("login")

	target := r.URL.Path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)

	return true
}
