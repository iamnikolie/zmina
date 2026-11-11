package cmd

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// Version skew. A new zmina replaces the executable; what an earlier one started keeps that one's code or command
// lines until it is started again: the cockpit (zmina ui) serves with its old code, a Codex role's bridge (_codex)
// runs its old code, and a Claude role's hooks are the command lines the earlier zmina wrote into its settings, run
// by the new executable. The hook names and flags those lines use are an API kept across versions
// (TestHookCommandsResolve); the rest only a restart brings up to date. doctor names what runs another version.

// staleRole is a role up that another zmina started: its name and the version that did.
type staleRole struct{ name, version string }

func (r staleRole) String() string { return r.name + " (" + r.version + ")" }

// staleRoles sorts the roles up that this zmina did not start: Codex roles, whose bridge runs that zmina's code;
// Claude roles, whose hooks keep the command lines that one wrote; and roles started by a zmina that did not record
// its version. A role that is down is not named.
func staleRoles(c *crew.Crew, st *shift.State) (codex, claude []staleRole, unknown []string) {
	for _, name := range slices.Sorted(maps.Keys(st.Roles)) {
		rs := st.Roles[name]
		if !rs.Up || rs.Version == version {
			continue
		}

		role, _, _ := strings.Cut(name, "#")
		r, known := c.Roles[role]

		switch {
		case rs.Version == "":
			unknown = append(unknown, name)
		case known && r.IsCodex():
			codex = append(codex, staleRole{name, rs.Version})
		default:
			claude = append(claude, staleRole{name, rs.Version})
		}
	}

	return codex, claude, unknown
}

func joinStale(rs []staleRole) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.String()
	}

	return strings.Join(parts, ", ")
}

// checkVersions is the doctor's versions check: the cockpit and the roles up that another zmina started.
func checkVersions(c *crew.Crew, st *shift.State, add func(name, level, format string, args ...any)) {
	add("versions", "ok", "this zmina is %s", version)

	if r, ok := recordedCockpit(); ok && r.Version != version {
		add("versions", "warn", "%s", cockpitSkew(r))
	}

	if st == nil {
		return
	}

	codexOld, claudeOld, unknown := staleRoles(c, st)

	if len(codexOld) > 0 {
		add("versions", "warn", "Codex roles whose bridge runs the code of another zmina: %s; zmina restart <role> "+
			"--continue starts each with this one", joinStale(codexOld))
	}

	if len(claudeOld) > 0 {
		add("versions", "ok", "roles started by another zmina: %s; their hooks run this zmina with the command "+
			"lines that one wrote, the rest of their settings and boot text is that one's until zmina restart <role> "+
			"--continue", joinStale(claudeOld))
	}

	if len(unknown) > 0 {
		add("versions", "ok", "roles started by a zmina that did not record its version: %s",
			strings.Join(unknown, ", "))
	}
}

// cockpitSkew is what to say of a running cockpit of another version.
func cockpitSkew(r uiRunning) string {
	was := r.Version
	if was == "" {
		was = "an earlier version"
	}

	return fmt.Sprintf("zmina ui (pid %d) runs %s, this zmina is %s: stop it and run zmina ui again to serve "+
		"this one", r.PID, was, version)
}

// noteCockpitSkew tells a zmina ui that found the cockpit running when that one is of another version.
func (a *app) noteCockpitSkew() {
	if r, ok := recordedCockpit(); ok && r.Version != version {
		fmt.Fprintln(a.err, "zmina:", cockpitSkew(r))
	}
}
