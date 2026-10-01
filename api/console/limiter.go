package console

import (
	"sync"
	"time"
)

type failures struct {
	count int
	since time.Time
}

// limiter counts failed sign-ins per key in a fixed window.
type limiter struct {
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string]*failures
}

func newLimiter(window time.Duration) *limiter {
	return &limiter{window: window, now: time.Now, failures: map[string]*failures{}}
}

// blocked reports whether key reached limit failures within the window.
func (l *limiter) blocked(key string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.failures[key]
	return f != nil && l.now().Sub(f.since) < l.window && f.count >= limit
}

func (l *limiter) fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key, f := range l.failures {
		if now.Sub(f.since) >= l.window {
			delete(l.failures, key)
		}
	}
	for _, key := range keys {
		f := l.failures[key]
		if f == nil {
			f = &failures{since: now}
			l.failures[key] = f
		}
		f.count++
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}
