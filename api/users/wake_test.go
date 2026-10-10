package users

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestWakeSchedulesKeepTheQuotaAndTheAlarmAcrossARestart(t *testing.T) {
	svc, gw, tenants, dsn := newTestService(t)
	u, _ := svc.Create(t.Context(), NewUser{Name: "qa1"})
	other, _ := svc.Create(t.Context(), NewUser{Name: "qa2"})
	_, _ = svc.Update(t.Context(), u.ID, Settings{MaxBoxesCap: new(4), MaxBoxes: new(4)})
	ids := []string{firstBoxID(u.ID), "cb-b1", "cb-b2", "cb-b3"}
	for i, id := range ids[1:] {
		if _, err := svc.AddBox(t.Context(), Box{ID: id, Owner: u.ID, Name: "box" + string(rune('1'+i))}); err != nil {
			t.Fatalf("AddBox: %v", err)
		}
	}
	next := time.Now().Add(time.Hour).Truncate(time.Second)
	schedule := func(box string) WakeSchedule {
		return WakeSchedule{Box: box, Owner: u.ID, Cron: "0 9 * * *", TZ: "Asia/Singapore", AwakeFor: 15 * time.Minute, NextAt: next}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var quota int
	for _, id := range ids {
		wg.Go(func() {
			err := svc.PutWakeSchedule(t.Context(), schedule(id), 3)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrQuota):
				quota++
			case err != nil:
				t.Errorf("PutWakeSchedule: %v", err)
			}
		})
	}
	wg.Wait()
	list, err := svc.WakeSchedules(t.Context(), u.ID)
	if err != nil || len(list) != 3 || quota != 1 {
		t.Fatalf("after 4 parallel puts: %d schedules, %d refused, %v; want 3 and 1", len(list), quota, err)
	}
	if err := svc.PutWakeSchedule(t.Context(), WakeSchedule{Box: list[0].Box, Owner: u.ID, Cron: "0 18 * * *", TZ: "UTC", AwakeFor: time.Minute, NextAt: next}, 3); err != nil {
		t.Errorf("replacing a schedule at the quota = %v, want it to replace", err)
	}
	if err := svc.PutWakeSchedule(t.Context(), WakeSchedule{Box: ids[0], Owner: other.ID, Cron: "0 9 * * *", TZ: "UTC", AwakeFor: time.Minute, NextAt: next}, 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("a schedule on another user's box = %v, want ErrNotFound", err)
	}

	stale := list[0]
	stale.NextAt = next.Add(time.Minute)
	if err := svc.SetWakeAlarm(t.Context(), stale); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetWakeAlarm of a replaced schedule = %v, want ErrNotFound", err)
	}
	if now, _ := svc.WakeSchedules(t.Context(), u.ID); !now[slices.IndexFunc(now, func(s WakeSchedule) bool { return s.Box == stale.Box })].NextAt.Equal(next) {
		t.Error("an alarm read before the schedule was replaced overwrote the new schedule")
	}

	w := list[1]
	w.NextAt, w.LastWakeAt, w.SleepAt = next.Add(24*time.Hour), next.Add(-time.Minute), next.Add(15*time.Minute)
	if err := svc.SetWakeAlarm(t.Context(), w); err != nil {
		t.Fatalf("SetWakeAlarm: %v", err)
	}
	reopened := openTestService(t, dsn, gw, tenants)
	all, err := reopened.WakeSchedules(t.Context(), "")
	if err != nil || len(all) != 3 {
		t.Fatalf("WakeSchedules after reopen = %d, %v; want 3", len(all), err)
	}
	i := slices.IndexFunc(all, func(s WakeSchedule) bool { return s.Box == w.Box })
	if i < 0 {
		t.Fatalf("schedule of %s is gone after reopen", w.Box)
	}
	if got := all[i]; !got.NextAt.Equal(w.NextAt) || !got.LastWakeAt.Equal(w.LastWakeAt) || !got.SleepAt.Equal(w.SleepAt) || got.AwakeFor != 15*time.Minute || got.TZ != "Asia/Singapore" {
		t.Errorf("schedule after reopen = %+v, want %+v", got, w)
	}
	if err := reopened.PutWakeSchedule(t.Context(), schedule(w.Box), 3); err != nil {
		t.Fatalf("PutWakeSchedule: %v", err)
	}
	if again, _ := reopened.WakeSchedules(t.Context(), u.ID); !keepsAlarm(again, w) {
		t.Errorf("a replaced schedule lost its alarm state: %+v", again)
	}

	if _, err := reopened.Update(t.Context(), u.ID, Settings{Disabled: new(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if all, _ := reopened.WakeSchedules(t.Context(), ""); len(all) != 0 {
		t.Errorf("the alarm sees %d schedules of a disabled user, want 0", len(all))
	}
	if own, _ := reopened.WakeSchedules(t.Context(), u.ID); len(own) != 3 {
		t.Errorf("a disabled user's own list has %d schedules, want 3", len(own))
	}
	if _, err := reopened.Update(t.Context(), u.ID, Settings{Disabled: new(false)}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := reopened.RemoveBox(t.Context(), u.ID, w.Box); err != nil {
		t.Fatalf("RemoveBox: %v", err)
	}
	if err := reopened.RemoveWakeSchedule(t.Context(), other.ID, list[0].Box); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveWakeSchedule by another user = %v, want ErrNotFound", err)
	}
	if err := reopened.RemoveWakeSchedule(t.Context(), u.ID, list[0].Box); err != nil {
		t.Errorf("RemoveWakeSchedule: %v", err)
	}
	if left, _ := reopened.WakeSchedules(t.Context(), ""); len(left) != 1 {
		t.Errorf("after removing a box and a schedule: %d schedules, want 1", len(left))
	}
	if err := reopened.Delete(t.Context(), u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if left, _ := reopened.WakeSchedules(t.Context(), ""); len(left) != 0 {
		t.Errorf("after deleting the user: %d schedules, want 0", len(left))
	}
}

func keepsAlarm(list []WakeSchedule, w WakeSchedule) bool {
	i := slices.IndexFunc(list, func(s WakeSchedule) bool { return s.Box == w.Box })
	return i >= 0 && list[i].Cron == "0 9 * * *" && list[i].LastWakeAt.Equal(w.LastWakeAt) && list[i].SleepAt.Equal(w.SleepAt)
}
