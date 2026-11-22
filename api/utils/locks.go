package utils

import "sync"

type Locks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (l *Locks) Lock(key string) func() {
	m := l.mutex(key)
	m.Lock()
	return m.Unlock
}

func (l *Locks) TryLock(key string) (func(), bool) {
	m := l.mutex(key)
	if !m.TryLock() {
		return nil, false
	}
	return m.Unlock, true
}

func (l *Locks) mutex(key string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.locks[key]
	if m == nil {
		if l.locks == nil {
			l.locks = map[string]*sync.Mutex{}
		}
		m = &sync.Mutex{}
		l.locks[key] = m
	}
	return m
}
