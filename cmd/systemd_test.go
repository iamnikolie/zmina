package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSystemdUnits(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/home/a b/bin")
	t.Setenv("HOME", "/home/a b")
	t.Setenv("LANG", "")

	svc := string(systemdService("/home/a b/bin/zmina", "/home/a b/.zmina/100%/crew.yaml", "/home/a b/.zmina/tick.log"))

	assert.Contains(t, svc, `ExecStart="/home/a b/bin/zmina" tick --crew "/home/a b/.zmina/100%%/crew.yaml"`,
		"words with spaces are quoted, % is not a specifier")
	assert.Contains(t, svc, `Environment="PATH=/usr/bin:/home/a b/bin"`)
	assert.Contains(t, svc, `Environment="LANG=en_US.UTF-8"`)
	assert.Contains(t, svc, "Type=oneshot")
	assert.Contains(t, svc, "KillMode=process", "the tmux server outlives the tick")
	assert.Contains(t, svc, "StandardOutput=append:/home/a b/.zmina/tick.log")

	timer := string(systemdTimer("/c/crew.yaml"))
	assert.Contains(t, timer, "OnCalendar=minutely")
	assert.Contains(t, timer, "WantedBy=timers.target")

	assert.Equal(t, `"a \"q\" \\ b"`, unitArg(`a "q" \ b`))
	assert.Equal(t, "50%%", unitEsc("50%"))
}
