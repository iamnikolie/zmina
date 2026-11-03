package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginCodesWorkOnceAndExpire(t *testing.T) {
	l := newUILogins()
	now := time.Now()
	l.now = func() time.Time { return now }

	_, ok := l.mint("wrong")
	assert.False(t, ok)

	code, ok := l.mint(l.handoff)
	require.True(t, ok)
	assert.True(t, l.take(code))
	assert.False(t, l.take(code), "once")

	code = l.code()
	now = now.Add(uiLoginTTL + time.Second)
	assert.False(t, l.take(code), "a minute at most")

	assert.False(t, (&uiLogins{codes: map[string]time.Time{}, now: time.Now}).take(""))
	_, ok = (&uiLogins{codes: map[string]time.Time{}, now: time.Now}).mint("")
	assert.False(t, ok, "no handoff secret, no code")
}

func tokenCockpit(t *testing.T) (*httptest.Server, *app) {
	t.Helper()

	a := &app{out: io.Discard, err: io.Discard, uiLogins: newUILogins()}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = a.uiHandler(srv.Listener.Addr().String(), true, "example-token")
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, a
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestHandoffSignsInWithoutTheToken(t *testing.T) {
	uiHome(t)

	srv, a := tokenCockpit(t)

	handoff := func(secret string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/handoff", nil)
		require.NoError(t, err)
		req.Header.Set("X-Zmina", "1")
		req.Header.Set("X-Zmina-Handoff", secret)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		return resp
	}

	assert.Equal(t, http.StatusForbidden, handoff("guess").StatusCode)

	resp := handoff(a.uiLogins.handoff)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		Code  string `json:"code"`
		Login bool   `json:"login"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.True(t, got.Login)
	assert.Len(t, got.Code, 32)

	login, err := noRedirect().Get(srv.URL + "/?login=" + got.Code)
	require.NoError(t, err)
	_ = login.Body.Close()
	assert.Equal(t, http.StatusSeeOther, login.StatusCode)
	assert.Equal(t, "/", login.Header.Get("Location"), "the code leaves the address")

	var cookie *http.Cookie
	for _, c := range login.Cookies() {
		if c.Name == uiCookie {
			cookie = c
		}
	}
	require.NotNil(t, cookie)
	assert.True(t, cookie.HttpOnly)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.AddCookie(cookie)
	state, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = state.Body.Close()
	assert.Equal(t, http.StatusOK, state.StatusCode)

	again, err := noRedirect().Get(srv.URL + "/?login=" + got.Code)
	require.NoError(t, err)
	_ = again.Body.Close()
	assert.Empty(t, again.Cookies(), "a code works once")

	page, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	_ = page.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, page.StatusCode)
}

func TestSecondUIFindsTheRunningCockpit(t *testing.T) {
	uiHome(t)

	_, ok := runningCockpit()
	assert.False(t, ok, "nothing recorded")

	srv, a := tokenCockpit(t)
	require.NoError(t, writeUIState(srv.URL+"/", a.uiLogins))

	info, err := os.Stat(uiStatePath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	url, ok := runningCockpit()
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(url, srv.URL+"/?login="), url)
	assert.NotContains(t, url, "example-token")

	dropUIState()
	_, err = os.Stat(uiStatePath())
	assert.True(t, os.IsNotExist(err), "the running process removes its own record")

	require.NoError(t, writeUIState(srv.URL+"/", a.uiLogins))
	srv.Close()

	_, ok = runningCockpit()
	assert.False(t, ok, "a record of a cockpit that no longer answers is ignored")
}
