package lesson

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DraftTitle is a merge request title GitLab takes as a draft on every version, push option or not.
func DraftTitle(title string) string {
	if strings.HasPrefix(title, "Draft: ") {
		return title
	}

	return "Draft: " + title
}

var mrParts = regexp.MustCompile(`^https?://([^/]+)/(.+?)/-/merge_requests/(\d+)`)

// Runner runs a command for ReadDraft and returns its standard output; a variable so tests stand in for glab and gl.
var Runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// LookPath finds glab or gl; a variable for tests.
var LookPath = exec.LookPath

// ReadDraft reads a pushed merge request back and tells whether GitLab holds it as a draft: "yes", "no", or ""
// when no client on this node could read it (no glab, no gl profile for the host, or the call failed). via names the
// client that answered. It never changes the merge request.
func ReadDraft(mr string) (draft, via string) {
	m := mrParts.FindStringSubmatch(mr)
	if m == nil {
		return "", ""
	}

	host, project, iid := m[1], m[2], m[3]
	path := "projects/" + url.PathEscape(project) + "/merge_requests/" + iid

	try := func(name string, args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		out, err := Runner(ctx, name, args...)
		if err != nil {
			return ""
		}

		var mr struct {
			Draft *bool `json:"draft"`
			WIP   *bool `json:"work_in_progress"`
		}

		if json.Unmarshal(out, &mr) != nil {
			return ""
		}

		switch {
		case mr.Draft != nil && *mr.Draft, mr.Draft == nil && mr.WIP != nil && *mr.WIP:
			return "yes"
		case mr.Draft != nil || mr.WIP != nil:
			return "no"
		}

		return ""
	}

	if _, err := LookPath("glab"); err == nil {
		if d := try("glab", "api", "--method", "GET", "--hostname", host, path); d != "" {
			return d, "glab"
		}
	}

	if _, err := LookPath("gl"); err == nil {
		for _, p := range glProfiles(host) {
			if d := try("gl", "--config", p, "--format", "json", "api", "GET", "/"+path); d != "" {
				return d, "gl --config " + p
			}
		}
	}

	return "", ""
}

// glProfiles are the gl profiles (~/.gl/<name>/config.yaml) whose host is the MR's (gitlab.com when a profile names
// none), $GL_CONFIG first when it is one of them: a profile of another host would read another project's MR.
func glProfiles(host string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	var out []string

	files, _ := filepath.Glob(filepath.Join(home, ".gl", "*", "config.yaml"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}

		h := "gitlab.com"
		for _, l := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "host:"); ok {
				h = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}

		h = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://"), "/")
		if !strings.EqualFold(h, host) {
			continue
		}

		name := filepath.Base(filepath.Dir(f))
		if name == os.Getenv("GL_CONFIG") {
			out = append([]string{name}, out...)
		} else {
			out = append(out, name)
		}
	}

	return out
}
