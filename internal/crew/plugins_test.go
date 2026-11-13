package crew

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pluginRoles = "namespace: /ns\nroles:\n  dispatcher:\n"

// No plugins is the crew as it was; a plugin with only a name takes the defaults.
func TestPluginsDefaults(t *testing.T) {
	c, err := Load(write(t, pluginRoles))
	require.NoError(t, err)
	assert.Empty(t, c.Plugins)

	c, err = Load(write(t, "plugins:\n  - name: pager\n"+pluginRoles))
	require.NoError(t, err)
	require.Len(t, c.Plugins, 1)
	assert.Empty(t, c.Warnings)

	p, ok := c.PluginNamed("pager")
	require.True(t, ok)
	assert.Equal(t, []string{"ask", "blocking", "dialog", "answered"}, p.Events())
	assert.False(t, p.Takes("reminder"), "a reminder is asked for")
	assert.Equal(t, 5*time.Minute, p.QuietD())
	assert.Equal(t, 10*time.Second, p.TimeoutD())
	assert.Equal(t, time.Minute, p.DialogAfterD())

	_, ok = c.PluginNamed("other")
	assert.False(t, ok)
}

func TestPluginsKeys(t *testing.T) {
	c, err := Load(write(t, `plugins:
  - name: pager
    run: [zmina-plugin-pager, --quiet]
    on: [reminder, answered, ask]
    quiet_when_active: off
    timeout: 45s
    dialog_after: 90s
  - name: log
    quiet_when_active: 1h
    dialog_after: 0s
`+pluginRoles))
	require.NoError(t, err)

	p, _ := c.PluginNamed("pager")
	assert.Equal(t, []string{"zmina-plugin-pager", "--quiet"}, p.Run)
	assert.Equal(t, []string{"ask", "reminder", "answered"}, p.Events(), "in the documented order, whatever the file's")
	assert.Zero(t, p.QuietD(), "off")
	assert.Equal(t, 45*time.Second, p.TimeoutD())
	assert.Equal(t, 90*time.Second, p.DialogAfterD())

	l, _ := c.PluginNamed("log")
	assert.Equal(t, time.Hour, l.QuietD())
	assert.Zero(t, l.DialogAfterD(), "a dialog may be told at once")

	assert.Equal(t, []string{"pager"}, names(c.PluginsTaking("reminder")))
	assert.Equal(t, []string{"pager", "log"}, names(c.PluginsTaking("ask")))
}

func names(ps []Plugin) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}

	return out
}

// The crew check refuses what a plugin cannot work with.
func TestPluginsCheck(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"no name":         {"plugins:\n  - on: [ask]\n", "name is a plain word"},
		"not a word":      {"plugins:\n  - name: Pager Two\n", `name "Pager Two": a lowercase word`},
		"a path":          {"plugins:\n  - name: ../pager\n", `name "../pager"`},
		"given twice":     {"plugins:\n  - name: pager\n  - name: pager\n", "plugin pager: the name is given twice"},
		"empty run word":  {"plugins:\n  - name: pager\n    run: [pager, \"\"]\n", "no word of it is empty"},
		"unknown event":   {"plugins:\n  - name: pager\n    on: [ask, shout]\n", `on "shout": one of ask, blocking, dialog, reminder, answered`},
		"bad quiet":       {"plugins:\n  - name: pager\n    quiet_when_active: soon\n", `quiet_when_active "soon"`},
		"timeout zero":    {"plugins:\n  - name: pager\n    timeout: 0s\n", `timeout "0s": a span from 1s to 1m0s`},
		"timeout too big": {"plugins:\n  - name: pager\n    timeout: 61s\n", `timeout "61s"`},
		"timeout bad":     {"plugins:\n  - name: pager\n    timeout: long\n", `timeout "long"`},
		"dialog_after":    {"plugins:\n  - name: pager\n    dialog_after: 2h\n", `dialog_after "2h"`},
		"dialog bad":      {"plugins:\n  - name: pager\n    dialog_after: soon\n", `dialog_after "soon"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.body+pluginRoles))
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}

	// The edges that are allowed.
	_, err := Load(write(t, "plugins:\n  - {name: a, timeout: 1s, dialog_after: 1h, quiet_when_active: 0}\n  - {name: b, timeout: 60s}\n"+pluginRoles))
	require.NoError(t, err)
}

// notify keeps working beside plugins, unchanged.
func TestPluginsBesideNotify(t *testing.T) {
	c, err := Load(write(t, "notify: [push, --title, zmina]\nnotify_on: [ask]\nplugins:\n  - name: pager\n"+pluginRoles))
	require.NoError(t, err)
	assert.True(t, c.NotifiesOn(NotifyAsk))
	assert.Len(t, c.Plugins, 1)
}

func TestPluginLayout(t *testing.T) {
	l := Layout{Dir: "/crew"}
	assert.Equal(t, "/crew/plugins", l.Plugins())
	assert.Equal(t, "/crew/plugins/pager.told.json", l.PluginTold("pager"))
	assert.Equal(t, "/crew/plugins/pager.lock", l.PluginLock("pager"))
	assert.Equal(t, "/crew/plugins/owner.json", l.PluginOwner())
}
