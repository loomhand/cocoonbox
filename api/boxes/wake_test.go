package boxes

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

var testWakeLimits = WakeLimits{MaxSchedules: 3, Floor: time.Hour, MaxAwakeFor: time.Hour}

func TestSetWakeScheduleRefusesWhatTheLimitsForbid(t *testing.T) {
	tests := []struct {
		name string
		req  *controlv1.SetWakeScheduleRequest
		code codes.Code
		want string
	}{
		{name: "no zone", req: &controlv1.SetWakeScheduleRequest{Cron: "0 9 * * *"}, code: codes.InvalidArgument, want: "must name an IANA zone"},
		{name: "unknown zone", req: &controlv1.SetWakeScheduleRequest{Cron: "0 9 * * *", Tz: "Mars/Olympus"}, code: codes.InvalidArgument, want: "must name an IANA zone"},
		{name: "server zone", req: &controlv1.SetWakeScheduleRequest{Cron: "0 9 * * *", Tz: "Local"}, code: codes.InvalidArgument, want: "must name an IANA zone"},
		{name: "zone in the cron", req: &controlv1.SetWakeScheduleRequest{Cron: "CRON_TZ=UTC 0 9 * * *", Tz: "UTC"}, code: codes.InvalidArgument, want: "tz does"},
		{name: "six fields", req: &controlv1.SetWakeScheduleRequest{Cron: "0 0 9 * * *", Tz: "UTC"}, code: codes.InvalidArgument, want: "expected exactly 5 fields"},
		{name: "under the floor", req: &controlv1.SetWakeScheduleRequest{Cron: "*/30 * * * *", Tz: "UTC"}, code: codes.InvalidArgument, want: "at most once per 1h0m0s"},
		{name: "two minutes of one hour", req: &controlv1.SetWakeScheduleRequest{Cron: "0,59 9 * * *", Tz: "UTC"}, code: codes.InvalidArgument, want: "fires 59m0s apart"},
		{name: "never", req: &controlv1.SetWakeScheduleRequest{Cron: "0 9 30 2 *", Tz: "UTC"}, code: codes.InvalidArgument, want: "never fires"},
		{name: "awake too long", req: &controlv1.SetWakeScheduleRequest{Cron: "0 9 * * *", Tz: "UTC", AwakeForS: 7200}, code: codes.InvalidArgument, want: "awake_for must be in 1m0s..1h0m0s"},
		{name: "awake too short", req: &controlv1.SetWakeScheduleRequest{Cron: "0 9 * * *", Tz: "UTC", AwakeForS: 30}, code: codes.InvalidArgument, want: "awake_for must be in"},
		{name: "another user's box", req: &controlv1.SetWakeScheduleRequest{BoxId: BoxID("bob"), Cron: "0 9 * * *", Tz: "UTC"}, code: codes.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newWakeEnv(t)
			tt.req.BoxId = cmp.Or(tt.req.BoxId, aliceBox)
			_, err := e.svc.SetWakeSchedule(e.ctx, tt.req)
			if status.Code(err) != tt.code || !strings.Contains(status.Convert(err).Message(), tt.want) {
				t.Errorf("SetWakeSchedule = %v, want %s containing %q", err, tt.code, tt.want)
			}
			if len(e.store.list("")) != 0 {
				t.Error("a refused schedule was stored")
			}
		})
	}
}

func TestSetWakeScheduleStoresTheNextTimeAndTheQuota(t *testing.T) {
	e := newWakeEnv(t)
	got, err := e.svc.SetWakeSchedule(e.ctx, &controlv1.SetWakeScheduleRequest{BoxId: aliceBox, Cron: "0 9 * * *", Tz: "Asia/Singapore"})
	if err != nil {
		t.Fatalf("SetWakeSchedule: %v", err)
	}
	next := time.UnixMilli(got.GetNextAtMs()).In(mustZone(t, "Asia/Singapore"))
	if got.GetAwakeForS() != 900 || next.Hour() != 9 || next.Minute() != 0 || !next.After(time.Now()) || time.Until(next) > 24*time.Hour {
		t.Errorf("schedule = %v, next %s; want 15 min awake and the next 09:00 in Singapore", got, next)
	}
	e.store.quotaErr = fmt.Errorf("%w: 3 wake schedules is the most this user may have", users.ErrQuota)
	_, err = e.svc.SetWakeSchedule(e.ctx, &controlv1.SetWakeScheduleRequest{BoxId: aliceBox, Cron: "0 9 * * *", Tz: "UTC"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "3 wake schedules") {
		t.Errorf("SetWakeSchedule past the quota = %v, want FailedPrecondition", err)
	}
	if list, _ := e.svc.ListWakeSchedules(e.ctx, &controlv1.ListWakeSchedulesRequest{}); len(list.GetSchedules()) != 1 {
		t.Errorf("ListWakeSchedules = %v, want one", list)
	}
	if _, err := e.svc.ClearWakeSchedule(e.ctx, &controlv1.BoxRef{BoxId: aliceBox}); err != nil {
		t.Errorf("ClearWakeSchedule: %v", err)
	}
	if _, err := e.svc.ClearWakeSchedule(e.ctx, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.NotFound {
		t.Errorf("ClearWakeSchedule again = %v, want NotFound", err)
	}
}

func TestAlarmWakesAHibernatedBoxAndHibernatesItAfterAwakeFor(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)

	e.alarm(t, nine.Add(-61*time.Second))
	if e.fake.state(aliceBox) != machines.StateHibernated {
		t.Fatalf("box woke %s before its time", nine.Add(-61*time.Second))
	}
	e.alarm(t, nine.Add(-wakeLead))
	if e.fake.state(aliceBox) != machines.StateStarting {
		t.Fatalf("box is %v a minute before its time, want starting", e.fake.state(aliceBox))
	}
	w := e.store.get(aliceBox)
	if !w.NextAt.Equal(nine.Add(24*time.Hour)) || !w.SleepAt.Equal(nine.Add(15*time.Minute)) || !w.LastWakeAt.Equal(nine.Add(-wakeLead)) {
		t.Fatalf("alarm = next %s, sleep %s, last wake %s", w.NextAt, w.SleepAt, w.LastWakeAt)
	}
	e.running(t)

	e.alarm(t, nine.Add(14*time.Minute))
	if e.fake.state(aliceBox) != machines.StateRunning {
		t.Fatalf("box is %v inside awake_for, want running", e.fake.state(aliceBox))
	}
	e.alarm(t, nine.Add(15*time.Minute))
	if e.fake.state(aliceBox) != machines.StateHibernating {
		t.Fatalf("box is %v after awake_for, want hibernating", e.fake.state(aliceBox))
	}
	if w := e.store.get(aliceBox); !w.SleepAt.IsZero() {
		t.Errorf("sleep at %s after the hibernate, want none", w.SleepAt)
	}
}

func TestAClientThatReachesTheWokenBoxKeepsItUpAcrossARestart(t *testing.T) {
	tests := []struct {
		name  string
		reach func(e *wakeEnv) error
		want  machines.State
	}{
		{name: "an edge call", reach: func(e *wakeEnv) error {
			e.svc.Reached(aliceBox)()
			return nil
		}, want: machines.StateRunning},
		{name: "an Ensure", reach: func(e *wakeEnv) error {
			_, err := e.svc.Ensure(e.ctx, &controlv1.EnsureRequest{BoxId: aliceBox})
			return err
		}, want: machines.StateRunning},
		{name: "a Resolve of the api's own", reach: func(e *wakeEnv) error {
			_, _, err := e.svc.Resolve(e.ctx, alice, aliceBox)
			return err
		}, want: machines.StateHibernating},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newWakeEnv(t)
			e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
			nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
			e.alarm(t, nine.Add(-wakeLead))
			e.running(t)
			e.clock = nine.Add(time.Minute)
			if err := tt.reach(e); err != nil {
				t.Fatalf("reach the box: %v", err)
			}
			e.alarm(t, nine.Add(2*time.Minute))

			restarted := New(e.fake, testDefaults, newFakeRegistry())
			restarted.EnableWake(e.store, testWakeLimits)
			restarted.wake.now = func() time.Time { return e.clock }
			e.svc = restarted
			e.alarm(t, nine.Add(15*time.Minute))
			if got := e.fake.state(aliceBox); got != tt.want {
				t.Fatalf("box is %v after awake_for, want %v", got, tt.want)
			}
			if w := e.store.get(aliceBox); !w.SleepAt.IsZero() {
				t.Errorf("sleep at %s, want none", w.SleepAt)
			}
		})
	}
}

func TestAFailedWakeHoldsNothing(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
	e.fake.ensureErr = machines.ErrCapacity
	e.alarm(t, nine.Add(-wakeLead))
	if w := e.store.get(aliceBox); !w.SleepAt.IsZero() || !w.LastWakeAt.IsZero() || !w.NextAt.Equal(nine.Add(24*time.Hour)) {
		t.Errorf("alarm after a failed wake = next %s, sleep %s, last wake %s; want only the next time moved", w.NextAt, w.SleepAt, w.LastWakeAt)
	}
}

func TestAWakeOfAReplacedScheduleDoesNotHappen(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
	e.store.replaceOnRead = users.WakeSchedule{Box: aliceBox, Owner: alice, Cron: "0 18 * * *", TZ: "UTC", AwakeFor: 15 * time.Minute, NextAt: nine.Add(9 * time.Hour)}
	e.alarm(t, nine.Add(-wakeLead))
	if e.fake.state(aliceBox) != machines.StateHibernated {
		t.Errorf("box is %v, want the replaced schedule's time not to wake it", e.fake.state(aliceBox))
	}
	if w := e.store.get(aliceBox); w.Cron != "0 18 * * *" || !w.NextAt.Equal(nine.Add(9*time.Hour)) {
		t.Errorf("schedule = %+v, want the new one untouched", w)
	}
}

func TestAlarmWaitsForAnOperationBeforeHibernating(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
	e.alarm(t, nine.Add(-wakeLead))
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 2})

	e.alarm(t, nine.Add(15*time.Minute))
	if e.fake.state(aliceBox) != machines.StateRunning || e.store.get(aliceBox).SleepAt.IsZero() {
		t.Fatalf("box is %v while its wake operation runs, want running and still held", e.fake.state(aliceBox))
	}
	e.fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning, Revision: 2})
	e.idle(t)
	e.alarm(t, nine.Add(16*time.Minute))
	if e.fake.state(aliceBox) != machines.StateHibernating {
		t.Errorf("box is %v once the operation ended, want hibernating", e.fake.state(aliceBox))
	}
}

func TestAlarmLeavesABoxThatWasUpAlone(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
	e.alarm(t, nine.Add(-wakeLead))
	e.alarm(t, nine.Add(20*time.Minute))
	if e.fake.state(aliceBox) != machines.StateRunning || len(e.fake.specs) != 0 {
		t.Errorf("box is %v with %d Ensure calls, want running and untouched", e.fake.state(aliceBox), len(e.fake.specs))
	}
	if w := e.store.get(aliceBox); !w.SleepAt.IsZero() || !w.LastWakeAt.IsZero() || !w.NextAt.Equal(nine.Add(24*time.Hour)) {
		t.Errorf("alarm = %+v, want only the next time moved", w)
	}
}

func TestAlarmKeepsTheWakeFloor(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
	w := e.store.get(aliceBox)
	w.LastWakeAt = nine.Add(-30 * time.Minute)
	e.store.put(w)
	e.alarm(t, nine.Add(-wakeLead))
	if e.fake.state(aliceBox) != machines.StateHibernated {
		t.Errorf("box is %v half an hour after its last wake, want hibernated", e.fake.state(aliceBox))
	}
	if !e.store.get(aliceBox).NextAt.Equal(nine.Add(24 * time.Hour)) {
		t.Errorf("next at %s, want the skipped time passed", e.store.get(aliceBox).NextAt)
	}
}

func TestAlarmWakesOnceForTimesMissedWhileDown(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
	now := nine.Add(3*24*time.Hour + 2*time.Hour)
	e.alarm(t, now)
	if e.fake.state(aliceBox) != machines.StateStarting || len(e.fake.specs) != 1 {
		t.Fatalf("box is %v after %d Ensure calls, want one late wake", e.fake.state(aliceBox), len(e.fake.specs))
	}
	if w := e.store.get(aliceBox); !w.NextAt.Equal(nine.Add(4*24*time.Hour)) || !w.SleepAt.Equal(now.Add(15*time.Minute)) {
		t.Errorf("alarm = next %s, sleep %s; want the next future time and a full awake_for from the late wake", w.NextAt, w.SleepAt)
	}
}

func TestAlarmHibernatesAfterARestart(t *testing.T) {
	e := newWakeEnv(t)
	e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
	nine := e.schedule(t, "0 9 * * *", 15*time.Minute)
	e.alarm(t, nine.Add(-wakeLead))
	e.running(t)

	e.svc = New(e.fake, testDefaults, newFakeRegistry())
	e.svc.EnableWake(e.store, testWakeLimits)
	e.alarm(t, nine.Add(5*time.Minute))
	if e.fake.state(aliceBox) != machines.StateRunning {
		t.Fatalf("box is %v inside awake_for after the restart, want running", e.fake.state(aliceBox))
	}
	e.alarm(t, nine.Add(15*time.Minute))
	if e.fake.state(aliceBox) != machines.StateHibernating {
		t.Errorf("box is %v after awake_for, want hibernating by the restarted api", e.fake.state(aliceBox))
	}
}

func TestRunAlarmsWakesOnTheClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newWakeEnv(t)
		e.fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 1})
		at := time.Now().Add(3 * time.Minute)
		e.store.put(users.WakeSchedule{Box: aliceBox, Owner: alice, Cron: "* * * * *", TZ: "UTC", AwakeFor: time.Minute, NextAt: at})
		ctx, cancel := context.WithCancel(t.Context())
		go e.svc.RunAlarms(ctx, 15*time.Second)

		time.Sleep(time.Until(at) - wakeLead - time.Second)
		synctest.Wait()
		if e.fake.state(aliceBox) != machines.StateHibernated {
			t.Errorf("box is %v before its wake, want hibernated", e.fake.state(aliceBox))
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if e.fake.state(aliceBox) != machines.StateStarting {
			t.Errorf("box is %v a minute before its time, want starting", e.fake.state(aliceBox))
		}
		e.running(t)
		cancel()
	})
}

func TestWallScheduleFiresOnceAcrossDST(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	tests := []struct {
		name string
		zone string
		cron string
		from time.Time
		want []string
	}{
		{
			name: "spring forward fires after the gap", cron: "30 2 * * *", from: time.Date(2026, 3, 7, 12, 0, 0, 0, ny),
			want: []string{"2026-03-08T03:00:00-04:00", "2026-03-09T02:30:00-04:00"},
		},
		{
			name: "fall back fires at the first 01:30", cron: "30 1 * * *", from: time.Date(2026, 10, 31, 12, 0, 0, 0, ny),
			want: []string{"2026-11-01T01:30:00-04:00", "2026-11-02T01:30:00-05:00"},
		},
		{
			name: "inside the repeated hour the next time is tomorrow", cron: "30 1 * * *", from: time.Date(2026, 11, 1, 6, 20, 0, 0, time.UTC),
			want: []string{"2026-11-02T01:30:00-05:00"},
		},
		{
			name: "east of UTC fall back fires at the first 02:30", zone: "Europe/Berlin", cron: "30 2 * * *", from: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
			want: []string{"2026-10-25T02:30:00+02:00", "2026-10-26T02:30:00+01:00"},
		},
		{
			name: "east of UTC spring forward fires after the gap", zone: "Europe/Berlin", cron: "30 2 * * *", from: time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC),
			want: []string{"2026-03-29T03:00:00+02:00", "2026-03-30T02:30:00+02:00"},
		},
		{
			name: "southern fall back fires at the first 02:30", zone: "Australia/Sydney", cron: "30 2 * * *", from: time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC),
			want: []string{"2026-04-05T02:30:00+11:00", "2026-04-06T02:30:00+10:00"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zone := cmp.Or(tt.zone, "America/New_York")
			sched, err := (&wakeAlarms{}).parse(users.WakeSchedule{Cron: tt.cron, TZ: zone})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			loc := mustZone(t, zone)
			var got []string
			for at := tt.from; len(got) < len(tt.want); {
				next := sched.Next(at)
				if !next.After(at) {
					t.Fatalf("Next(%s) = %s, not after it", at, next)
				}
				at = next
				got = append(got, at.In(loc).Format(time.RFC3339))
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("times = %v, want %v", got, tt.want)
			}
		})
	}
}

type wakeEnv struct {
	fake  *fakeMachines
	svc   *Service
	store *fakeWakeStore
	ctx   context.Context
	clock time.Time
}

func newWakeEnv(t *testing.T) *wakeEnv {
	t.Helper()
	e := &wakeEnv{fake: newFakeMachines(), store: &fakeWakeStore{schedules: map[string]users.WakeSchedule{}}, ctx: auth.WithUser(t.Context(), alice)}
	e.svc = New(e.fake, testDefaults, newFakeRegistry())
	e.svc.EnableWake(e.store, testWakeLimits)
	return e
}

// alarm runs one pass at now on the clock the alarm and its reaches read.
func (e *wakeEnv) alarm(t *testing.T, now time.Time) {
	t.Helper()
	e.clock = now
	e.svc.wake.now = func() time.Time { return e.clock }
	e.svc.alarm(t.Context(), now)
}

// schedule stores cron for alice's box with its next time at 09:00 UTC tomorrow, which it returns.
func (e *wakeEnv) schedule(t *testing.T, cron string, awakeFor time.Duration) time.Time {
	t.Helper()
	nine := time.Now().UTC().Truncate(24 * time.Hour).Add(33 * time.Hour)
	e.store.put(users.WakeSchedule{Box: aliceBox, Owner: alice, Cron: cron, TZ: "UTC", AwakeFor: awakeFor, NextAt: nine})
	return nine
}

// running finishes the wake operation with the box running.
func (e *wakeEnv) running(t *testing.T) {
	t.Helper()
	e.fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning, Revision: 2})
	e.idle(t)
}

func (e *wakeEnv) idle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.svc.ops.activeFor(aliceBox) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the operation did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeWakeStore refuses an alarm write for a schedule that changed, as the users table does; replaceOnRead stands for a SetWakeSchedule
// that lands after the next read.
type fakeWakeStore struct {
	mu            sync.Mutex
	schedules     map[string]users.WakeSchedule
	quotaErr      error
	replaceOnRead users.WakeSchedule
}

func (s *fakeWakeStore) PutWakeSchedule(_ context.Context, w users.WakeSchedule, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quotaErr != nil {
		return s.quotaErr
	}
	old := s.schedules[w.Box]
	w.LastWakeAt, w.SleepAt = old.LastWakeAt, old.SleepAt
	s.schedules[w.Box] = w
	return nil
}

func (s *fakeWakeStore) WakeSchedules(_ context.Context, owner string) ([]users.WakeSchedule, error) {
	list := s.list(owner)
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.replaceOnRead; w.Box != "" {
		s.schedules[w.Box], s.replaceOnRead = w, users.WakeSchedule{}
	}
	return list, nil
}

func (s *fakeWakeStore) RemoveWakeSchedule(_ context.Context, owner, box string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.schedules[box]; !ok || w.Owner != owner {
		return users.ErrNotFound
	}
	delete(s.schedules, box)
	return nil
}

func (s *fakeWakeStore) SetWakeAlarm(_ context.Context, w users.WakeSchedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.schedules[w.Box]; !ok || old.Cron != w.Cron || old.TZ != w.TZ || old.AwakeFor != w.AwakeFor {
		return users.ErrNotFound
	}
	s.schedules[w.Box] = w
	return nil
}

func (s *fakeWakeStore) list(owner string) []users.WakeSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []users.WakeSchedule
	for _, w := range s.schedules {
		if owner == "" || w.Owner == owner {
			out = append(out, w)
		}
	}
	return out
}

func (s *fakeWakeStore) get(box string) users.WakeSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schedules[box]
}

func (s *fakeWakeStore) put(w users.WakeSchedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules[w.Box] = w
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone %s: %v", name, err)
	}
	return loc
}
