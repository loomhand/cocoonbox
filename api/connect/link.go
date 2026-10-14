package connect

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/projecteru2/core/log"

	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	spendInterval   = 30 * time.Second
	spendBackoffMax = 10 * time.Minute
	spendMaxShift   = 5
)

var waitingStatuses = []string{"pending_approval", "requires_action"}

type spendPoll struct {
	requests []*controlv1.SpendRequest
	polledAt time.Time
	failures int
	next     time.Time
}

type linkSpendRequest struct {
	ID           string `json:"id"`
	Amount       int64  `json:"amount"`
	Currency     string `json:"currency"`
	MerchantName string `json:"merchant_name"`
	MerchantURL  string `json:"merchant_url"`
	Status       string `json:"status"`
	ApprovalURL  string `json:"approval_url"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at"`
}

// spendRequests returns user's last read of its waiting spend requests, read from Link first when that read is older
// than spendInterval and no failure backoff holds; concurrent callers share one read.
func (s *Service) spendRequests(ctx context.Context, user string) spendPoll {
	v, _, _ := s.spendReads.Do(user, func() (any, error) {
		s.spendMu.Lock()
		p := s.spends[user]
		due := p == nil || !time.Now().Before(p.next)
		s.spendMu.Unlock()
		if due {
			s.pollSpend(context.WithoutCancel(ctx), user)
		}
		s.spendMu.Lock()
		defer s.spendMu.Unlock()
		if p := s.spends[user]; p != nil {
			return *p, nil
		}
		return spendPoll{}, nil
	})
	return v.(spendPoll)
}

func (s *Service) pollSpend(ctx context.Context, user string) {
	requests, err := s.readSpends(ctx, user)
	now := time.Now()
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	p := s.spends[user]
	if p == nil {
		p = &spendPoll{}
		s.spends[user] = p
	}
	if err != nil {
		p.failures++
		p.next = now.Add(min(spendInterval<<min(p.failures, spendMaxShift), spendBackoffMax))
		log.WithFunc("connect.Service.pollSpend").Warnf(ctx, "read the spend requests of %s (failure %d): %v", user, p.failures, err)
		return
	}
	p.requests, p.polledAt, p.failures, p.next = requests, now, 0, now.Add(spendInterval)
}

func (s *Service) readSpends(ctx context.Context, user string) ([]*controlv1.SpendRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	g, err := s.linkGrant(ctx, user)
	if err != nil {
		return nil, err
	}
	body, err := s.spendsBody(ctx, g)
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []linkSpendRequest `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode the spend requests: %w", err)
	}
	var out []*controlv1.SpendRequest
	for _, r := range list.Data {
		if !slices.Contains(waitingStatuses, r.Status) {
			continue
		}
		sr := &controlv1.SpendRequest{
			Id: r.ID, Amount: r.Amount, Currency: r.Currency, MerchantName: r.MerchantName, MerchantUrl: r.MerchantURL,
			Status: r.Status, ApprovalUrl: r.ApprovalURL, ExpiresAtMs: r.ExpiresAt * 1000,
		}
		if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
			sr.CreatedAtMs = t.UnixMilli()
		}
		out = append(out, sr)
	}
	return out, nil
}

func (s *Service) spendsBody(ctx context.Context, g *grant) ([]byte, error) {
	var body []byte
	err := s.withCredential(ctx, g, func(u *upstream) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.linkAPI+"/spend_requests", nil)
		if err != nil {
			return err
		}
		req.Header.Set(headerAuthorization, u.cred.value)
		req.Header.Set("Accept", "application/json")
		status, answer, err := s.send(req)
		switch {
		case err != nil:
			return err
		case status == http.StatusUnauthorized:
			return errUnauthorized
		case status != http.StatusOK:
			return fmt.Errorf("link answered %d", status)
		}
		body = answer
		return nil
	})
	return body, err
}

// linkGrant returns user's active Link grant; a custom connector named link is not one.
func (s *Service) linkGrant(ctx context.Context, user string) (*grant, error) {
	g, err := s.store.grant(ctx, user, linkID)
	if err != nil {
		return nil, err
	}
	if g.kind != linkID || g.status != statusActive {
		return nil, errNoGrant
	}
	return g, nil
}
