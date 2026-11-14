//go:build browser

package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestBrowserFixtures opens every page in testdata/ in headless Chrome, as testdata/README.md says to by hand, and
// waits for its window.<name>Results.passed. Real time, not virtual: the outbox and draft files are IndexedDB
// Blobs, which virtual time runs ahead of. Run with: go test -tags browser -run TestBrowserFixtures ./internal/ui
// ($CHROME names the browser when it is not found).
func TestBrowserFixtures(t *testing.T) {
	chrome := findChrome(t)

	srv := httptest.NewServer(http.FileServer(http.Dir(".")))
	t.Cleanup(srv.Close)

	page := startChrome(t, chrome)

	fixtures, err := filepath.Glob("testdata/*.html")
	require.NoError(t, err)
	require.NotEmpty(t, fixtures)

	for _, f := range fixtures {
		name := strings.TrimSuffix(filepath.Base(f), ".html")

		t.Run(name, func(t *testing.T) {
			if why, ok := knownFailing[name]; ok {
				t.Skip("known to fail: " + why)
			}

			_, err := page.call("Page.navigate", map[string]any{"url": srv.URL + "/testdata/" + name + ".html"})
			require.NoError(t, err)

			result := page.wait(t, resultsVar(name), 90*time.Second)
			if !result.Passed {
				t.Fatalf("%s did not pass: %s", name, result.Raw)
			}
		})
	}
}

// knownFailing are fixtures that fail on main as it is, each with what fails; a fix removes its line. They are
// skipped so the job guards the others from the day it is added.
var knownFailing = map[string]string{}

// resultsVar is the global a fixture page sets: chat-outbox → chatOutboxResults.
func resultsVar(name string) string {
	parts := strings.Split(name, "-")
	for i := 1; i < len(parts); i++ {
		r := []rune(parts[i])
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}

	return strings.Join(parts, "") + "Results"
}

func findChrome(t *testing.T) string {
	t.Helper()

	candidates := []string{os.Getenv("CHROME"), "google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	}

	for _, c := range candidates {
		if c == "" {
			continue
		}

		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}

	if os.Getenv("CI") != "" {
		t.Fatal("no Chrome found: CI must run the browser fixtures (set CHROME)")
	}

	t.Skip("no Chrome found (set CHROME)")

	return ""
}

// cdpPage is one page of a headless Chrome, driven over the DevTools protocol.
type cdpPage struct {
	conn *websocket.Conn
	next int
}

// startChrome starts a headless Chrome on its own profile and returns its first page. The process is killed by its
// pid when the test ends: a headless Chrome left behind can keep the owner's own Chrome from opening.
func startChrome(t *testing.T, chrome string) *cdpPage {
	t.Helper()

	// Not t.TempDir: Chrome's helper processes outlive the kill by a moment and still write to the profile, and a
	// directory that could not be removed then would fail a run whose pages all passed.
	profile, err := os.MkdirTemp("", "zmina-browser-profile-")
	require.NoError(t, err)
	args := []string{"--headless=new", "--remote-debugging-port=0", "--user-data-dir=" + profile, "--no-first-run",
		"--no-default-browser-check", "--disable-gpu", "--window-size=1280,900", "about:blank"}

	if runtime.GOOS == "linux" {
		// CI runners restrict the user namespaces Chrome's sandbox needs.
		args = append(args, "--no-sandbox")
	}

	cmd := exec.Command(chrome, args...)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		for i := 0; i < 50 && os.RemoveAll(profile) != nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	})

	var port string

	deadline := time.Now().Add(30 * time.Second)
	for port == "" {
		if time.Now().After(deadline) {
			t.Fatal("Chrome did not open its debugging port")
		}

		if f, err := os.Open(filepath.Join(profile, "DevToolsActivePort")); err == nil {
			sc := bufio.NewScanner(f)
			if sc.Scan() {
				port = strings.TrimSpace(sc.Text())
			}

			_ = f.Close()
		}

		time.Sleep(100 * time.Millisecond)
	}

	var targets []struct {
		Type string `json:"type"`
		WS   string `json:"webSocketDebuggerUrl"`
	}

	resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&targets))
	_ = resp.Body.Close()

	for _, tg := range targets {
		if tg.Type == "page" {
			conn, _, err := websocket.DefaultDialer.Dial(tg.WS, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			return &cdpPage{conn: conn}
		}
	}

	t.Fatal("Chrome has no page")

	return nil
}

// call sends one DevTools command and returns its result, skipping the events that come between.
func (p *cdpPage) call(method string, params map[string]any) (json.RawMessage, error) {
	p.next++
	id := p.next

	if err := p.conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}

	_ = p.conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	for {
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}

		if err := p.conn.ReadJSON(&msg); err != nil {
			return nil, err
		}

		if msg.ID != id {
			continue
		}

		if msg.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, msg.Error.Message)
		}

		return msg.Result, nil
	}
}

type fixtureResult struct {
	Passed bool
	Raw    string
}

// wait polls the page until window.<global> is set, then reads whether it passed. A page that reloads itself
// (chat-outbox) is read after the reload: the global is set only by the page that finishes.
func (p *cdpPage) wait(t *testing.T, global string, limit time.Duration) fixtureResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	expr := "window." + global + " === undefined ? '' : JSON.stringify(window." + global + ")"

	for {
		raw, err := p.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
		if err == nil {
			var out struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}

			if json.Unmarshal(raw, &out) == nil && out.Result.Value != "" {
				var res struct {
					Passed *bool `json:"passed"`
				}

				// chat-content sets passed at once; the others set it false while running only on error.
				if json.Unmarshal([]byte(out.Result.Value), &res) == nil && res.Passed != nil {
					return fixtureResult{Passed: *res.Passed, Raw: clip(out.Result.Value)}
				}
			}
		}

		select {
		case <-ctx.Done():
			t.Fatalf("window.%s was not set within %s", global, limit)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func clip(s string) string {
	if len(s) > 4000 {
		return s[:4000] + "…"
	}

	return s
}
