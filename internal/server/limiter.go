package server

import (
	"sync"
	"time"
)

type bucket struct {
	start time.Time
	count int
}
type limiter struct {
	mu          sync.Mutex
	values      map[string]bucket
	lastCleanup time.Time
}

const maxLimiterEntries = 10_000

func newLimiter() *limiter { return &limiter{values: map[string]bucket{}, lastCleanup: time.Now()} }
func (l *limiter) allow(key string, max int, window time.Duration) (bool, int, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.lastCleanup) >= min(window, time.Minute) {
		for existingKey, existing := range l.values {
			if now.Sub(existing.start) >= window {
				delete(l.values, existingKey)
			}
		}
		l.lastCleanup = now
	}
	current := l.values[key]
	if current.start.IsZero() && len(l.values) >= maxLimiterEntries {
		return false, 0, now.Add(window)
	}
	if current.start.IsZero() || now.Sub(current.start) >= window {
		current = bucket{start: now}
	}
	current.count++
	l.values[key] = current
	remaining := max - current.count
	if remaining < 0 {
		remaining = 0
	}
	return current.count <= max, remaining, current.start.Add(window)
}
