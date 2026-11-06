package sandboxd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/loomhand/cocoonbox/api/cluster/clustertest"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	benchOwners  = 50
	benchOwner   = "u-00"
	benchMachine = "cb-0000"
	benchSeedFan = 16
	benchSettle  = time.Minute
)

var benchSizes = []int{1, 100, 1000}

func BenchmarkListOwner(b *testing.B) {
	benchSeeded(b, func(ctx context.Context, be *Backend) error {
		_, err := be.List(ctx, benchOwner)
		return err
	})
}

func BenchmarkListAll(b *testing.B) {
	benchSeeded(b, func(ctx context.Context, be *Backend) error {
		_, err := be.List(ctx, "")
		return err
	})
}

func BenchmarkAddress(b *testing.B) {
	benchSeeded(b, func(ctx context.Context, be *Backend) error {
		_, err := be.Address(ctx, benchMachine)
		return err
	})
}

func BenchmarkResolveShape(b *testing.B) {
	benchSeeded(b, func(ctx context.Context, be *Backend) error {
		ms, err := be.List(ctx, benchOwner)
		if err != nil {
			return err
		}
		if len(ms) == 0 {
			return machines.ErrNotFound
		}
		_, err = be.Address(ctx, ms[0].ID)
		return err
	})
}

func benchSeeded(b *testing.B, op func(context.Context, *Backend) error) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("machines=%d", n), func(b *testing.B) {
			be := seedBackend(b, n)
			b.Run("serial", func(b *testing.B) {
				for b.Loop() {
					if err := op(b.Context(), be); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("parallel", func(b *testing.B) {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if err := op(b.Context(), be); err != nil {
							b.Error(err)
							return
						}
					}
				})
			})
		})
	}
}

func seedBackend(b *testing.B, n int) *Backend {
	b.Helper()
	fake := newFakeSandboxd(b)
	be, err := New(b.Context(), Options{URL: fake.srv.URL, Token: rootToken, Image: template, Flavors: []machines.Flavor{xlarge}, DiskGiB: 40, AgentPort: 7443, Shared: clustertest.Open(b)})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	go be.Run(b.Context())
	var g errgroup.Group
	g.SetLimit(benchSeedFan)
	for i := range n {
		g.Go(func() error {
			_, _, err := be.Ensure(b.Context(), machines.Spec{ID: fmt.Sprintf("cb-%04d", i), Owner: fmt.Sprintf("u-%02d", i%benchOwners), Flavor: xlarge})
			return err
		})
	}
	if err := g.Wait(); err != nil {
		b.Fatalf("Ensure: %v", err)
	}
	deadline := time.Now().Add(benchSettle)
	for {
		ms, err := be.List(b.Context(), "")
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		running := 0
		for _, m := range ms {
			if m.State == machines.StateRunning {
				running++
			}
		}
		if running == n {
			return be
		}
		if time.Now().After(deadline) {
			b.Fatalf("%d of %d machines running after %s", running, n, benchSettle)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
