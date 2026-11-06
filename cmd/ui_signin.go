package cmd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/ui"
)

const (
	// signinTTL is how long a sign-in code lives; signinTries how many wrong codes end it.
	signinTTL   = 5 * time.Minute
	signinTries = 5
	// signinGap is the least time between two codes; signinPerHour how many codes an hour may send.
	signinGap     = time.Minute
	signinPerHour = 10
	// signinSendFor bounds the notifier command.
	signinSendFor = 20 * time.Second
)

var errSigninWait = errors.New("wait before asking for another code")

// uiSignin signs a new device in with a one-time code sent through the owner's own notifier (--code-command), for a
// browser that has neither the ?token= link nor its cookie. One code lives at a time: six digits, single use, five
// minutes, five tries; codes are rate-limited. The code is never logged, never in the page, never in an argv.
type uiSignin struct {
	mu      sync.Mutex
	send    func(ctx context.Context, text string) error
	now     func() time.Time
	log     func(string)
	code    string
	expires time.Time
	tries   int
	sent    []time.Time
}

func newUISignin(command string, log func(string)) *uiSignin {
	return &uiSignin{send: commandNotifier(command), now: time.Now, log: log}
}

// commandNotifier runs the owner's command through the shell with the message on its stdin (never in its argv,
// where other users could read it); what it prints is dropped.
func commandNotifier(command string) func(ctx context.Context, text string) error {
	return func(ctx context.Context, text string) error {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Stdin = strings.NewReader(text)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard

		return cmd.Run()
	}
}

// request makes a new code and sends it; wait is how long until the next one may be asked for when refused.
func (s *uiSignin) request(ctx context.Context) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	kept := s.sent[:0]

	for _, at := range s.sent {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}

	s.sent = kept

	if n := len(s.sent); n > 0 && now.Sub(s.sent[n-1]) < signinGap {
		s.log("sign-in: code refused, asked again within a minute")

		return signinGap - now.Sub(s.sent[n-1]), errSigninWait
	}

	if len(s.sent) >= signinPerHour {
		s.log("sign-in: code refused, " + fmt.Sprint(signinPerHour) + " codes this hour")

		return time.Hour - now.Sub(s.sent[0]), errSigninWait
	}

	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return 0, err
	}

	// The send counts against the limits even when it fails: a broken notifier is not a way to ask without end.
	s.sent = append(s.sent, now)
	s.code, s.expires, s.tries = fmt.Sprintf("%06d", n.Int64()), now.Add(signinTTL), 0

	sctx, cancel := context.WithTimeout(ctx, signinSendFor)
	defer cancel()

	text := "zmina sign-in code: " + s.code + "\nIt works once, for 5 minutes. If you did not ask for it, ignore it."
	if err := s.send(sctx, text); err != nil {
		s.code = ""
		s.log("sign-in: the code command failed: " + err.Error())

		return 0, err
	}

	s.log("sign-in: code sent")

	return 0, nil
}

// check takes a code: true once, for the live code, within its time and its tries.
func (s *uiSignin) check(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.code == "" {
		s.log("sign-in: a code was tried with none live")

		return false
	}

	if !s.now().Before(s.expires) {
		s.code = ""
		s.log("sign-in: a code was tried after it expired")

		return false
	}

	s.tries++

	if len(code) == len(s.code) && subtle.ConstantTimeCompare([]byte(code), []byte(s.code)) == 1 {
		s.code = ""
		s.log("sign-in: signed in with a code")

		return true
	}

	if s.tries >= signinTries {
		s.code = ""
		s.log(fmt.Sprintf("sign-in: wrong code (%d of %d), the code is spent", s.tries, signinTries))

		return false
	}

	s.log(fmt.Sprintf("sign-in: wrong code (%d of %d)", s.tries, signinTries))

	return false
}

// signinAsset is what a browser not signed in may load: the sign-in page's own script and style.
func signinAsset(path string) bool { return path == "/signin.js" || path == "/signin.css" }

// serveSignin answers the sign-in API (POST /api/signin/code, POST /api/signin/verify) for a browser not signed in;
// false when the request is not one of them.
func serveSignin(w http.ResponseWriter, r *http.Request, s *uiSignin, token string, allowHosts []string) bool {
	if s == nil || (r.URL.Path != "/api/signin/code" && r.URL.Path != "/api/signin/verify") {
		return false
	}

	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost || r.Header.Get("X-Zmina") != "1" {
		http.Error(w, "forbidden", http.StatusForbidden)

		return true
	}

	if r.URL.Path == "/api/signin/code" {
		wait, err := s.request(r.Context())

		switch {
		case errors.Is(err, errSigninWait):
			w.WriteHeader(http.StatusTooManyRequests)
			writeJSON(w, map[string]any{"wait": int(wait.Round(time.Second) / time.Second)})
		case err != nil:
			w.WriteHeader(http.StatusBadGateway)
			writeJSON(w, map[string]any{"error": "the code could not be sent"})
		default:
			writeJSON(w, map[string]any{"ttl": int(signinTTL / time.Second)})
		}

		return true
	}

	var body struct {
		Code string `json:"code"`
	}

	if err := json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return true
	}

	if !s.check(strings.TrimSpace(body.Code)) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "wrong or expired code"})

		return true
	}

	setUICookie(w, r, token, allowHosts)
	writeJSON(w, map[string]any{"ok": true})

	return true
}

// signinPage is the page a browser not signed in gets in place of the cockpit when codes can be sent.
func signinPage(w http.ResponseWriter) {
	page, err := fs.ReadFile(ui.Files, "static/signin.html")
	if err != nil {
		http.Error(w, "sign-in page missing", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(page)
}
