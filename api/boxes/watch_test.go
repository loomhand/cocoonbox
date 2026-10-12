package boxes

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/machines"
)

func TestWatchSendsEveryChangeOfTheCallersBoxesAndNothingWhileIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeMachines()
		fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateRunning, Flavor: defaultFlavor, Revision: 1})
		svc := New(fake, testDefaults, newFakeRegistry())
		ctx := auth.WithUser(t.Context(), alice)
		mine := startWatch(t, svc, alice)
		bobs := startWatch(t, svc, "bob")

		first := mine.next(t, func(*controlv1.ListBoxesResponse) bool { return true })
		if len(first.GetBoxes()) != 1 || first.GetBoxes()[0].GetState() != controlv1.BoxState_BOX_STATE_RUNNING {
			t.Fatalf("first = %v", first)
		}
		synctest.Wait()
		idleFrom := fake.listCalls()
		time.Sleep(time.Hour)
		synctest.Wait()
		if calls := fake.listCalls(); calls != idleFrom {
			t.Errorf("an idle Watch listed the machines %d times", calls-idleFrom)
		}

		op, err := svc.Hibernate(ctx, &controlv1.BoxRef{BoxId: aliceBox})
		if err != nil {
			t.Fatalf("Hibernate: %v", err)
		}
		fake.push(machines.Machine{ID: aliceBox, State: machines.StateHibernating})
		fake.push(machines.Machine{ID: aliceBox, State: machines.StateHibernated})
		mine.next(t, stateIs(controlv1.BoxState_BOX_STATE_HIBERNATED))
		watch(t, svc, ctx, op.GetId(), "")

		wake, err := svc.Ensure(ctx, &controlv1.EnsureRequest{BoxId: aliceBox})
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		mine.next(t, stateIs(controlv1.BoxState_BOX_STATE_STARTING))
		fake.push(machines.Machine{ID: aliceBox, State: machines.StateRunning})
		mine.next(t, stateIs(controlv1.BoxState_BOX_STATE_RUNNING))
		watch(t, svc, ctx, wake.GetOperation().GetId(), "")

		if _, err := svc.Rename(ctx, &controlv1.RenameBoxRequest{BoxId: aliceBox, Name: "work"}); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		mine.next(t, func(r *controlv1.ListBoxesResponse) bool {
			return len(r.GetBoxes()) == 1 && r.GetBoxes()[0].GetName() == "work"
		})

		fake.put(machines.Machine{ID: aliceBox, Owner: alice, State: machines.StateHibernated, Flavor: defaultFlavor, Revision: 3})
		mine.next(t, stateIs(controlv1.BoxState_BOX_STATE_HIBERNATED))

		again := startWatch(t, svc, alice)
		if resync := again.next(t, func(*controlv1.ListBoxesResponse) bool { return true }); !stateIs(controlv1.BoxState_BOX_STATE_HIBERNATED)(resync) {
			t.Errorf("a new Watch starts with %v, want the current list", resync)
		}

		del, err := svc.Delete(ctx, &controlv1.BoxRef{BoxId: aliceBox})
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
		mine.next(t, func(r *controlv1.ListBoxesResponse) bool { return len(r.GetBoxes()) == 0 })
		watch(t, svc, ctx, del.GetId(), "")

		all := mine.seen()
		for i := 1; i < len(all); i++ {
			if proto.Equal(all[i-1], all[i]) {
				t.Errorf("message %d repeats the one before: %v", i, all[i])
			}
		}
		if got := bobs.seen(); len(got) != 1 || len(got[0].GetBoxes()) != 1 || got[0].GetBoxes()[0].GetId() != BoxID("bob") {
			t.Errorf("bob's Watch saw %v, want only his first list", got)
		}
	})
}

type boxStream struct {
	grpc.ServerStream
	ctx context.Context
	out chan *controlv1.ListBoxesResponse
	all []*controlv1.ListBoxesResponse
}

func (s *boxStream) Context() context.Context { return s.ctx }

func (s *boxStream) Send(r *controlv1.ListBoxesResponse) error {
	s.out <- r
	return nil
}

func (s *boxStream) next(t *testing.T, match func(*controlv1.ListBoxesResponse) bool) *controlv1.ListBoxesResponse {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case r := <-s.out:
			s.all = append(s.all, r)
			if match(r) {
				return r
			}
		case <-timeout:
			t.Fatalf("no matching list; saw %v", s.all)
			return nil
		}
	}
}

func (s *boxStream) seen() []*controlv1.ListBoxesResponse {
	for {
		select {
		case r := <-s.out:
			s.all = append(s.all, r)
		default:
			return s.all
		}
	}
}

func startWatch(t *testing.T, svc *Service, user string) *boxStream {
	t.Helper()
	stream := &boxStream{ctx: auth.WithUser(t.Context(), user), out: make(chan *controlv1.ListBoxesResponse, 64)}
	go func() { _ = svc.Watch(&controlv1.WatchBoxesRequest{}, stream) }()
	return stream
}

func stateIs(state controlv1.BoxState) func(*controlv1.ListBoxesResponse) bool {
	return func(r *controlv1.ListBoxesResponse) bool {
		return len(r.GetBoxes()) == 1 && r.GetBoxes()[0].GetState() == state
	}
}
