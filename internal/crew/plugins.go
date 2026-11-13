package crew

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The events a plugin is told of (plugins[].on). A plugin is a program zmina runs with one JSON line on its stdin when
// something reaches the owner's todo or a role's screen, so that the owner can be reached, and answer, somewhere zmina
// has no channel of its own (see cmd/skill.md, Plugins).
const (
	// PluginAsk is a question put on the owner's todo: an ask, a decision or check, a task that waits on him.
	PluginAsk = "ask"
	// PluginBlocking is such an item while it holds a role up; it goes instead of PluginAsk, never as well.
	PluginBlocking = "blocking"
	// PluginDialog is a question on a role's screen that zmina does not answer itself, once it has stood a while.
	PluginDialog = "dialog"
	// PluginReminder is a reminder ringing, a parked item coming back.
	PluginReminder = "reminder"
	// PluginAnswered is an item or dialog the plugin was told of, closed, answered or gone.
	PluginAnswered = "answered"
)

var pluginEvents = []string{PluginAsk, PluginBlocking, PluginDialog, PluginReminder, PluginAnswered}

// defaultPluginOn is what a plugin is told of when its on: is not given.
var defaultPluginOn = []string{PluginAsk, PluginBlocking, PluginDialog, PluginAnswered}

// The defaults and limits of a plugin's spans.
const (
	DefaultPluginQuiet       = 5 * time.Minute
	DefaultPluginTimeout     = 10 * time.Second
	MinPluginTimeout         = time.Second
	MaxPluginTimeout         = time.Minute
	DefaultPluginDialogAfter = time.Minute
	// MaxPluginDialogAfter bounds dialog_after: a question that waits longer than this is asked about by nobody.
	MaxPluginDialogAfter = time.Hour
)

// Plugin is one program told of what reaches the owner, and able to answer back. The program is Run when the crew file
// gives its argv, else zmina-plugin-<Name> on PATH, else <crew dir>/plugins/<Name>; it must be a regular executable file
// that is not writable by group or others.
type Plugin struct {
	// Name is a plain word, unique in the crew file; the plugin is known by it in the answer commands (--by), the
	// plugin commands and the event log.
	Name string `yaml:"name"`
	// Run is the argv to run instead of looking the program up: its first word is a path or a name on PATH.
	Run []string `yaml:"run"`
	// On are the events it is told of (ask, blocking, dialog, reminder, answered); default ask, blocking, dialog, answered.
	On []string `yaml:"on"`
	// QuietWhenActive holds the push while the owner was at the machine within this span (he typed to a role, or
	// answered in a terminal): default 5m, off for never. Answered is never held.
	QuietWhenActive string `yaml:"quiet_when_active"`
	// Timeout is how long one run may take (default 10s, 1s to 60s); its process group is killed after it.
	Timeout string `yaml:"timeout"`
	// DialogAfter is how long a question on a role's screen stands before the plugin is told (default 60s, up to 1h).
	DialogAfter string `yaml:"dialog_after"`
}

// Takes reports whether the plugin is told of an event.
func (p Plugin) Takes(event string) bool { return slices.Contains(p.Events(), event) }

// Events are the events the plugin is told of, in the order they are documented.
func (p Plugin) Events() []string {
	if len(p.On) == 0 {
		return slices.Clone(defaultPluginOn)
	}

	var out []string

	for _, e := range pluginEvents {
		if slices.ContainsFunc(p.On, func(w string) bool { return strings.TrimSpace(w) == e }) {
			out = append(out, e)
		}
	}

	return out
}

// QuietD is how long after the owner was last active the plugin's pushes are held; zero is never.
func (p Plugin) QuietD() time.Duration {
	d, _ := pluginSpan(p.QuietWhenActive, DefaultPluginQuiet, true)

	return d
}

// TimeoutD is how long one run may take.
func (p Plugin) TimeoutD() time.Duration {
	d, _ := pluginSpan(p.Timeout, DefaultPluginTimeout, false)

	return d
}

// DialogAfterD is how long a question must stand on a role's screen before the plugin is told of it.
func (p Plugin) DialogAfterD() time.Duration {
	d, _ := pluginSpan(p.DialogAfter, DefaultPluginDialogAfter, false)

	return d
}

// pluginSpan reads a span: empty is the default; off (when offOK) or 0 is zero.
func pluginSpan(v string, def time.Duration, offOK bool) (time.Duration, error) {
	switch v = strings.TrimSpace(v); {
	case v == "":
		return def, nil
	case offOK && v == "off":
		return 0, nil
	}

	return ParseSpan(v)
}

// PluginNamed is the plugin of that name.
func (c *Crew) PluginNamed(name string) (Plugin, bool) {
	for _, p := range c.Plugins {
		if p.Name == name {
			return p, true
		}
	}

	return Plugin{}, false
}

// PluginsTaking are the plugins that are told of an event.
func (c *Crew) PluginsTaking(event string) []Plugin {
	var out []Plugin

	for _, p := range c.Plugins {
		if p.Takes(event) {
			out = append(out, p)
		}
	}

	return out
}

// checkPlugins says what is wrong with the plugins block: a name that is not a plain word or is given twice, an empty
// word in run, an event it does not know, a span that is not one or is out of range.
func (c *Crew) checkPlugins() error {
	seen := map[string]bool{}

	for i, p := range c.Plugins {
		label := fmt.Sprintf("plugins[%d]", i)

		if p.Name != "" {
			label = "plugin " + p.Name
		}

		switch {
		case p.Name == "":
			return fmt.Errorf("%s: name is a plain word such as pager", label)
		case !roleRe.MatchString(p.Name):
			return fmt.Errorf("%s: name %q: a lowercase word joined by dashes", label, p.Name)
		case seen[p.Name]:
			return fmt.Errorf("plugin %s: the name is given twice", p.Name)
		}

		seen[p.Name] = true

		for _, w := range p.Run {
			if strings.TrimSpace(w) == "" {
				return fmt.Errorf("%s: run is the argv, and no word of it is empty", label)
			}
		}

		for _, e := range p.On {
			if !slices.Contains(pluginEvents, strings.TrimSpace(e)) {
				return fmt.Errorf("%s: on %q: one of %s", label, e, strings.Join(pluginEvents, ", "))
			}
		}

		if _, err := pluginSpan(p.QuietWhenActive, 0, true); err != nil {
			return fmt.Errorf("%s: quiet_when_active %q: a span such as 5m or 1h, or off", label, p.QuietWhenActive)
		}

		if d, err := pluginSpan(p.Timeout, DefaultPluginTimeout, false); err != nil || d < MinPluginTimeout || d > MaxPluginTimeout {
			return fmt.Errorf("%s: timeout %q: a span from %s to %s, such as 10s", label, p.Timeout, MinPluginTimeout, MaxPluginTimeout)
		}

		if d, err := pluginSpan(p.DialogAfter, DefaultPluginDialogAfter, false); err != nil || d > MaxPluginDialogAfter {
			return fmt.Errorf("%s: dialog_after %q: a span from 0s to %s, such as 60s", label, p.DialogAfter, MaxPluginDialogAfter)
		}
	}

	return nil
}
