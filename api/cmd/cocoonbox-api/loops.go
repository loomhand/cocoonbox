package main

import (
	"context"
	"sync"

	"github.com/loomhand/cocoonbox/api/cluster"
)

// leaderLoops are the background loops that one api of a cluster runs at a time.
type leaderLoops []cluster.Work

func (l *leaderLoops) add(fn cluster.Work) {
	*l = append(*l, fn)
}

// run starts the loops in this api without a shared table, else whenever it holds the cluster's leader lock.
func (l *leaderLoops) run(ctx context.Context, shared *cluster.Store) {
	start := func(ctx context.Context) {
		var wg sync.WaitGroup
		for _, fn := range *l {
			wg.Go(func() { fn(ctx) })
		}
		wg.Wait()
	}
	if shared == nil {
		go start(ctx)
		return
	}
	go shared.Lead(ctx, start)
}
