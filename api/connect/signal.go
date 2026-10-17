package connect

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	signalChannel = "cocoonbox_connect"
	signalRetry   = 5 * time.Second
	signalTimeout = 5 * time.Second
)

// signal is a change of a user's watchers one api tells the others; Answer, when set, is an approval answer to push.
type signal struct {
	Origin string `json:"origin"`
	User   string `json:"user"`
	Answer []byte `json:"answer,omitempty"`
}

// RunSignals wakes the watchers here for the changes other apis signal, until ctx ends.
func (s *Service) RunSignals(ctx context.Context) {
	for {
		err := s.listenSignals(ctx)
		if ctx.Err() != nil {
			return
		}
		log.WithFunc("connect.Service.RunSignals").Warnf(ctx, "listen on %s: %v", signalChannel, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(signalRetry):
		}
	}
}

// changedFor wakes user's watchers in every api.
func (s *Service) changedFor(ctx context.Context, user string) {
	s.watchers.notify(user)
	s.publish(ctx, signal{User: user})
}

// answerFor pushes the approval answer msg to user's watchers in every api.
func (s *Service) answerFor(ctx context.Context, user string, msg []byte) {
	s.watchers.answer(user, msg)
	s.publish(ctx, signal{User: user, Answer: msg})
}

func (s *Service) publish(ctx context.Context, sig signal) {
	sig.Origin = s.origin
	raw, err := json.Marshal(sig)
	if err == nil {
		notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signalTimeout)
		_, err = s.store.pool.Exec(notifyCtx, "SELECT pg_notify($1, $2)", signalChannel, string(raw))
		cancel()
	}
	if err != nil {
		log.WithFunc("connect.Service.publish").Warnf(ctx, "the watchers of %s in other apis miss a change: %v", sig.User, err)
	}
}

func (s *Service) listenSignals(ctx context.Context) error {
	pooled, err := s.store.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire a connection: %w", err)
	}
	conn := pooled.Hijack()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+signalChannel); err != nil {
		return err
	}
	s.watchers.notifyAll()
	s.replayAnswers(ctx)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var sig signal
		if json.Unmarshal([]byte(n.Payload), &sig) != nil || sig.Origin == s.origin {
			continue
		}
		if sig.Answer != nil {
			s.watchers.answer(sig.User, sig.Answer)
			continue
		}
		s.watchers.notify(sig.User)
	}
}

func (s *Service) replayAnswers(ctx context.Context) {
	for _, user := range s.watchers.boxUsers() {
		answered, err := s.store.answered(ctx, user)
		if err != nil {
			log.WithFunc("connect.Service.replayAnswers").Warnf(ctx, "the box watchers of %s miss the answers signaled while none listened: %v", user, err)
			continue
		}
		for _, a := range answered {
			s.watchers.answer(user, approvalAnswered(a.id, a.decision))
		}
	}
}
