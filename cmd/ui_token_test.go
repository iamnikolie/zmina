package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUIOnLoopbackWithNoTokenStillNeedsOne: loopback keeps out other machines, not another local account. With no
// token given, zmina ui makes one for the run: a local client without the browser's cookie reads and writes
// nothing, the terminal socket included; the browser it opened signs in with its one-time code.
func TestUIOnLoopbackWithNoTokenStillNeedsOne(t *testing.T) {
	uiHome(t)

	var (
		mu     sync.Mutex
		opened []string
	)

	openBrowserWas := openBrowser
	openBrowser = func(url string) { mu.Lock(); opened = append(opened, url); mu.Unlock() }
	t.Cleanup(func() { openBrowser = openBrowserWas })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	root := NewRoot(io.Discard, io.Discard)
	root.SetArgs([]string{"ui", "--addr", "127.0.0.1:0"})

	go func() { done <- root.ExecuteContext(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("zmina ui did not stop")
		}
	})

	var url string

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		if len(opened) > 0 {
			url = opened[0]
		}

		return url != ""
	}, 5*time.Second, 10*time.Millisecond)

	base, code, ok := strings.Cut(url, "?login=")
	require.True(t, ok, "the browser opened here gets a one-time code: %s", url)
	require.NotEmpty(t, code)

	// Another local client: the Host, Origin and X-Zmina it sets itself are not enough.
	call := func(c *http.Client, method, path string, header map[string]string) int {
		req, err := http.NewRequest(method, base+path, strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("X-Zmina", "1")
		req.Header.Set("Origin", strings.TrimSuffix(base, "/"))

		for k, v := range header {
			req.Header.Set(k, v)
		}

		resp, err := c.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		return resp.StatusCode
	}

	stranger := &http.Client{Timeout: 2 * time.Second}
	assert.Equal(t, http.StatusUnauthorized, call(stranger, http.MethodGet, "api/state", nil))
	assert.Equal(t, http.StatusUnauthorized, call(stranger, http.MethodPost, "api/todo", nil))
	assert.Equal(t, http.StatusUnauthorized, call(stranger, http.MethodGet, "api/terminal/connect",
		map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
			"Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}))

	// The opened browser spends its code once and is signed in.
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	owner := &http.Client{Timeout: 2 * time.Second, Jar: jar}
	resp, err := owner.Get(url)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, call(owner, http.MethodGet, "api/state", nil))

	// The code works once: a second client that copied it is not signed in.
	jar2, err := cookiejar.New(nil)
	require.NoError(t, err)

	late := &http.Client{Timeout: 2 * time.Second, Jar: jar2}
	resp, err = late.Get(url)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, call(late, http.MethodGet, "api/state", nil))
}

func TestUIRefusesAShortToken(t *testing.T) {
	uiHome(t)

	for _, tc := range []struct {
		name string
		args []string
		env  string
	}{
		{"flag", []string{"ui", "--addr", "127.0.0.1:0", "--no-open", "--token", "short-secret"}, ""},
		{"environment", []string{"ui", "--addr", "127.0.0.1:0", "--no-open"}, "short-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZMINA_UI_TOKEN", tc.env)

			var out bytes.Buffer

			root := NewRoot(&out, &out)
			root.SetArgs(tc.args)

			err := root.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("at least %d", uiTokenMin))
			assert.Contains(t, err.Error(), "12 characters")
			assert.NotContains(t, err.Error(), "short-secret", "the token is not echoed")
		})
	}
}
