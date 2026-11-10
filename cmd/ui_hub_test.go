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

// A snapshot that was being made when a change came in is not kept: the next tick makes one after the change.
func TestUIHubKeepsNoSnapshotBegunBeforeAChange(t *testing.T) {
	var mu sync.Mutex
	state := "old"
	started, release := make(chan struct{}), make(chan struct{})
	first := true

	hub := newUIHub(func() uiSnapshot {
		mu.Lock()
		seen, wait := state, first
		first = false
		mu.Unlock()

		if wait {
			close(started)
			<-release
		}

		return uiSnapshot{Version: seen}
	})
	changes := hub.dropAfterChanges(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		state = "new"
		mu.Unlock()
	}))

	done := make(chan []byte)
	go func() { done <- hub.get() }()

	<-started
	changes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/x", nil))
	close(release)
	assert.Contains(t, string(<-done), `"version":"old"`, "the tick that began before the change")

	assert.Contains(t, string(hub.get()), `"version":"new"`, "the next tick")
}
