package boxes

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/gwkeys"
)

func TestUsageSumsTodayAndMonthByModelUnderTheCap(t *testing.T) {
	now := time.Now().UTC()
	day := now.Truncate(24 * time.Hour)
	meter := &fakeMeter{
		rows: map[time.Time][]gwkeys.Usage{
			day: {
				{Model: "anthropic/claude-sonnet-5", Requests: 2, PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100, CostMicros: 4_000_000},
				{Model: "typesafe/jev-1.13", Requests: 3, PromptTokens: 900, CompletionTokens: 30, TotalTokens: 930, CostMicros: 1_500_000},
			},
			time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC): {
				{Model: "typesafe/jev-1.13", Requests: 7, PromptTokens: 2100, CompletionTokens: 70, TotalTokens: 2170, CostMicros: 3_500_000},
			},
		},
		caps: map[string]int64{"bob": gwkeys.Unlimited, "carol": 10_000_000},
	}
	svc := &Service{}
	svc.EnableUsage(meter, 5_000_000)

	got, err := svc.Usage(auth.WithUser(t.Context(), "alice"), &controlv1.UsageRequest{})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	today := got.GetToday()
	if today.GetSinceMs() != day.UnixMilli() || today.GetRequests() != 5 || today.GetPromptTokens() != 1900 || today.GetCompletionTokens() != 130 ||
		today.GetTotalTokens() != 2030 || today.GetCostMicros() != 5_500_000 || len(today.GetModels()) != 2 || today.GetModels()[1].GetRequests() != 3 {
		t.Errorf("today = %v", today)
	}
	if month := got.GetMonth(); month.GetRequests() != 7 || month.GetCostMicros() != 3_500_000 || month.GetModels()[0].GetModel() != "typesafe/jev-1.13" {
		t.Errorf("month = %v", month)
	}
	if got.GetDailyCapMicros() != 5_000_000 || !got.GetDefaultCap() || got.GetRemainingTodayMicros() != 0 {
		t.Errorf("cap %d default %t remaining %d, want the 5000000 default spent past", got.GetDailyCapMicros(), got.GetDefaultCap(), got.GetRemainingTodayMicros())
	}

	for user, want := range map[string][2]int64{"bob": {gwkeys.Unlimited, -1}, "carol": {10_000_000, 4_500_000}} {
		got, err := svc.Usage(auth.WithUser(t.Context(), user), &controlv1.UsageRequest{})
		if err != nil || got.GetDailyCapMicros() != want[0] || got.GetDefaultCap() || got.GetRemainingTodayMicros() != want[1] {
			t.Errorf("Usage of %s = %v, %v; want cap %d and %d left", user, got, err, want[0], want[1])
		}
	}

	unknown := &Service{}
	unknown.EnableUsage(meter, 0)
	if got, err := unknown.Usage(auth.WithUser(t.Context(), "alice"), &controlv1.UsageRequest{}); err != nil || got.GetDailyCapMicros() != 0 || got.GetRemainingTodayMicros() != -1 {
		t.Errorf("Usage without a known default = %v, %v; want cap 0 and -1 left", got, err)
	}
}

func TestUsageRefusals(t *testing.T) {
	svc := &Service{}
	if _, err := svc.Usage(t.Context(), &controlv1.UsageRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Usage without a user = %v, want Unauthenticated", err)
	}
	if _, err := svc.Usage(auth.WithUser(t.Context(), "alice"), &controlv1.UsageRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Usage without a meter = %v, want FailedPrecondition", err)
	}
	svc.EnableUsage(&fakeMeter{err: errors.New("GET /admin/usage/users: 502 Bad Gateway")}, 5_000_000)
	if _, err := svc.Usage(auth.WithUser(t.Context(), "alice"), &controlv1.UsageRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("Usage while gw fails = %v, want Unavailable", err)
	}
}

type fakeMeter struct {
	rows map[time.Time][]gwkeys.Usage
	caps map[string]int64
	err  error
}

func (m *fakeMeter) Usage(_ context.Context, _ string, since time.Time) ([]gwkeys.Usage, error) {
	return m.rows[since], m.err
}

func (m *fakeMeter) DailyCap(_ context.Context, user string) (*int64, error) {
	if c, ok := m.caps[user]; ok {
		return &c, nil
	}
	return nil, m.err
}
