package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uiHome makes a home with two crews: the default one and ~/.zmina/ops, each with its own tmux server name.
func uiHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZMINA_CREW", "")
	t.Setenv("ZMINA_ROLE", "")

	n := time.Now().UnixNano() % 1_000_000_000
	for _, c := range []struct{ dir, name string }{{".zmina", fmt.Sprintf("uid%d", n)}, {".zmina/ops", fmt.Sprintf("uio%d", n)}} {
		dir := filepath.Join(home, c.dir)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "crew.yaml"), []byte(fmt.Sprintf(
			"name: %s\nnamespace: %s\ndir: %s\nroles:\n  dispatcher:\n  dev:\n", c.name, filepath.Join(dir, "ns"), home)),
			0o600))
	}

	return home
}

func uiServer(t *testing.T, loopback bool, token string) (*httptest.Server, *app) {
	t.Helper()

	a := &app{out: io.Discard, err: io.Discard}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = a.uiHandler(srv.Listener.Addr().String(), loopback, token)
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, a
}

func TestUISnapshotShowsEveryCrew(t *testing.T) {
	uiHome(t)

	_, _, err := zm(t, crewPathOf(t, "default"), "todo", "add", "--rec", "yes", "ship v2?")
	require.NoError(t, err)
	_, _, err = zm(t, crewPathOf(t, "ops"), "todo", "add", "--blocks", "dev", "rotate the key")
	require.NoError(t, err)

	srv, _ := uiServer(t, true, "")

	resp, err := http.Get(srv.URL + "/api/state")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	var snap uiSnapshot
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&snap))
	require.Len(t, snap.Crews, 2)
	assert.ElementsMatch(t, []string{"default", "ops"}, []string{snap.Crews[0].Key, snap.Crews[1].Key})
	require.Len(t, snap.Todo, 2)
	assert.Equal(t, "ops", snap.Todo[0].Crew, "blocking first, across crews")
	assert.Equal(t, "default", snap.Todo[1].Crew)
	assert.Len(t, snap.Crews[0].Roles, 2)
}

func crewPathOf(t *testing.T, key string) string {
	t.Helper()

	for _, c := range uiCrews("") {
		if c.Key == key {
			return c.Path
		}
	}

	t.Fatalf("no crew %s", key)

	return ""
}

func TestUIAnswerGoesToItsOwnCrew(t *testing.T) {
	uiHome(t)

	_, _, err := zm(t, crewPathOf(t, "ops"), "todo", "add", "--rec", "B", "pick A or B")
	require.NoError(t, err)

	srv, _ := uiServer(t, true, "")

	post := func(body string, header bool) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/todo", strings.NewReader(body))
		if header {
			req.Header.Set("X-Zmina", "1")
		}

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		return resp
	}

	assert.Equal(t, http.StatusForbidden, post(`{"crew":"ops","id":"A-1","action":"yes"}`, false).StatusCode,
		"a write without X-Zmina is refused")
	assert.Equal(t, http.StatusConflict, post(`{"crew":"default","id":"A-1","action":"yes"}`, true).StatusCode,
		"the default crew has no A-1")
	assert.Equal(t, http.StatusConflict, post(`{"crew":"../x","id":"A-1","action":"yes"}`, true).StatusCode)
	assert.Equal(t, http.StatusBadRequest, post(`{"crew":"ops","id":"A-1","action":"keys"}`, true).StatusCode)
	assert.Equal(t, http.StatusOK, post(`{"crew":"ops","id":"A-1","action":"yes"}`, true).StatusCode)

	out, _, err := zm(t, crewPathOf(t, "ops"), "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "ui: A-1 · yes: B — pick A or B")

	out, _, err = zm(t, crewPathOf(t, "default"), "queue", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "A-1", "nothing reaches another crew's dispatcher")
}

func TestUIGuard(t *testing.T) {
	uiHome(t)

	srv, _ := uiServer(t, true, "")

	get := func(url, host string) int {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if host != "" {
			req.Host = host
		}

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		return resp.StatusCode
	}

	assert.Equal(t, http.StatusOK, get(srv.URL+"/", ""))
	assert.Equal(t, http.StatusOK, get(srv.URL+"/api/ping", ""))
	assert.Equal(t, http.StatusForbidden, get(srv.URL+"/api/state", "attacker.example"), "DNS rebinding")

	tok, _ := uiServer(t, false, "s3cret")
	assert.Equal(t, http.StatusUnauthorized, get(tok.URL+"/api/state", "lan.example"))
	assert.Equal(t, http.StatusOK, get(tok.URL+"/api/state?token=s3cret", "lan.example"))

	resp, err := http.Get(tok.URL + "/?token=s3cret")
	require.NoError(t, err)
	_ = resp.Body.Close()

	var cookie *http.Cookie

	for _, c := range resp.Cookies() {
		if c.Name == uiCookie {
			cookie = c
		}
	}

	require.NotNil(t, cookie, "the token is kept in a cookie")

	req, _ := http.NewRequest(http.MethodGet, tok.URL+"/api/state", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestUIRefusesAnOpenBindWithoutAToken(t *testing.T) {
	uiHome(t)

	var out, errb bytes.Buffer

	root := NewRoot(&out, &errb)
	root.SetArgs([]string{"ui", "--addr", "0.0.0.0:0", "--no-open"})
	require.ErrorContains(t, root.Execute(), "needs --token")
}

func TestRoleIcon(t *testing.T) {
	for role, want := range map[string]string{"dispatcher": "dispatcher", "dev": "developer", "dev2#3": "developer",
		"reviewer": "reviewer", "designer": "designer", "scribe": "generic"} {
		assert.Equal(t, want, roleIcon(role), role)
	}
}

func TestUITaskIDIsNeverAPath(t *testing.T) {
	home := uiHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "secret.json"), []byte(`{"id":"../../secret"}`), 0o600))

	srv, _ := uiServer(t, true, "")

	for _, id := range []string{"../../secret", "T-1/../../../secret", "%2e%2e%2fsecret"} {
		resp, err := http.Get(srv.URL + "/api/task?crew=default&id=" + id)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, id)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/todo",
		strings.NewReader(`{"crew":"default","id":"A-../../../secret","action":"done"}`))
	req.Header.Set("X-Zmina", "1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}
