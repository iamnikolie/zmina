package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type signinFix struct {
	s    *uiSignin
	now  time.Time
	sent []string
	logs []string
	fail error
}

func newSigninFix() *signinFix {
	f := &signinFix{now: time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)}
	f.s = &uiSignin{
		send: func(_ context.Context, text string) error {
			if f.fail != nil {
				return f.fail
			}
			f.sent = append(f.sent, text)

			return nil
		},
		now: func() time.Time { return f.now },
		log: func(line string) { f.logs = append(f.logs, line) },
	}

	return f
}

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

// code is the code in the last message sent.
func (f *signinFix) code(t *testing.T) string {
	t.Helper()
	require.NotEmpty(t, f.sent)
	c := sixDigits.FindString(f.sent[len(f.sent)-1])
	require.NotEmpty(t, c)

	return c
}

// wrong is a six-digit code that is not the live one.
func wrong(code string) string {
	if code == "000000" {
		return "000001"
	}

	return "000000"
}

func TestSigninCodeWorksOnce(t *testing.T) {
	f := newSigninFix()
	_, err := f.s.request(context.Background())
	require.NoError(t, err)

	code := f.code(t)
	assert.True(t, f.s.check(code))
	assert.False(t, f.s.check(code), "single use")

	for _, line := range f.logs {
		assert.NotContains(t, line, code, "the code is never logged")
	}
}

func TestSigninCodeExpires(t *testing.T) {
	f := newSigninFix()
	_, err := f.s.request(context.Background())
	require.NoError(t, err)

	f.now = f.now.Add(signinTTL)
	assert.False(t, f.s.check(f.code(t)))
}

func TestSigninCodeIsSpentByWrongTries(t *testing.T) {
	f := newSigninFix()
	_, err := f.s.request(context.Background())
	require.NoError(t, err)

	code := f.code(t)
	for range signinTries {
		assert.False(t, f.s.check(wrong(code)))
	}

	assert.False(t, f.s.check(code), "five wrong tries end the code")
	assert.False(t, f.s.check(""), "an empty code never matches")
}

func TestSigninCodesAreRateLimited(t *testing.T) {
	f := newSigninFix()
	_, err := f.s.request(context.Background())
	require.NoError(t, err)

	wait, err := f.s.request(context.Background())
	require.ErrorIs(t, err, errSigninWait, "one a minute")
	assert.Equal(t, signinGap, wait)

	for range signinPerHour - 1 {
		f.now = f.now.Add(signinGap)
		_, err = f.s.request(context.Background())
		require.NoError(t, err)
	}

	f.now = f.now.Add(signinGap)
	_, err = f.s.request(context.Background())
	require.ErrorIs(t, err, errSigninWait, "ten an hour")

	f.now = f.now.Add(time.Hour)
	_, err = f.s.request(context.Background())
	require.NoError(t, err)
}

func TestSigninFailedSendLeavesNoCode(t *testing.T) {
	f := newSigninFix()
	f.fail = errors.New("notifier down")
	_, err := f.s.request(context.Background())
	require.Error(t, err)
	assert.Empty(t, f.s.mail.code)

	f.fail = nil
	_, err = f.s.request(context.Background())
	require.ErrorIs(t, err, errSigninWait, "a failed send still counts against the limits")
}

func TestCommandNotifierGetsTheCodeOnStdin(t *testing.T) {
	out := t.TempDir() + "/got"
	send := commandNotifier("cat > '" + out + "'")
	require.NoError(t, send(context.Background(), "zmina sign-in code: 123456"))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "zmina sign-in code: 123456", string(got))
}

func TestSigninThroughTheGuard(t *testing.T) {
	f := newSigninFix()
	reached := ""
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = r.URL.Path; w.WriteHeader(http.StatusOK) })
	h := guardLogins(app, "127.0.0.1:7518", true, "example-token", nil, f.s)

	do := func(method, path, body string, zmina bool, cookie string) *httptest.ResponseRecorder {
		reached = ""
		r := httptest.NewRequest(method, "http://127.0.0.1:7518"+path, strings.NewReader(body))
		if zmina {
			r.Header.Set("X-Zmina", "1")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: uiCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		return w
	}

	page := do(http.MethodGet, "/", "", false, "")
	assert.Equal(t, http.StatusUnauthorized, page.Code)
	assert.Contains(t, page.Body.String(), "Send code")
	assert.Empty(t, reached, "the cockpit stays closed")

	assert.Equal(t, http.StatusUnauthorized, do(http.MethodGet, "/api/state", "", false, "").Code)
	assert.Empty(t, reached)

	do(http.MethodGet, "/signin.js", "", false, "")
	assert.Equal(t, "/signin.js", reached, "the sign-in page's own script loads")

	assert.Equal(t, http.StatusForbidden, do(http.MethodPost, "/api/signin/code", "", false, "").Code, "needs X-Zmina")
	require.Equal(t, http.StatusOK, do(http.MethodPost, "/api/signin/code", "", true, "").Code)
	code := f.code(t)
	assert.NotContains(t, page.Body.String(), code)

	bad := do(http.MethodPost, "/api/signin/verify", `{"code":"`+wrong(code)+`"}`, true, "")
	assert.Equal(t, http.StatusUnauthorized, bad.Code)
	assert.Empty(t, bad.Result().Cookies())

	good := do(http.MethodPost, "/api/signin/verify", `{"code":"`+code+`"}`, true, "")
	require.Equal(t, http.StatusOK, good.Code)
	cookies := good.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, uiCookie, cookies[0].Name)
	assert.Equal(t, "example-token", cookies[0].Value, "the same device cookie as the ?token= link")
	assert.True(t, cookies[0].HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)

	do(http.MethodGet, "/api/state", "", false, cookies[0].Value)
	assert.Equal(t, "/api/state", reached, "signed in")

	do(http.MethodGet, "/?token=example-token", "", false, "")
	assert.Equal(t, "/", reached, "the ?token= link still signs in")
}

func TestSigninPageWithoutACodeCommandIsTheOldHint(t *testing.T) {
	h := guardLogins(http.NotFoundHandler(), "127.0.0.1:7518", true, "example-token", nil, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7518/", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Token needed")

	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7518/api/signin/code", nil)
	r.Header.Set("X-Zmina", "1")
	h.ServeHTTP(w, r)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "no codes without a code command")
}

// A notifier that leaves a child behind is ended with its whole group at the deadline.
func TestCommandNotifierEndsItsGroupOnTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := commandNotifier("sleep 30 & sleep 30")(ctx, "x")
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "the deadline holds with a child still running")
}

// A slow send holds up no check: the code is settled under the lock, the send runs outside it.
func TestSigninSendRunsOutsideTheLock(t *testing.T) {
	f := newSigninFix()
	sending, release := make(chan struct{}), make(chan struct{})
	f.s.send = func(context.Context, string) error { close(sending); <-release; return nil }

	go func() { _, _ = f.s.request(context.Background()) }()
	<-sending

	done := make(chan bool)
	go func() { done <- f.s.check("000000") }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a check waited for the send")
	}
	close(release)
}
