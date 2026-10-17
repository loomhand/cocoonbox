package main

import (
	"context"
	"sync"

	"github.com/loomhand/cocoonbox/api/cluster"
)

// leaderLoops are the background loops that one api of a cluster runs at a time.
type leaderLoops []func(ctx context.Context)

func (l *leaderLoops) add(fn func(ctx context.Context)) {
	*l = append(*l, fn)
}

// run starts the loops in this api alone without a ledger, else whenever it holds the cluster's leader lock.
func (l leaderLoops) run(ctx context.Context, ledger *cluster.Store) {
	start := func(ctx context.Context) {
		var wg sync.WaitGroup
		for _, fn := range l {
			wg.Go(func() { fn(ctx) })
		}
		wg.Wait()
	}
	if ledger == nil {
		go start(ctx)
		return
	}
	go ledger.Lead(ctx, start)
}
