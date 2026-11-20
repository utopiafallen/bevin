package server

import (
	"sync"
	"time"
)

// Login rate limiting (security 2026-11-20): a brute force against /login
// must not run unthrottled. Two independent fixed-window caps:
//
//   - per client IP: one attacker is bounded (even if they spread guesses
//     across many handles);
//   - per handle: one target account is bounded (even if the guesses come
//     from many IPs).
//
// A SUCCESSFUL login resets that handle's counter, so a few legitimate
// typos never lock a user out. In memory (a restart clears the counters)
// with an opportunistic sweep of stale entries (the fileReadSets pattern).

const (
	defaultLoginLimit  = 10              // attempts per key...
	defaultLoginWindow = 5 * time.Minute // ...per window
)

type loginCounters struct {
	n       int
	started time.Time
}

func (c *loginCounters) hit(now time.Time, window time.Duration) (int, time.Duration) {
	if now.Sub(c.started) > window {
		c.n = 0
		c.started = now
	}
	c.n++
	return c.n, now.Sub(c.started)
}

func (c *loginCounters) stale(now time.Time, window time.Duration) bool {
	return now.Sub(c.started) > window
}

type loginLimiter struct {
	mu       sync.Mutex
	byIP     map[string]*loginCounters
	byHandle map[string]*loginCounters
	limit    int
	window   time.Duration
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		byIP:     map[string]*loginCounters{},
		byHandle: map[string]*loginCounters{},
		limit:    limit,
		window:   window,
	}
}

// attempt records one login attempt for (ip, handle) and reports whether it
// is allowed (true) or throttled (false, with the time to the next window).
// The caps are independent: exceeding EITHER one throttles.
func (l *loginLimiter) attempt(ip, handle string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// Opportunistic sweep: drop keys whose window has long since closed
	// (bound the maps on a long-lived process without a janitor).
	sweep := func(m map[string]*loginCounters) {
		for k, c := range m {
			if c.stale(now, 8*l.window) {
				delete(m, k)
			}
		}
	}
	if len(l.byIP) > 1024 {
		sweep(l.byIP)
	}
	if len(l.byHandle) > 1024 {
		sweep(l.byHandle)
	}
	get := func(m map[string]*loginCounters, key string) *loginCounters {
		c := m[key]
		if c == nil {
			c = &loginCounters{}
			m[key] = c
		}
		return c
	}
	ipN, ipAge := get(l.byIP, ip).hit(now, l.window)
	hN, hAge := get(l.byHandle, handle).hit(now, l.window)
	if ipN > l.limit || hN > l.limit {
		retry := l.window - max(ipAge, hAge)
		if retry < 0 {
			retry = 0
		}
		return false, retry
	}
	return true, 0
}

// reset clears a handle's counter (a successful login).
func (l *loginLimiter) reset(handle string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byHandle, handle)
}
