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

// checkVersions is the doctor's versions check: the cockpit and the roles up that another zmina started.
func checkVersions(c *crew.Crew, st *shift.State, add func(name, level, format string, args ...any)) {
	add("versions", "ok", "this zmina is %s", version)

	if r, ok := recordedCockpit(); ok && r.Version != version {
		add("versions", "warn", "%s", cockpitSkew(r))
	}

	if st == nil {
		return
	}

	var codexOld, claudeOld, unknown []string

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
			codexOld = append(codexOld, name+" ("+rs.Version+")")
		default:
			claudeOld = append(claudeOld, name+" ("+rs.Version+")")
		}
	}

	if len(codexOld) > 0 {
		add("versions", "warn", "Codex roles whose bridge runs the code of another zmina: %s; zmina restart <role> "+
			"--continue starts each with this one", strings.Join(codexOld, ", "))
	}

	if len(claudeOld) > 0 {
		add("versions", "ok", "roles started by another zmina: %s; their hooks run this zmina with the command "+
			"lines that one wrote, the rest of their settings and boot text is that one's until zmina restart <role> "+
			"--continue", strings.Join(claudeOld, ", "))
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
