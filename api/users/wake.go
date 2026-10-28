package users

import (
	"context"
	"time"
)

// WakeSchedule wakes Box at the times of Cron in TZ for AwakeFor; NextAt, LastWakeAt and SleepAt are the alarm's state.
type WakeSchedule struct {
	Box        string
	Owner      string
	Cron       string
	TZ         string
	AwakeFor   time.Duration
	NextAt     time.Time
	LastWakeAt time.Time
	SleepAt    time.Time // non-zero while the alarm keeps the box it woke awake; it hibernates the box then
}

// PutWakeSchedule replaces the schedule of w's box and keeps its alarm state, ErrQuota once the owner has max other schedules.
func (s *Service) PutWakeSchedule(ctx context.Context, w WakeSchedule, maxSchedules int) error {
	return s.store.putWakeSchedule(ctx, w, maxSchedules)
}

// WakeSchedules returns owner's schedules, or those of every enabled user when owner is empty.
func (s *Service) WakeSchedules(ctx context.Context, owner string) ([]WakeSchedule, error) {
	return s.store.wakeSchedules(ctx, owner)
}

// RemoveWakeSchedule removes the schedule of owner's box.
func (s *Service) RemoveWakeSchedule(ctx context.Context, owner, box string) error {
	return s.store.removeWakeSchedule(ctx, owner, box)
}

// SetWakeAlarm stores the alarm state of w (NextAt, LastWakeAt and SleepAt), ErrNotFound when its schedule changed or went since w was read.
func (s *Service) SetWakeAlarm(ctx context.Context, w WakeSchedule) error {
	return s.store.setWakeAlarm(ctx, w)
}
