package cluster

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	leaderLock  = 0x6362_6c65_6164 // "cblead"
	leaderRetry = 5 * time.Second
	leaderCheck = 2 * time.Second
	// leaderIdle ends the session of a leader whose checks stop reaching the server, which frees the lock.
	leaderIdle = 3 * leaderCheck
	// leaderGrace outlasts the failing check by which an old leader stops, before a new one starts.
	leaderGrace   = 2*leaderCheck + time.Second
	unlockTimeout = 5 * time.Second
)

// Work runs until its context ends.
type Work func(ctx context.Context)

// Lead runs lead while this process holds the leader lock, until ctx ends; lead's context ends when the lock is lost, and Lead waits for lead to return before it competes again.
func (s *Store) Lead(ctx context.Context, lead Work) {
	logger := log.WithFunc("cluster.Store.Lead")
	for ctx.Err() == nil {
		held, err := s.leadOnce(ctx, lead)
		switch {
		case ctx.Err() != nil:
		case held:
			logger.Warnf(ctx, "lost the leader lock: %v", err)
		case err != nil:
			logger.Warnf(ctx, "leader lock: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(leaderRetry):
		}
	}
}

// LockUser holds the advisory lock of user's starts until the returned func runs, on a connection of the lock pool.
func (s *Store) LockUser(ctx context.Context, user string) (func(), error) {
	conn, err := s.locks.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	key := userLock(user)
	drop := func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		_ = conn.Hijack().Close(closeCtx)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		// A canceled wait may still have taken the lock, which only closing the session drops.
		drop()
		return nil, fmt.Errorf("lock the starts of %s: %w", user, err)
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			drop()
			return
		}
		conn.Release()
	}, nil
}

// leadOnce waits for the leader lock and runs lead until the lock is lost; it reports whether it held the lock.
func (s *Store) leadOnce(ctx context.Context, lead Work) (bool, error) {
	pooled, err := s.locks.Acquire(ctx)
	if err != nil {
		return false, err
	}
	conn := pooled.Hijack()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err = conn.Exec(ctx, fmt.Sprintf("SET idle_session_timeout = %d", leaderIdle.Milliseconds())); err != nil {
		return false, err
	}
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(leaderLock)); err != nil {
		return false, err
	}
	log.WithFunc("cluster.Store.leadOnce").Info(ctx, "took the leader lock")
	check := func() error {
		checkCtx, cancel := context.WithTimeout(ctx, leaderCheck)
		defer cancel()
		_, checkErr := conn.Exec(checkCtx, "SELECT 1")
		return checkErr
	}
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-time.After(leaderGrace):
	}
	if err = check(); err != nil {
		return true, err
	}
	leading, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lead(leading)
	}()
	ticker := time.NewTicker(leaderCheck)
	defer ticker.Stop()
	for err == nil {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-ticker.C:
			err = check()
		}
	}
	stop()
	<-done
	return true, err
}

func userLock(user string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("cocoonbox-starts:" + user))
	return int64(h.Sum64()) //nolint:gosec // a lock key, its sign does not matter
}
