package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// The programs zmina asks a question of and waits for (ps, launchctl, systemctl, loginctl) run under a deadline: a
// service manager whose bus is gone, or a ps stuck on a dying process, must not hold the beat or a command for good.
// The deadlines are variables so tests can shorten them.
var (
	// psTimeout is for ps, which a beat may run for each role it reads.
	psTimeout = 5 * time.Second
	// serviceTimeout is for launchctl, systemctl and loginctl: starting or stopping a unit may take a moment.
	serviceTimeout = 30 * time.Second
)

// toolWaitDelay is how long Wait keeps waiting for a program's output once the program itself has ended or was
// killed: a process it started may hold the pipe far longer.
const toolWaitDelay = 2 * time.Second

// runTool runs name with args and returns its output (with combined, its standard error too). It ends after
// timeout: the program is killed and the error says so.
func runTool(timeout time.Duration, combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = toolWaitDelay

	var out bytes.Buffer

	cmd.Stdout = &out

	if combined {
		cmd.Stderr = &out
	}

	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%s: no answer in %s", name, timeout)
	}

	return out.Bytes(), err
}
