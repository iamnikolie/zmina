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
	"syscall"
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

var (
	errSigninWait = errors.New("wait before asking for another code")
	// errSigninNoSend is a code asked from the page of a cockpit with no notifier: its codes come from zmina connect.
	errSigninNoSend = errors.New("no code command: run zmina connect on the machine where zmina ui runs")
)

// uiSignin signs a new device in with a one-time code, for a browser that has neither the ?token= link nor its
// cookie. A code comes two ways, each in a slot of its own: sent through the owner's own notifier (--code-command)
// when the page asks, or printed in the owner's terminal by zmina connect. The slots are apart, so a page asking for
// a code never replaces the one the owner is typing. Each code: six digits, single use, five minutes, five wrong
// tries; codes of each slot are rate-limited. A code is never logged, never in the page, never in an argv.
type uiSignin struct {
	mu sync.Mutex
	// send delivers a code through the owner's notifier; nil when there is none (codes only from zmina connect).
	send func(ctx context.Context, text string) error
	now  func() time.Time
	log  func(string)
	// mail holds the code sent through the notifier, term the one zmina connect printed.
	mail, term signinSlot
}

// signinSlot is one live code with its tries, and the times codes were made there (for the limits).
type signinSlot struct {
	code    string
	expires time.Time
	tries   int
	made    []time.Time
}

// newUISignin is the sign-in of a cockpit with a token; command is its notifier, empty for none.
func newUISignin(command string, log func(string)) *uiSignin {
	s := &uiSignin{now: time.Now, log: log}
	if command != "" {
		s.send = commandNotifier(command)
	}

	return s
}

// sends reports whether codes can be sent through a notifier.
func (s *uiSignin) sends() bool { return s.send != nil }

// commandNotifier runs the owner's command through the shell with the message on its stdin (never in its argv,
// where other users could read it); what it prints goes to /dev/null, so no pipe of ours can keep it waiting. It
// runs in a process group of its own: past the deadline the whole group is killed, not only the shell.
func commandNotifier(command string) func(ctx context.Context, text string) error {
	return func(ctx context.Context, text string) error {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Stdin = strings.NewReader(text)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 2 * time.Second

		return cmd.Run()
	}
}

// request makes a new code and sends it; wait is how long until the next one may be asked for when refused. The
// limits and the code are settled under the lock, the send runs outside it: a slow notifier holds up no check.
func (s *uiSignin) request(ctx context.Context) (time.Duration, error) {
	if s.send == nil {
		return 0, errSigninNoSend
	}

	code, wait, err := s.reserve(&s.mail, "sign-in")
	if err != nil {
		return wait, err
	}

	sctx, cancel := context.WithTimeout(ctx, signinSendFor)
	defer cancel()

	text := "zmina sign-in code: " + code + "\nIt works once, for 5 minutes. If you did not ask for it, ignore it."
	if err := s.send(sctx, text); err != nil {
		s.mu.Lock()
		if s.mail.code == code {
			s.mail.code = ""
		}
		s.mu.Unlock()
		s.log("sign-in: the code command failed: " + err.Error())

		return 0, err
	}

	s.log("sign-in: code sent")

	return 0, nil
}

// connect makes the code zmina connect prints; wait is how long until the next one when refused. It replaces an
// earlier connect code, never the notifier's.
func (s *uiSignin) connect() (string, time.Duration, error) {
	code, wait, err := s.reserve(&s.term, "sign-in: zmina connect")
	if err != nil {
		return "", wait, err
	}

	s.log("sign-in: code made for zmina connect")

	return code, 0, nil
}

// reserve checks a slot's limits and makes its live code, counting it before it is used.
func (s *uiSignin) reserve(slot *signinSlot, who string) (string, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	kept := slot.made[:0]

	for _, at := range slot.made {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}

	slot.made = kept

	if n := len(slot.made); n > 0 && now.Sub(slot.made[n-1]) < signinGap {
		s.log(who + ": code refused, asked again within a minute")

		return "", signinGap - now.Sub(slot.made[n-1]), errSigninWait
	}

	if len(slot.made) >= signinPerHour {
		s.log(who + ": code refused, " + fmt.Sprint(signinPerHour) + " codes this hour")

		return "", time.Hour - now.Sub(slot.made[0]), errSigninWait
	}

	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", 0, err
	}

	// The code counts against the limits even when its send fails: a broken notifier is not a way to ask without end.
	slot.made = append(slot.made, now)
	slot.code, slot.expires, slot.tries = fmt.Sprintf("%06d", n.Int64()), now.Add(signinTTL), 0

	return slot.code, 0, nil
}

// check takes a code: true once, for a live code, within its time and its tries. A wrong code counts as a try on
// every live code; a right one spends only its own.
func (s *uiSignin) check(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	live := []*signinSlot{}
	expired := false

	for _, slot := range []*signinSlot{&s.mail, &s.term} {
		if slot.code == "" {
			continue
		}

		if !now.Before(slot.expires) {
			slot.code, expired = "", true

			continue
		}

		live = append(live, slot)
	}

	if len(live) == 0 {
		if expired {
			s.log("sign-in: a code was tried after it expired")
		} else {
			s.log("sign-in: a code was tried with none live")
		}

		return false
	}

	// Every live code is compared, matched or not, so the time taken says nothing about which one was.
	var hit *signinSlot

	for _, slot := range live {
		if len(code) == len(slot.code) && subtle.ConstantTimeCompare([]byte(code), []byte(slot.code)) == 1 {
			hit = slot
		}
	}

	if hit != nil {
		hit.code = ""
		s.log("sign-in: signed in with a code")

		return true
	}

	for _, slot := range live {
		slot.tries++

		if slot.tries >= signinTries {
			slot.code = ""
			s.log(fmt.Sprintf("sign-in: wrong code (%d of %d), the code is spent", slot.tries, signinTries))

			continue
		}

		s.log(fmt.Sprintf("sign-in: wrong code (%d of %d)", slot.tries, signinTries))
	}

	return false
}

// signinAsset is what a browser not signed in may load: the sign-in page's own script and style.
func signinAsset(path string) bool { return path == "/signin.js" || path == "/signin.css" }

// serveSignin answers the sign-in API for a browser not signed in: GET /api/signin (whether codes can be sent, so
// the page knows which way to offer), POST /api/signin/code, POST /api/signin/verify; false when the request is not
// one of them.
func serveSignin(w http.ResponseWriter, r *http.Request, s *uiSignin, token string, allowHosts []string) bool {
	if s == nil || (r.URL.Path != "/api/signin" && r.URL.Path != "/api/signin/code" && r.URL.Path != "/api/signin/verify") {
		return false
	}

	w.Header().Set("Cache-Control", "no-store")

	if r.URL.Path == "/api/signin" {
		if r.Method != http.MethodGet {
			http.Error(w, "forbidden", http.StatusForbidden)

			return true
		}

		writeJSON(w, map[string]any{"send": s.sends()})

		return true
	}

	if r.Method != http.MethodPost || r.Header.Get("X-Zmina") != "1" {
		http.Error(w, "forbidden", http.StatusForbidden)

		return true
	}

	if r.URL.Path == "/api/signin/code" {
		wait, err := s.request(r.Context())

		switch {
		case errors.Is(err, errSigninNoSend):
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": "no code command: run zmina connect"})
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

// signinPage is the page a browser not signed in gets in place of the cockpit when it has a token.
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
