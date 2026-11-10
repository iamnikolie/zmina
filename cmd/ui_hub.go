package cmd

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// uiHubFresh is how old a shared snapshot may be when a stream asks for one: under uiPoll, so a stream ticking every
// uiPoll still sees each change within one tick, and every other stream that ticks meanwhile shares the same one.
const uiHubFresh = uiPoll * 3 / 4

// uiHub makes the cockpit's snapshot once for every stream open at the time. A snapshot reads every crew's state and
// probes the screen of every role that is up; computed per tab, a phone, a desktop and a forgotten tab triple it.
type uiHub struct {
	compute func() uiSnapshot
	now     func() time.Time

	// busy is held while a snapshot is computed: a stream that asks meanwhile waits for it instead of making another.
	busy sync.Mutex
	mu   sync.Mutex
	raw  []byte
	at   time.Time
	// made counts snapshots computed, for the tests.
	made int
}

func newUIHub(compute func() uiSnapshot) *uiHub {
	return &uiHub{compute: compute, now: time.Now}
}

// get is a snapshot as JSON no older than uiHubFresh.
func (h *uiHub) get() []byte {
	if raw, ok := h.cached(); ok {
		return raw
	}

	h.busy.Lock()
	defer h.busy.Unlock()

	// Another stream may have made one while this one waited.
	if raw, ok := h.cached(); ok {
		return raw
	}

	raw, _ := json.Marshal(h.compute())

	h.mu.Lock()
	h.raw, h.at, h.made = raw, h.now(), h.made+1
	h.mu.Unlock()

	return raw
}

func (h *uiHub) cached() ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.raw, h.raw != nil && h.now().Sub(h.at) < uiHubFresh
}

// drop forgets the shared snapshot, so the next stream tick reads the shift again.
func (h *uiHub) drop() {
	h.mu.Lock()
	h.raw = nil
	h.mu.Unlock()
}

// dropAfterChanges forgets the shared snapshot after every request that may change something (anything but GET or
// HEAD): what the owner just did shows on the next tick, not one shared snapshot later.
func (h *uiHub) dropAfterChanges(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.drop()
		}
	})
}
