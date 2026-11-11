package tmux

import (
	"sync"
	"time"
)

// calls is the slowest tmux call since the last ResetSlowest. It is bookkeeping for the state lock's hold record
// (which call a long hold waited on): process-wide, so a call made by another goroutine counts too.
var calls struct {
	mu   sync.Mutex
	name string
	took time.Duration
}

// ResetSlowest forgets the slowest call so far.
func ResetSlowest() {
	calls.mu.Lock()
	defer calls.mu.Unlock()

	calls.name, calls.took = "", 0
}

// Slowest is the tmux subcommand (list-panes, capture-pane, ...) of the slowest call since ResetSlowest and how
// long it took; empty when none ran.
func Slowest() (string, time.Duration) {
	calls.mu.Lock()
	defer calls.mu.Unlock()

	return calls.name, calls.took
}

// track starts timing a call with these arguments; the function it returns, run when the call ends, notes it.
func track(args []string) func() {
	start := time.Now()

	return func() {
		took := time.Since(start)

		calls.mu.Lock()
		defer calls.mu.Unlock()

		if took > calls.took && len(args) > 0 {
			calls.name, calls.took = args[0], took
		}
	}
}
