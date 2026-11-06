package console

import (
	"maps"
	"sync"
	"time"
)

type failures struct {
	count int
	since time.Time
}

// limiter counts attempts per key in a fixed window.
type limiter struct {
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string]*failures
}

func newLimiter(window time.Duration) *limiter {
	return &limiter{window: window, now: time.Now, failures: map[string]*failures{}}
}

// take reserves both attempts under the lock so parallel requests cannot pass the check together.
func (l *limiter) take(name, addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	maps.DeleteFunc(l.failures, func(_ string, f *failures) bool { return now.Sub(f.since) >= l.window })
	if l.failures[name] != nil && l.failures[name].count >= nameLimit || l.failures[addr] != nil && l.failures[addr].count >= addrLimit {
		return false
	}
	for _, key := range []string{name, addr} {
		f := l.failures[key]
		if f == nil {
			f = &failures{since: now}
			l.failures[key] = f
		}
		f.count++
	}
	return true
}

func (l *limiter) allow(key string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	maps.DeleteFunc(l.failures, func(_ string, f *failures) bool { return now.Sub(f.since) >= l.window })
	f := l.failures[key]
	if f == nil {
		f = &failures{since: now}
		l.failures[key] = f
	}
	if f.count >= limit {
		return false
	}
	f.count++
	return true
}

func (l *limiter) settle(name, addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, name)
	if f := l.failures[addr]; f != nil {
		f.count--
	}
}
