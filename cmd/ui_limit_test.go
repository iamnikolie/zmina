package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uiClock is a clock the test moves.
type uiClock struct {
	mu sync.Mutex
	t  time.Time
}

func newUIClock() *uiClock { return &uiClock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *uiClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *uiClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// TestLimiterMakesAnAddressWaitAfterFiveWrongTries: five wrong tries are free, then each one is refused for a wait
// that doubles up to five minutes; a try made while it lasts is refused without being counted.
func TestLimiterMakesAnAddressWaitAfterFiveWrongTries(t *testing.T) {
	clock := newUIClock()
	l := newUILimiter(clock.now)

	for i := range uiFreeFails {
		assert.Zero(t, l.take("192.0.2.1"), "try %d is free", i+1)
	}

	assert.Equal(t, 5*time.Second, l.take("192.0.2.1"), "the sixth waits")
	clock.add(3 * time.Second)
	assert.Equal(t, 2*time.Second, l.take("192.0.2.1"), "what is left of the wait; the try is not counted")

	clock.add(2 * time.Second)
	assert.Zero(t, l.take("192.0.2.1"), "the wait is over: this one is tried, and counted")
	assert.Equal(t, 10*time.Second, l.take("192.0.2.1"), "twice as long now")

	for _, want := range []time.Duration{20, 40, 80, 160, 300, 300} {
		clock.add(l.take("192.0.2.1"))
		assert.Zero(t, l.take("192.0.2.1"))
		assert.Equal(t, want*time.Second, l.take("192.0.2.1"), "doubling up to five minutes")
	}

	assert.Zero(t, l.take("192.0.2.2"), "another address is not held up by it")
}

// TestLimiterAllTogether: many addresses, each under its five, still hold up everyone once too many wrong tries come
// in all.
func TestLimiterAllTogether(t *testing.T) {
	clock := newUIClock()
	l := newUILimiter(clock.now)

	for i := range uiFreeFailsAll {
		assert.Zero(t, l.take(fmt.Sprintf("192.0.2.%d", i+1)))
	}

	assert.Positive(t, l.take("198.51.100.1"), "an address that never failed waits too")

	clock.add(uiFailDelay)
	assert.Zero(t, l.take("198.51.100.1"))
}

// TestLimiterForgetsAndRefunds: a right secret clears its address; a quiet quarter hour clears every count.
func TestLimiterForgetsAndRefunds(t *testing.T) {
	clock := newUIClock()
	l := newUILimiter(clock.now)

	for range uiFreeFails {
		assert.Zero(t, l.take("192.0.2.1"))
	}

	require.Positive(t, l.take("192.0.2.1"))
	clock.add(uiFailDelay)
	require.Zero(t, l.take("192.0.2.1"))
	l.refund("192.0.2.1")

	for range uiFreeFails {
		assert.Zero(t, l.take("192.0.2.1"), "the address starts clean after a right secret")
	}

	for range uiFreeFails + 1 {
		l.take("192.0.2.9")
	}

	require.Positive(t, l.take("192.0.2.9"))
	clock.add(uiFailForget)
	assert.Zero(t, l.take("192.0.2.9"), "a quiet quarter hour, and the count starts again")
	assert.Equal(t, 1, l.per["192.0.2.9"].n)
}

// TestLimiterCountsTriesMadeAtTheSameTime: fifty tries at once do not all pass before the first is counted.
func TestLimiterCountsTriesMadeAtTheSameTime(t *testing.T) {
	l := newUILimiter(newUIClock().now)

	var (
		free atomic.Int32
		wg   sync.WaitGroup
	)

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if l.take("192.0.2.1") == 0 {
				free.Add(1)
			}
		}()
	}

	wg.Wait()
	assert.EqualValues(t, uiFreeFails, free.Load())
}

func TestLimiterKeepsABoundedNumberOfAddresses(t *testing.T) {
	l := newUILimiter(newUIClock().now)

	for i := range uiLimiterKeep + 100 {
		l.take(fmt.Sprintf("addr-%d", i))
	}

	assert.LessOrEqual(t, len(l.per), uiLimiterKeep+1)
}

func TestClientAddr(t *testing.T) {
	at := func(remote, host, forwarded string) string {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		r.RemoteAddr = remote

		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}

		return clientAddr(r, []string{"proxy.example"})
	}

	assert.Equal(t, "192.0.2.7", at("192.0.2.7:4000", "127.0.0.1:7517", ""))
	assert.Equal(t, "192.0.2.7", at("192.0.2.7:4000", "127.0.0.1:7517", "198.51.100.1"), "from anyone else a header means nothing")
	assert.Equal(t, "198.51.100.9", at("127.0.0.1:4000", "proxy.example", "203.0.113.1, 198.51.100.9"),
		"behind the named proxy: the address it appended, not the one the client wrote")
	assert.Equal(t, "127.0.0.1", at("127.0.0.1:4000", "proxy.example", ""), "a proxy that says nothing")
	assert.Equal(t, "127.0.0.1", at("127.0.0.1:4000", "127.0.0.1:7517", "198.51.100.9"), "a header with no proxy host")
	assert.Equal(t, "2001:db8:1:2::", at("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:4000", "127.0.0.1:7517", ""), "an IPv6 address counts as its /64")
}
