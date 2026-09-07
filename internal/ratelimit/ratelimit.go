// Package ratelimit implements simple brute-force protection: too many
// failed authentication attempts from the same key (normally a client IP)
// within a trailing window triggers a temporary ban.
//
// The limiter itself doesn't know or care what a "key" represents — it's
// the caller's job to compute the *real* client identity before calling in,
// which for anything reachable through a reverse proxy means resolving it
// via internal/realip rather than trusting a raw connection's peer address
// or an unauthenticated header. Passing the wrong key here (e.g. a proxy's
// own address because a forwarded-for header was blindly trusted) is
// exactly how you ban your own reverse proxy and take the service down.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

type entry struct {
	failures    []time.Time
	bannedUntil time.Time
}

// Limiter tracks failed attempts per key and temporarily bans a key once it
// accumulates MaxAttempts failures inside Window.
type Limiter struct {
	MaxAttempts int
	Window      time.Duration
	BanDuration time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

func New(maxAttempts int, window, banDuration time.Duration) *Limiter {
	return &Limiter{
		MaxAttempts: maxAttempts,
		Window:      window,
		BanDuration: banDuration,
		entries:     make(map[string]*entry),
	}
}

// Allowed reports whether key is currently permitted to attempt
// authentication (i.e. isn't under an active ban).
func (l *Limiter) Allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		return true
	}
	return time.Now().After(e.bannedUntil)
}

// RecordFailure records a failed attempt for key, starting a ban if that
// pushes it to MaxAttempts failures within Window.
func (l *Limiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	e, ok := l.entries[key]
	if !ok {
		e = &entry{}
		l.entries[key] = e
	}

	cutoff := now.Add(-l.Window)
	kept := e.failures[:0]
	for _, t := range e.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	e.failures = append(kept, now)

	if len(e.failures) >= l.MaxAttempts {
		e.bannedUntil = now.Add(l.BanDuration)
		e.failures = nil
	}
}

// RecordSuccess clears key's failure history: a successful authentication
// resets the counter rather than letting stale failures carry toward a
// future ban.
func (l *Limiter) RecordSuccess(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// cleanup drops entries with no ban in effect and no failures still inside
// the window, so long-running processes don't accumulate one entry per
// distinct IP ever seen.
func (l *Limiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.Window)
	for k, e := range l.entries {
		if now.Before(e.bannedUntil) {
			continue
		}
		stale := true
		for _, t := range e.failures {
			if t.After(cutoff) {
				stale = false
				break
			}
		}
		if stale {
			delete(l.entries, k)
		}
	}
}

// Run periodically sweeps stale entries until ctx is canceled.
func (l *Limiter) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.cleanup()
		}
	}
}
