package boxes

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/users"
)

func TestStartChecksShareTheUserLockAcrossReplicas(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{false: "ensure", true: "create"}[create], func(t *testing.T) {
			shared, dsn := clustertest.OpenDSN(t)
			other := clustertest.OpenReplica(t, dsn)
			backend, registry := newFakeMachines(), newFakeRegistry()
			registry.boxes[alice] = []users.Box{{ID: aliceBox, Owner: alice, Name: "first", Agent: true}, {ID: "cb-other", Owner: alice, Name: "second", Agent: true}}
			first, second := newService(backend, testDefaults, registry), newService(backend, testDefaults, registry)
			first.EnableLedger(t.Context(), shared)
			second.EnableLedger(t.Context(), other)
			first.EnableShared(t.Context(), shared)
			second.EnableShared(t.Context(), other)
			entered, resume := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			refused := errors.New("grant running cap reached")
			check := func(context.Context) error {
				if backend.specCount() != 0 {
					return refused
				}
				close(entered)
				<-resume
				return nil
			}
			ctx := WithStartCheck(auth.WithUser(t.Context(), alice), check)
			start := func(svc *Service, box, name string) error {
				if create {
					_, err := svc.Create(WithAgent(ctx, func(context.Context, string) error { return nil }), &controlv1.CreateBoxRequest{Name: name})
					return err
				}
				_, err := svc.Ensure(ctx, &controlv1.EnsureRequest{BoxId: box})
				return err
			}
			done := make(chan error, 2)
			go func() { done <- start(first, aliceBox, "one") }()
			<-entered
			go func() { done <- start(second, "cb-other", "two") }()
			clustertest.WaitFor(t, "the second start waits for the first check", clustertest.AdvisoryWaiting(t, dsn))
			release()
			var denied int
			for range 2 {
				if err := <-done; errors.Is(err, refused) {
					denied++
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if denied != 1 || backend.specCount() != 1 {
				t.Errorf("denied %d starts, made %d machines; want one of each", denied, backend.specCount())
			}
		})
	}
}
