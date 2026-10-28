package utils

import "sync"

type Locks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (l *Locks) Lock(key string) func() {
	l.mu.Lock()
	m := l.locks[key]
	if m == nil {
		if l.locks == nil {
			l.locks = map[string]*sync.Mutex{}
		}
		m = &sync.Mutex{}
		l.locks[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}
