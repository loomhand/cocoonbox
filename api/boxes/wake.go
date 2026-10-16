package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/robfig/cron/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

const (
	wakeLead        = time.Minute
	defaultAwakeFor = 15 * time.Minute
	minAwakeFor     = time.Minute
	floorSamples    = 512
)

// WakeStore keeps the wake schedules and their alarm state; users.Service is the production one.
type WakeStore interface {
	PutWakeSchedule(ctx context.Context, w users.WakeSchedule, maxSchedules int) error
	// WakeSchedules returns owner's schedules, or those of every enabled user when owner is empty.
	WakeSchedules(ctx context.Context, owner string) ([]users.WakeSchedule, error)
	RemoveWakeSchedule(ctx context.Context, owner, box string) error
	// SetWakeAlarm stores w's alarm state, and is users.ErrNotFound when w's schedule changed or went since it was read.
	SetWakeAlarm(ctx context.Context, w users.WakeSchedule) error
}

// WakeLimits bound the wake schedules: how many one user has, how often one box wakes from hibernation, and how long it stays up.
type WakeLimits struct {
	MaxSchedules int
	Floor        time.Duration
	MaxAwakeFor  time.Duration
}

// alarmCall marks the context of the alarm's own Ensure, which is no reach.
type alarmCall struct{}

// EnableWake serves the wake schedules store keeps, within limits; RunAlarms acts on them.
func (s *Service) EnableWake(store WakeStore, limits WakeLimits) {
	s.wake = &wakeAlarms{store: store, limits: limits, now: time.Now, reached: map[string]time.Time{}}
}

func (s *Service) SetWakeSchedule(ctx context.Context, req *controlv1.SetWakeScheduleRequest) (*controlv1.WakeSchedule, error) {
	user, err := s.ownerOf(ctx, req.GetBoxId())
	if err != nil {
		return nil, err
	}
	if s.wake == nil {
		return nil, status.Error(codes.FailedPrecondition, "scheduled wakes are not configured")
	}
	w := users.WakeSchedule{
		Box: req.GetBoxId(), Owner: user, Cron: strings.TrimSpace(req.GetCron()), TZ: req.GetTz(),
		AwakeFor: cmp.Or(time.Duration(req.GetAwakeForS())*time.Second, defaultAwakeFor),
	}
	now := s.wake.now()
	sched, err := s.wake.check(w, now)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	w.NextAt = sched.Next(now)
	if err = s.wake.store.PutWakeSchedule(ctx, w, s.wake.limits.MaxSchedules); err != nil {
		return nil, registryError(ctx, err, "set wake schedule")
	}
	log.WithFunc("boxes.Service.SetWakeSchedule").WithField("box", w.Box).Infof(ctx, "wakes on %q in %s for %s, next at %s",
		w.Cron, w.TZ, w.AwakeFor, w.NextAt.Format(time.RFC3339))
	list, err := s.ListWakeSchedules(ctx, &controlv1.ListWakeSchedulesRequest{})
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(list.GetSchedules(), func(got *controlv1.WakeSchedule) bool { return got.GetBoxId() == w.Box }); i >= 0 {
		return list.GetSchedules()[i], nil
	}
	return toWakeSchedule(w), nil
}

func (s *Service) ListWakeSchedules(ctx context.Context, _ *controlv1.ListWakeSchedulesRequest) (*controlv1.ListWakeSchedulesResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	resp := &controlv1.ListWakeSchedulesResponse{}
	if s.wake == nil {
		return resp, nil
	}
	list, err := s.wake.store.WakeSchedules(ctx, user)
	if err != nil {
		return nil, registryError(ctx, err, "list wake schedules")
	}
	for _, w := range list {
		resp.Schedules = append(resp.Schedules, toWakeSchedule(w))
	}
	return resp, nil
}

func (s *Service) ClearWakeSchedule(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.ClearWakeScheduleResponse, error) {
	user, err := s.ownerOf(ctx, ref.GetBoxId())
	if err != nil {
		return nil, err
	}
	if s.wake == nil {
		return nil, status.Error(codes.FailedPrecondition, "scheduled wakes are not configured")
	}
	if err := s.wake.store.RemoveWakeSchedule(ctx, user, ref.GetBoxId()); err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "the box has no wake schedule")
		}
		return nil, registryError(ctx, err, "clear wake schedule")
	}
	log.WithFunc("boxes.Service.ClearWakeSchedule").WithField("box", ref.GetBoxId()).Info(ctx, "cleared the wake schedule")
	return &controlv1.ClearWakeScheduleResponse{}, nil
}

// Reached records that a client's call goes to box, which keeps up a box the alarm woke or an agent made; the returned func records its end.
func (s *Service) Reached(box string) func() {
	if s.wake != nil {
		s.wake.reach(box)
	}
	if s.idle == nil {
		return func() {}
	}
	return s.idle.enter(box)
}

// RunAlarms acts on the wake schedules every interval until ctx ends.
func (s *Service) RunAlarms(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.alarm(ctx, s.wake.now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) alarm(ctx context.Context, now time.Time) {
	logger := log.WithFunc("boxes.Service.alarm")
	list, err := s.wake.store.WakeSchedules(ctx, "")
	if err != nil {
		logger.Error(ctx, err, "list wake schedules")
		return
	}
	held := map[string]bool{}
	for _, w := range list {
		next := s.alarmFor(context.WithValue(auth.WithUser(ctx, w.Owner), alarmCall{}, true), w, now)
		if !sameAlarm(w, next) {
			if err := s.wake.store.SetWakeAlarm(context.WithoutCancel(ctx), next); err != nil && !errors.Is(err, users.ErrNotFound) {
				logger.WithField("box", w.Box).Error(ctx, err, "store the alarm")
			}
		}
		if !next.SleepAt.IsZero() {
			held[w.Box] = true
		}
	}
	s.wake.keep(held)
}

func (s *Service) alarmFor(ctx context.Context, w users.WakeSchedule, now time.Time) users.WakeSchedule {
	sched, err := s.wake.parse(w)
	if err != nil {
		log.WithFunc("boxes.Service.alarmFor").WithField("box", w.Box).Warnf(ctx, "skip schedule %q in %s: %v", w.Cron, w.TZ, err)
		return w
	}
	if !now.Before(w.NextAt.Add(-wakeLead)) {
		w = s.wakeFor(ctx, w, sched, now)
	}
	if !w.SleepAt.IsZero() {
		w = s.sleepAfter(ctx, w, now)
	}
	return w
}

func (s *Service) wakeFor(ctx context.Context, w users.WakeSchedule, sched cron.Schedule, now time.Time) users.WakeSchedule {
	logger := log.WithFunc("boxes.Service.wakeFor").WithField("box", w.Box)
	at := w.NextAt
	cur, err := s.current(ctx, w.Owner, w.Box)
	switch {
	case err != nil && !errors.Is(err, machines.ErrNotFound):
		logger.Error(ctx, err, "get box")
		return w
	case err == nil && cur.State == machines.StateHibernating && now.Before(at.Add(w.AwakeFor)):
		return w
	case err == nil && holdsMemory(cur):
		if !w.SleepAt.IsZero() {
			w.SleepAt = later(w.SleepAt, later(now, at).Add(w.AwakeFor))
		}
		logger.Infof(ctx, "box is up for %s; nothing to wake", at.Format(time.RFC3339))
	case now.Sub(w.LastWakeAt) < s.wake.limits.Floor:
		logger.Warnf(ctx, "skip the wake for %s: the box woke at %s, under the %s floor", at.Format(time.RFC3339), w.LastWakeAt.Format(time.RFC3339), s.wake.limits.Floor)
	default:
		woke := w
		woke.LastWakeAt, woke.SleepAt = now, later(now, at).Add(w.AwakeFor)
		if err := s.wake.store.SetWakeAlarm(context.WithoutCancel(ctx), woke); err != nil {
			if !errors.Is(err, users.ErrNotFound) {
				logger.Error(ctx, err, "store the wake")
			}
			return w
		}
		if _, err := s.Ensure(ctx, &controlv1.EnsureRequest{BoxId: w.Box}); err != nil {
			logger.Warnf(ctx, "wake for %s: %s", at.Format(time.RFC3339), status.Convert(err).Message())
			break
		}
		w = woke
		logger.Infof(ctx, "woke box for %s; hibernating it at %s unless a client reaches it", at.Format(time.RFC3339), w.SleepAt.Format(time.RFC3339))
	}
	w.NextAt = sched.Next(later(now, at))
	return w
}

// sleepAfter hands the box the alarm woke to the user once a client reached it, else hibernates it at w.SleepAt
// when it runs with no operation.
func (s *Service) sleepAfter(ctx context.Context, w users.WakeSchedule, now time.Time) users.WakeSchedule {
	logger := log.WithFunc("boxes.Service.sleepAfter").WithField("box", w.Box)
	cur, err := s.current(ctx, w.Owner, w.Box)
	switch {
	case errors.Is(err, machines.ErrNotFound):
		logger.Info(ctx, "box has no machine; the scheduled wake is over")
	case err != nil:
		logger.Error(ctx, err, "get box")
		return w
	case s.wake.reachedSince(w.Box, w.LastWakeAt):
		logger.Info(ctx, "a client reached the box since its scheduled wake; leaving it up")
	case now.Before(w.SleepAt) || cur.State == machines.StateStarting || (cur.State == machines.StateRunning && s.ops.activeFor(w.Box) != nil):
		return w
	case cur.State != machines.StateRunning:
		logger.Infof(ctx, "box is %s; the scheduled wake is over", stateWords[cur.State])
	default:
		if _, err := s.Hibernate(ctx, &controlv1.BoxRef{BoxId: w.Box}); err != nil {
			logger.Warnf(ctx, "hibernate after the scheduled wake: %s", status.Convert(err).Message())
			return w
		}
		logger.Info(ctx, "hibernating box after its scheduled wake")
	}
	w.SleepAt = time.Time{}
	return w
}

type wakeAlarms struct {
	store  WakeStore
	limits WakeLimits
	now    func() time.Time

	mu sync.Mutex
	// reached holds when a client last reached each box; a pass forgets the boxes the alarm keeps no longer awake.
	reached map[string]time.Time
}

func (a *wakeAlarms) check(w users.WakeSchedule, now time.Time) (cron.Schedule, error) {
	if w.AwakeFor < minAwakeFor || w.AwakeFor > a.limits.MaxAwakeFor {
		return nil, fmt.Errorf("awake_for must be in %s..%s, got %s", minAwakeFor, a.limits.MaxAwakeFor, w.AwakeFor)
	}
	sched, err := a.parse(w)
	if err != nil {
		return nil, err
	}
	t := sched.Next(now)
	if t.IsZero() {
		return nil, fmt.Errorf("cron %q never fires", w.Cron)
	}
	for range floorSamples {
		next := sched.Next(t)
		if next.IsZero() {
			break
		}
		if next.Sub(t) < a.limits.Floor {
			return nil, fmt.Errorf("cron %q fires %s apart (%s, then %s); a box wakes at most once per %s",
				w.Cron, next.Sub(t), t.Format(time.RFC3339), next.Format(time.RFC3339), a.limits.Floor)
		}
		t = next
	}
	return sched, nil
}

func (a *wakeAlarms) parse(w users.WakeSchedule) (cron.Schedule, error) {
	loc, err := time.LoadLocation(w.TZ)
	if err != nil || w.TZ == "" || w.TZ == "Local" {
		return nil, fmt.Errorf("tz %q must name an IANA zone, such as Asia/Singapore", w.TZ)
	}
	if strings.Contains(w.Cron, "=") {
		return nil, errors.New("cron must not name a zone; tz does")
	}
	sched, err := cron.ParseStandard("CRON_TZ=UTC " + w.Cron)
	if err != nil {
		return nil, err
	}
	return wallSchedule{sched: sched, loc: loc}, nil
}

func (a *wakeAlarms) reach(box string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reached[box] = a.now()
}

func (a *wakeAlarms) reachedSince(box string, t time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.reached[box]
	return ok && !at.Before(t)
}

func (a *wakeAlarms) keep(held map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	maps.DeleteFunc(a.reached, func(box string, _ time.Time) bool { return !held[box] })
}

// wallSchedule runs a cron on the wall clock of loc: a time a DST change skips fires once after the gap, and a time it repeats fires once.
type wallSchedule struct {
	sched cron.Schedule
	loc   *time.Location
}

func (w wallSchedule) Next(t time.Time) time.Time {
	l := t.In(w.loc)
	wall := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), time.UTC)
	for {
		n := w.sched.Next(wall)
		if n.IsZero() {
			return n
		}
		if at := w.instant(n); at.After(t) {
			return at
		}
		wall = n
	}
}

// instant is the first instant whose wall clock in w.loc reads wall, or the end of the DST gap wall falls in.
func (w wallSchedule) instant(wall time.Time) time.Time {
	at := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, w.loc)
	start, end := at.ZoneBounds()
	switch got := w.wallClock(at); {
	case got.After(wall):
		return start
	case got.Before(wall):
		return end
	}
	if start.IsZero() {
		return at
	}
	_, before := start.Add(-time.Nanosecond).Zone()
	_, offset := at.Zone()
	if earlier := at.Add(time.Duration(offset-before) * time.Second); earlier.Before(at) && w.wallClock(earlier).Equal(wall) {
		return earlier
	}
	return at
}

func (w wallSchedule) wallClock(t time.Time) time.Time {
	l := t.In(w.loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
}

func toWakeSchedule(w users.WakeSchedule) *controlv1.WakeSchedule {
	out := &controlv1.WakeSchedule{
		BoxId: w.Box, Cron: w.Cron, Tz: w.TZ, NextAtMs: w.NextAt.UnixMilli(),
		AwakeForS: uint32(w.AwakeFor / time.Second), //nolint:gosec // SetWakeSchedule bounds awake_for by max_awake_for
	}
	if !w.LastWakeAt.IsZero() {
		out.LastWakeAtMs = w.LastWakeAt.UnixMilli()
	}
	return out
}

func sameAlarm(a, b users.WakeSchedule) bool {
	return a.NextAt.Equal(b.NextAt) && a.LastWakeAt.Equal(b.LastWakeAt) && a.SleepAt.Equal(b.SleepAt)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
