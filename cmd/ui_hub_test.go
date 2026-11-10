package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Five streams that tick within one poll share one snapshot; the next poll makes one more.
func TestUIHubSharesOneSnapshotAcrossStreams(t *testing.T) {
	clock := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	computed := 0
	hub := newUIHub(func() uiSnapshot {
		computed++
		time.Sleep(10 * time.Millisecond) // a slow shift: streams that ask meanwhile wait for this one

		return uiSnapshot{Version: "v"}
	})
	hub.now = func() time.Time { return clock }

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)

		go func() {
			defer wg.Done()
			assert.Contains(t, string(hub.get()), `"version":"v"`)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, computed, "five streams at once")

	clock = clock.Add(uiHubFresh / 2)
	hub.get()
	assert.Equal(t, 1, computed, "a stream ticking later in the same poll")

	clock = clock.Add(uiPoll)
	hub.get()
	assert.Equal(t, 2, computed, "the next poll reads the shift again")
}

// What the owner just did shows on the next tick: a request that may change something drops the shared snapshot.
func TestUIHubDropsTheSnapshotAfterAChange(t *testing.T) {
	computed := 0
	hub := newUIHub(func() uiSnapshot { computed++; return uiSnapshot{} })
	h := hub.dropAfterChanges(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	hub.get()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/state", nil))
	hub.get()
	assert.Equal(t, 1, computed, "a read keeps it")

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/todo/answer", nil))
	hub.get()
	assert.Equal(t, 2, computed, "a write drops it")
}
