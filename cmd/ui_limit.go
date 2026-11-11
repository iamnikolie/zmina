package cmd

import (
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// uiFreeFails is how many wrong secrets an address may present before it is made to wait; uiFreeFailsAll the same
	// for every address together, higher so that one stranger does not stop the owner for long.
	uiFreeFails    = 5
	uiFreeFailsAll = 30
	// The wait after the last free failure, doubled with each further one up to uiFailDelayMax.
	uiFailDelay    = 5 * time.Second
	uiFailDelayMax = 5 * time.Minute
	// uiFailForget is how long without a failure takes an address (or the whole) back to a clean count.
	uiFailForget = 15 * time.Minute
	// uiLimiterKeep bounds the addresses remembered; past it new ones share one count.
	uiLimiterKeep = 4096
)

// uiLimiter slows down whoever keeps presenting wrong secrets to the cockpit: a wrong token, a wrong sign-in code, a
// wrong login code. After uiFreeFails wrong ones from one address, or uiFreeFailsAll from all, every further try is
// refused for a wait that doubles, up to five minutes, and no secret is compared while it lasts. It lives in memory:
// a restart starts the counts again.
type uiLimiter struct {
	mu  sync.Mutex
	now func() time.Time
	per map[string]*uiFails
	all uiFails
}

// uiFails is one count of wrong tries: how many, when the last was, and until when the next is refused.
type uiFails struct {
	n     int
	last  time.Time
	until time.Time
}

func newUILimiter(now func() time.Time) *uiLimiter {
	return &uiLimiter{now: now, per: map[string]*uiFails{}}
}

// failDelay is the wait after the nth wrong try when free ones are allowed first; zero while n is within them.
func failDelay(n, free int) time.Duration {
	if n < free {
		return 0
	}

	d := uiFailDelay << min(n-free, 16)
	if d > uiFailDelayMax || d <= 0 {
		return uiFailDelayMax
	}

	return d
}

// take is asked before a secret is compared: zero means go ahead, and the try is counted as a wrong one at once, so
// tries made at the same moment cannot all pass before the first is counted; more than zero means refused for that
// long, the secret not looked at. A right secret gives the count back (refund).
func (l *uiLimiter) take(addr string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.forget(now)

	f := l.per[addr]
	if f == nil {
		if len(l.per) >= uiLimiterKeep {
			addr = "*"
		}

		if f = l.per[addr]; f == nil {
			f = &uiFails{}
			l.per[addr] = f
		}
	}

	if wait := max(f.until.Sub(now), l.all.until.Sub(now)); wait > 0 {
		return wait
	}

	f.count(now, uiFreeFails)
	l.all.count(now, uiFreeFailsAll)

	return 0
}

// refund takes back the count of a try that presented the right secret: the address starts clean, and the whole
// count loses the one try.
func (l *uiLimiter) refund(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.per[addr]; ok {
		delete(l.per, addr)
	} else {
		delete(l.per, "*")
	}

	if l.all.n > 0 {
		l.all.n--
		l.all.until = time.Time{}

		if d := failDelay(l.all.n, uiFreeFailsAll); d > 0 {
			l.all.until = l.all.last.Add(d)
		}
	}
}

func (f *uiFails) count(now time.Time, free int) {
	f.n++
	f.last = now

	if d := failDelay(f.n, free); d > 0 {
		f.until = now.Add(d)
	}
}

// forget drops what has been quiet for uiFailForget; the caller holds the lock.
func (l *uiLimiter) forget(now time.Time) {
	for addr, f := range l.per {
		if now.Sub(f.last) >= uiFailForget && !now.Before(f.until) {
			delete(l.per, addr)
		}
	}

	if l.all.n > 0 && now.Sub(l.all.last) >= uiFailForget && !now.Before(l.all.until) {
		l.all = uiFails{}
	}
}

// clientAddr is the address a request is counted under: the peer of the connection, or, from a local proxy named
// with --allow-host, the last address it appended to X-Forwarded-For (the first is whatever the client wrote). An
// IPv6 address counts as its /64, which one machine holds whole.
func clientAddr(r *http.Request, proxyHosts []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	ip := net.ParseIP(host)

	if ip != nil && ip.IsLoopback() && slices.Contains(proxyHosts, r.Host) {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			if from := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); from != nil {
				ip = from
			}
		}
	}

	switch {
	case ip == nil:
		return host
	case ip.To4() != nil:
		return ip.To4().String()
	default:
		return ip.Mask(net.CIDRMask(64, 128)).String()
	}
}
