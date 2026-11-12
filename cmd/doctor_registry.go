package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
)

// registryLine is one line of the registry check: its level and text.
type registryLine struct{ level, text string }

// registryKeys are the fields of a session entry that zmina reads to tell idle from busy.
var registryKeys = []string{"pid", "sessionId", "name", "status", "statusUpdatedAt"}

// registryReport reads every session file of a namespace (<ns>/sessions/*.json) and says what it finds. A file that is
// empty or does not parse is named for that, with the likely cause (Claude Code writes such files when its disk is
// full) and what to do: fail while the claude it belongs to runs, since zmina then cannot tell idle from busy and the
// role shows as "up unknown"; a warning for the leftover of a process that is gone. owners maps the pids of the roles
// that are up to their names, asked only when a bad file is found.
func registryReport(ns string, owners func() map[int]string) []registryLine {
	dir := filepath.Join(ns, "sessions")

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		return []registryLine{{"warn", fmt.Sprintf("no session entries in %s yet (none running, or Claude Code moved them)", dir)}}
	}

	var (
		out     []registryLine
		entry   map[string]any
		entries int
		named   map[int]string
	)

	for _, f := range files {
		e, err := registry.ReadEntry(f)

		var bad *registry.BadFileError

		switch {
		case errors.As(err, &bad):
			pid := filePID(f)
			level := "warn"

			if pidRuns(pid) {
				level = "fail"
			}

			if named == nil && owners != nil {
				named = owners()
			}

			out = append(out, registryLine{level, badSessionText(bad, pid, level == "fail", named[pid])})
		case err != nil:
			out = append(out, registryLine{"fail", fmt.Sprintf("%s does not read: %v", f, err)})
		default:
			if entry == nil {
				entry = e
			}

			entries++
		}
	}

	if entry != nil {
		var missing []string

		for _, k := range registryKeys {
			if _, ok := entry[k]; !ok {
				missing = append(missing, k)
			}
		}

		if len(missing) > 0 {
			return append(out, registryLine{"fail", fmt.Sprintf("entries lack %s: Claude Code changed its registry; "+
				"idle/busy and delivery will not work", strings.Join(missing, ", "))})
		}

		detail := fmt.Sprintf("%d entries in %s", entries, dir)
		if v, ok := entry["version"].(string); ok {
			detail += " · written by Claude Code " + v
		}

		out = append(out, registryLine{"ok", detail})
	}

	return out
}

// badSessionText says what an empty or cut-short session file means for the role that owns it.
func badSessionText(bad *registry.BadFileError, pid int, runs bool, role string) string {
	text := bad.Error()

	switch {
	case runs && role != "":
		return fmt.Sprintf("%s: it shows as up unknown until restarted: zmina restart %s", text, role)
	case runs:
		return text + ": the role that runs it shows as up unknown until restarted (zmina ls shows the pids)"
	default:
		return fmt.Sprintf("%s: its pid %d does not run, so nothing waits on it (a leftover of a claude that ended while "+
			"the disk was full, likely)", bad.What(), pid)
	}
}

// filePID is the pid a session file is named for (<pid>.json), 0 when it is not one.
func filePID(path string) int {
	pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(path), ".json"))
	if err != nil {
		return 0
	}

	return pid
}

// pidRuns reports whether a process with that pid exists.
func pidRuns(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

// roleOwners maps the pids of the roles that are up to their names, as zmina ls sees them.
func (a *app) roleOwners() map[int]string {
	out := map[int]string{}

	_ = a.readState(func(s *shift.Shift) error {
		for _, v := range s.Views() {
			if v.State == shift.PresenceUp && v.PID > 0 {
				out[v.PID] = v.Role
			}
		}

		return nil
	})

	return out
}
