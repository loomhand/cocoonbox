package boxes

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/gwkeys"
)

type Meter interface {
	Usage(ctx context.Context, user string, since time.Time) ([]gwkeys.Usage, error)
	DailyCap(ctx context.Context, user string) (*int64, error)
}

func (s *Service) EnableUsage(meter Meter, defaultCap int64) {
	s.usage = &usageMeter{meter: meter, defaultCap: defaultCap}
}

func (s *Service) Usage(ctx context.Context, _ *controlv1.UsageRequest) (*controlv1.UsageResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	if s.usage == nil {
		return nil, status.Error(codes.FailedPrecondition, "usage is not metered")
	}
	now := time.Now().UTC()
	day := now.Truncate(24 * time.Hour)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	resp := &controlv1.UsageResponse{}
	if resp.Today, err = s.usage.period(ctx, user, day); err != nil {
		return nil, err
	}
	if resp.Month, err = s.usage.period(ctx, user, month); err != nil {
		return nil, err
	}
	own, err := s.usage.meter.DailyCap(ctx, user)
	if err != nil {
		return nil, registryError(ctx, err, "read the daily cap")
	}
	resp.DailyCapMicros, resp.DefaultCap = s.usage.defaultCap, own == nil
	if own != nil {
		resp.DailyCapMicros = *own
	}
	resp.RemainingTodayMicros = -1
	if resp.DailyCapMicros > 0 {
		resp.RemainingTodayMicros = max(resp.DailyCapMicros-resp.Today.GetCostMicros(), 0)
	}
	return resp, nil
}

type usageMeter struct {
	meter      Meter
	defaultCap int64
}

func (m *usageMeter) period(ctx context.Context, user string, since time.Time) (*controlv1.UsagePeriod, error) {
	rows, err := m.meter.Usage(ctx, user, since)
	if err != nil {
		return nil, registryError(ctx, err, "read gw usage")
	}
	p := &controlv1.UsagePeriod{SinceMs: since.UnixMilli()}
	for _, r := range rows {
		p.Requests += r.Requests
		p.PromptTokens += r.PromptTokens
		p.CompletionTokens += r.CompletionTokens
		p.TotalTokens += r.TotalTokens
		p.CostMicros += r.CostMicros
		p.Models = append(p.Models, &controlv1.ModelUsage{
			Model: r.Model, Requests: r.Requests, PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
			TotalTokens: r.TotalTokens, CostMicros: r.CostMicros,
		})
	}
	return p, nil
}
