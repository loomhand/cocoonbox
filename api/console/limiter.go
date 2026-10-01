package console

import (
	"sync"
	"time"
)

type failures struct {
	count int
	since time.Time
}

// limiter counts password attempts per name and per client address in a fixed window; a correct password gives its attempt back.
type limiter struct {
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string]*failures
}

func newLimiter(window time.Duration) *limiter {
	return &limiter{window: window, now: time.Now, failures: map[string]*failures{}}
}

// take reserves one attempt for the name and address keys unless either is at its limit, so parallel requests cannot pass the check together.
func (l *limiter) take(name, addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key, f := range l.failures {
		if now.Sub(f.since) >= l.window {
			delete(l.failures, key)
		}
	}
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

// settle clears name's attempts and returns the address's reserved one after a correct password.
func (l *limiter) settle(name, addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, name)
	if f := l.failures[addr]; f != nil {
		f.count--
	}
}
