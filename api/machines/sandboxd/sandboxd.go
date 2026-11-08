// Package sandboxd runs machines as claims of a cocoonbox sandboxd instance.
package sandboxd

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/utils"
)

const (
	netNone                = "none"
	leaseSeconds           = 3600
	onExpire               = "archive"
	identityTimeoutSeconds = 30
	screenPort             = 5900

	refreshInterval = 2 * time.Second
	roomInterval    = 30 * time.Second
	renewInterval   = 5 * time.Minute
	watchInterval   = 500 * time.Millisecond
	opTimeout       = 10 * time.Minute
	loopTimeout     = 30 * time.Second
	renewTimeout    = 4 * time.Minute
	renewCall       = 10 * time.Second
	renewFanout     = 16
	probeTimeout    = 2 * time.Second
	probeFanout     = 8
	sharedTimeout   = 10 * time.Second
	ownerTimeout    = 10 * time.Second
	nodeLostAfter   = 5 * time.Minute
)

var writeIdentity = []string{"sh", "-c", `set -e; umask 022; mkdir -p /run/cocoonbox-identity; chmod 0755 /run/cocoonbox-identity
t=$(mktemp /run/cocoonbox-identity/.identity.XXXXXX); printf '%s' "$CB_STDIN" > "$t"; chmod 0644 "$t"; mv -f "$t" /run/cocoonbox-identity/identity`}

// Options names the sandboxd instance and the claims the backend makes on it.
type Options struct {
	URL string
	// Token is the instance's root api token.
	Token string
	// Image is the default template.
	Image string
	// Flavor is the flavor every box takes; it must be a sandboxd tier.
	Flavor machines.Flavor
	// DiskGiB is the disk the node gives each claim.
	DiskGiB           uint32
	AgentPort         int
	TenantEgressClass string
	// Nodes lists the node addresses the backend may call besides the entry node; empty allows every node the mesh names.
	Nodes []string
	// Store holds the machine records: every change lands on a row's version.
	Store *cluster.Store
}

// Check reports whether New refuses opts before it reads the instance.
func (o Options) Check() error {
	_, err := o.client()
	return err
}

func (o Options) client() (*client, error) {
	if _, ok := sizeOf(o.Flavor); !ok {
		return nil, fmt.Errorf("flavor vcpus:%d memory_mib:%d is not a sandboxd tier", o.Flavor.VCPUs, o.Flavor.MemoryMiB)
	}
	return newClient(o.URL, o.Token)
}

type task func(context.Context) error

var _ machines.Machines = (*Backend)(nil)

// Backend implements machines.Machines and machines.Tenants on a sandboxd instance, or the mesh it enters, that holds only cocoonbox claims.
type Backend struct {
	*Tenants

	opts                 Options
	api                  *client
	entryAddr            string
	ctx                  context.Context
	leaseFor, renewEvery time.Duration

	nodeMu sync.Mutex
	nodes  map[string]*client
	peers  []string

	mu       sync.Mutex
	seen     map[string]seenClaim
	watchers map[string]chan struct{}
	owners   map[string]chan struct{}
	// away maps a node that machines name to when the refresh first saw it out of the mesh.
	away map[string]time.Time
	full map[string]bool
}

// New checks opts, reads the instance's claims and keeps them indexed until ctx ends.
func New(ctx context.Context, opts Options) (*Backend, error) {
	api, err := opts.client()
	if err != nil {
		return nil, err
	}
	advertised, err := api.advertised(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the sandboxd entry node: %w", err)
	}
	b := &Backend{
		Tenants:   &Tenants{api: api, egressClass: opts.TenantEgressClass},
		opts:      opts,
		api:       api,
		entryAddr: cmp.Or(advertised, opts.URL),
		ctx:       ctx,
		nodes:     map[string]*client{},
		seen:      map[string]seenClaim{},
		watchers:  map[string]chan struct{}{},
		owners:    map[string]chan struct{}{},
		away:      map[string]time.Time{},
		full:      map[string]bool{},

		leaseFor: cluster.Lease, renewEvery: cluster.LeaseRenew,
	}
	if err = b.checkNodes(ctx, advertised); err != nil {
		return nil, err
	}
	go opts.Store.Listen(ctx, cluster.ChannelMachines, b.publishAll, func(payload string) {
		c, err := cluster.ParseChange(payload)
		if err != nil {
			log.WithFunc("sandboxd.New").Warnf(ctx, "take a machines notify: %v", err)
			return
		}
		b.publish(c.ID, c.Owner)
	})
	if err := b.refresh(ctx); err != nil {
		return nil, fmt.Errorf("read sandboxd claims: %w", err)
	}
	go b.loop(ctx, renewInterval, loopTimeout, func(ctx context.Context) error {
		_, err := b.snapshot(ctx)
		return err
	})
	return b, nil
}

// Run re-reads the claims and renews their leases until ctx ends; one api of the cluster runs it for all.
func (b *Backend) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { b.loop(ctx, refreshInterval, loopTimeout, b.refresh) })
	wg.Go(func() { b.loop(ctx, roomInterval, loopTimeout, b.readRoom) })
	wg.Go(func() { b.loop(ctx, renewInterval, renewTimeout, b.renew) })
	wg.Wait()
}

func (b *Backend) Ensure(ctx context.Context, spec machines.Spec) (machines.Machine, machines.Action, error) {
	size, ok := sizeOf(spec.Flavor)
	if !ok {
		return machines.Machine{}, machines.ActionNone, fmt.Errorf("flavor vcpus:%d memory_mib:%d is not a sandboxd tier", spec.Flavor.VCPUs, spec.Flavor.MemoryMiB)
	}
	if _, err := b.lookup(ctx, spec.ID); err != nil && !errors.Is(err, machines.ErrNotFound) {
		return machines.Machine{}, machines.ActionNone, err
	}
	template, action, sandbox, addr, wakes := "", machines.ActionNone, "", "", 0
	e, err := b.mutate(ctx, spec.ID, func(e *entry) (*entry, bool) {
		now := time.Now()
		action = machines.ActionNone
		switch {
		case e == nil || (e.row == nil && !e.busy(now)):
			template = cmp.Or(spec.Image, b.opts.Image)
			if e != nil {
				template = cmp.Or(spec.Image, e.image)
			}
			e = &entry{id: spec.ID, owner: spec.Owner, image: template, created: now, meta: maps.Clone(spec.Meta)}
			e.begin(opClaim, now, b.leaseFor)
			action = machines.ActionCreated
			return e, true
		case e.busy(now):
			return e, false
		}
		if state, _ := e.state(); state == machines.StateHibernated || state == machines.StateFailed {
			sandbox, addr, wakes = e.row.ID, e.ownerAddr, e.wakes+1
			e.begin(opWake, now, b.leaseFor)
			action = machines.ActionWoken
			return e, true
		}
		return e, false
	})
	if err != nil {
		return machines.Machine{}, machines.ActionNone, err
	}
	switch action {
	case machines.ActionCreated:
		b.run(spec.ID, opClaim, e.revision, func(ctx context.Context) error { return b.claim(ctx, spec, template, size) })
	case machines.ActionWoken:
		b.run(spec.ID, opWake, e.revision, func(ctx context.Context) error { return b.wake(ctx, spec.ID, addr, sandbox, wakes) })
	}
	return b.machine(e), action, nil
}

func (b *Backend) Get(ctx context.Context, id string) (machines.Machine, error) {
	e, err := b.lookup(ctx, id)
	if err != nil {
		return machines.Machine{}, err
	}
	return b.machine(e), nil
}

func (b *Backend) List(ctx context.Context, owner string) ([]machines.Machine, error) {
	var recs []cluster.Machine
	var err error
	if owner == "" {
		recs, err = b.opts.Store.Machines(ctx)
	} else {
		recs, err = b.opts.Store.MachinesOf(ctx, owner)
	}
	if err != nil {
		return nil, err
	}
	out := make([]machines.Machine, 0, len(recs))
	for _, rec := range recs {
		out = append(out, b.machine(loadEntry(rec)))
	}
	return out, nil
}

func (b *Backend) Hibernate(ctx context.Context, id string) (machines.Machine, error) {
	if _, err := b.lookup(ctx, id); err != nil {
		return machines.Machine{}, err
	}
	started, missing, starting, sandbox, addr := false, false, false, "", ""
	e, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		now := time.Now()
		started, missing, starting = false, e == nil || e.row == nil && !e.busy(now), false
		if missing {
			return e, false
		}
		state, _ := e.state()
		if state == machines.StateRunning || state == machines.StateStarting && !e.busy(now) {
			sandbox, addr, started = e.row.ID, e.ownerAddr, true
			e.begin(opHibernate, now, b.leaseFor)
			return e, true
		}
		starting = state == machines.StateStarting
		return e, false
	})
	switch {
	case err != nil:
		return machines.Machine{}, err
	case missing:
		return machines.Machine{}, machines.ErrNotFound
	case starting:
		return machines.Machine{}, fmt.Errorf("machine %s is being claimed or woken: %w", id, machines.ErrConflict)
	}
	if started {
		b.run(id, opHibernate, e.revision, func(ctx context.Context) error {
			at, err := b.atOwner(ctx, id, addr, sandbox, func(node *client) error { return node.verb(ctx, sandbox, "hibernate", nil) })
			if err != nil {
				return err
			}
			return b.reread(ctx, id, addr, at, sandbox)
		})
	}
	return b.machine(e), nil
}

func (b *Backend) Watch(ctx context.Context, id string, revision int64) <-chan machines.Machine {
	out := make(chan machines.Machine)
	go func() {
		defer close(out)
		tick := time.Tick(watchInterval)
		seen, last := false, ""
		for {
			changed := b.changes(id)
			e, err := b.lookup(ctx, id)
			switch {
			case errors.Is(err, machines.ErrNotFound) && seen:
				send(ctx, out, machines.Machine{ID: id, State: machines.StateGone})
				return
			case err == nil:
				seen = true
				if m := b.machine(b.probe(ctx, e)); m.Revision >= revision {
					if key := fmt.Sprintf("%d|%s|%s|%d|%t|%v", m.State, m.Reason, m.Incarnation, m.Revision, m.EnvMissing, m.Meta); key != last {
						if !send(ctx, out, m) {
							return
						}
						last = key
					}
				}
			}
			select {
			case <-tick:
			case <-changed:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (b *Backend) Changes(owner string) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return waitOn(b.owners, owner)
}

func (b *Backend) Address(ctx context.Context, id string) (machines.Endpoint, error) {
	e, err := b.stored(ctx, id)
	if err != nil {
		return machines.Endpoint{}, err
	}
	if state, _ := e.state(); state != machines.StateRunning {
		return machines.Endpoint{}, &machines.NotRunningError{State: state}
	}
	node, err := b.node(e.ownerAddr)
	if err != nil {
		return machines.Endpoint{}, err
	}
	sandbox := e.row.ID
	key := net.JoinHostPort(sandbox+"."+utils.HashHex(e.ownerAddr)[:32]+".sandboxd", strconv.Itoa(b.opts.AgentPort))
	return machines.Endpoint{
		Key: key,
		Dial: func(ctx context.Context) (net.Conn, error) {
			return node.dialPort(ctx, sandbox, b.opts.AgentPort)
		},
		Screen: func(ctx context.Context) (net.Conn, error) {
			return node.dialPort(ctx, sandbox, screenPort)
		},
	}, nil
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	now, claiming := time.Now(), false
	unclaimed, err := b.drop(ctx, id, func(e *entry) bool {
		claiming = e.row == nil && e.busy(now)
		return e.row == nil && !e.busy(now)
	})
	switch {
	case err != nil:
		return err
	case claiming:
		return fmt.Errorf("machine %s is being claimed: %w", id, machines.ErrConflict)
	}
	e, node, err := b.sandboxOf(ctx, id)
	if unclaimed && errors.Is(err, machines.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = node.verb(ctx, e.row.ID, "release", nil); err != nil {
		return err
	}
	_, err = b.drop(ctx, id, func(*entry) bool { return true })
	return err
}

func (b *Backend) SetMeta(ctx context.Context, id, key, value string) error {
	e, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil || e.meta[key] == value {
			return e, false
		}
		e.meta[key] = value
		return e, true
	})
	if err == nil && e == nil {
		return machines.ErrNotFound
	}
	return err
}

func (b *Backend) Deliver(ctx context.Context, id string, identity machines.Identity) error {
	e, node, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	if state, _ := e.state(); state != machines.StateRunning {
		return &machines.NotRunningError{State: state}
	}
	sandbox := e.row.ID
	file, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	r, err := node.get(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("read sandbox token: %w", err)
	}
	resp, err := node.exec(ctx, sandbox, r.Token, execRequest{Argv: writeIdentity, Env: map[string]string{"CB_STDIN": string(file)}, TimeoutSeconds: identityTimeoutSeconds})
	switch {
	case err != nil:
		return fmt.Errorf("write identity: %w", err)
	case resp.ExitCode != 0:
		return fmt.Errorf("write identity: exit %d: %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}

func (b *Backend) SetEgress(ctx context.Context, id, upstream string) error {
	e, node, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	var v *envVar
	if upstream != "" {
		v = &envVar{Value: upstream, Guest: new(false)}
	}
	if err = node.patchEnv(ctx, e.row.ID, map[string]*envVar{envUpstream: v}); err != nil {
		return fmt.Errorf("set egress upstream: %w", err)
	}
	_, err = b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil || e.routed == (upstream != "") {
			return e, false
		}
		e.routed = upstream != ""
		return e, true
	})
	return err
}

func (b *Backend) SetCredentials(ctx context.Context, id string, c machines.Credentials) error {
	e, node, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	if at := cmp.Or(e.ownerAddr, b.entryAddr); c.Node != "" && c.Node != at {
		return fmt.Errorf("machine %s is on %s, not on %s: %w", id, at, c.Node, machines.ErrConflict)
	}
	changes, sums, had := map[string]*envVar{}, map[string]string{}, map[string]string{}
	add := func(name string, v *envVar) {
		sum := fingerprint(b.opts.Token, v)
		have, ok := e.env[name]
		if (v == nil && !ok) || (v != nil && ok && have == sum && !e.row.EnvMissing) {
			return
		}
		changes[name], sums[name], had[name] = v, sum, have
	}
	for name, sec := range c.Secrets {
		add(name, secretVar(sec))
	}
	if c.Prune != "" {
		for name := range e.env {
			if _, ok := c.Secrets[name]; !ok && strings.HasPrefix(name, c.Prune) {
				add(name, nil)
			}
		}
	}
	if state, _ := e.state(); state == machines.StateRunning {
		for name, value := range c.Flags {
			var v *envVar
			if value != "" {
				v = &envVar{Value: value}
			}
			add(name, v)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	err = node.patchEnv(ctx, e.row.ID, changes)
	if err != nil && !errors.Is(err, machines.ErrRefused) {
		return fmt.Errorf("set credentials: %w", err)
	}
	// A refused write counts as written, so a pool that cannot inject is retried only when the values change.
	if _, merr := b.mutate(ctx, id, func(cur *entry) (*entry, bool) {
		if cur == nil {
			return cur, false
		}
		if cur.env == nil {
			cur.env = map[string]string{}
		}
		for name, sum := range sums {
			switch {
			case cur.env[name] != had[name]:
				// Another process wrote the name since this one read it, so the value there is unknown.
				cur.env[name] = ""
			case changes[name] == nil:
				delete(cur.env, name)
			default:
				cur.env[name] = sum
			}
		}
		return cur, true
	}); merr != nil {
		return fmt.Errorf("record credentials: %w", merr)
	}
	if err != nil {
		return fmt.Errorf("set credentials: %w", err)
	}
	return nil
}

func (b *Backend) DropMissing(ctx context.Context, id string, keep []string) ([]string, error) {
	e, node, err := b.sandboxOf(ctx, id)
	if err != nil {
		return nil, err
	}
	env, err := node.env(ctx, e.row.ID)
	if err != nil {
		return nil, fmt.Errorf("read env: %w", err)
	}
	drops := map[string]*envVar{}
	for name, v := range env {
		if !v.Missing {
			continue
		}
		if slices.Contains(keep, name) {
			return nil, fmt.Errorf("%s went missing again: %w", name, machines.ErrConflict)
		}
		drops[name] = nil
	}
	if len(drops) == 0 {
		return nil, nil
	}
	if err = node.patchEnv(ctx, e.row.ID, drops); err != nil {
		return nil, fmt.Errorf("drop missing env: %w", err)
	}
	_, err = b.mutate(ctx, id, func(cur *entry) (*entry, bool) {
		if cur == nil {
			return cur, false
		}
		for name := range drops {
			delete(cur.env, name)
		}
		_, upstream := drops[envUpstream]
		cur.routed = cur.routed && !upstream
		return cur, true
	})
	return slices.Sorted(maps.Keys(drops)), err
}

func (b *Backend) mutate(ctx context.Context, id string, fn func(e *entry) (*entry, bool)) (*entry, error) {
	// An op whose begin commits must reach run even when the caller left.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedTimeout)
	defer cancel()
	var kept *entry
	wrote := false
	rec, err := b.opts.Store.UpdateMachine(ctx, id, func(m *cluster.Machine, found bool) (bool, error) {
		var cur *entry
		if found {
			cur = loadEntry(*m)
		}
		next, changed := fn(cur)
		kept, wrote = next, false
		if next == nil || !changed {
			return false, nil
		}
		rec := next.record()
		if rec.Equal(*m) {
			return false, nil
		}
		*m, wrote = rec, true
		return true, nil
	})
	if err != nil || kept == nil {
		return nil, err
	}
	if wrote {
		b.publish(id, rec.Owner)
	}
	return loadEntry(rec), nil
}

// drop removes machine id while cond holds for its entry and reports whether it did.
func (b *Backend) drop(ctx context.Context, id string, cond func(e *entry) bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedTimeout)
	defer cancel()
	owner := ""
	removed, err := b.opts.Store.DeleteMachineIf(ctx, id, func(m cluster.Machine) bool {
		owner = m.Owner
		return cond(loadEntry(m))
	})
	if errors.Is(err, cluster.ErrNotFound) {
		return false, nil
	}
	if removed {
		b.publish(id, owner)
	}
	return removed, err
}

// run runs fn as op of machine id, begun at revision, off the caller's request, and ends the op when fn returns.
func (b *Backend) run(id, op string, revision int64, fn task) {
	go func() {
		ctx, cancel := context.WithTimeout(b.ctx, opTimeout)
		defer cancel()
		logger := log.WithFunc("sandboxd.Backend.run").WithField("machine", id)
		go b.renewOp(ctx, cancel, id, op, revision)
		err := fn(ctx)
		if err != nil {
			logger.Error(ctx, err, op)
		}
		opErr := ""
		switch {
		case b.ctx.Err() != nil:
			// The process stops; the claim row tells the next reader how far the op came.
		case errors.Is(err, machines.ErrCapacity):
			opErr = machines.ErrCapacity.Error()
		case err != nil:
			opErr = op + " failed"
		}
		if _, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
			if e == nil || e.op != op || e.revision != revision {
				return e, false
			}
			e.op, e.opErr, e.opUntil = "", opErr, time.Time{}
			return e, true
		}); err != nil {
			logger.Errorf(ctx, err, "end %s", op)
		}
	}()
}

// renewOp extends the lease of op on machine id every renewEvery until ctx ends or the op does, and stops the op through stop before a lease it failed to renew runs out.
func (b *Backend) renewOp(ctx context.Context, stop context.CancelFunc, id, op string, revision int64) {
	logger := log.WithFunc("sandboxd.Backend.renewOp").WithField("machine", id)
	until := time.Now().Add(b.leaseFor)
	tick := time.Tick(b.renewEvery)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		running, next := false, time.Now().Add(b.leaseFor)
		_, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
			if running = e != nil && e.op == op && e.revision == revision; !running {
				return e, false
			}
			e.opUntil = next
			return e, true
		})
		switch {
		case err == nil && !running:
			return
		case err == nil:
			until = next
		case !time.Now().Add(b.renewEvery).Before(until):
			logger.Errorf(ctx, err, "stop %s, whose lease runs out unrenewed", op)
			stop()
			return
		default:
			logger.Warnf(ctx, "renew the lease of %s: %v", op, err)
		}
	}
}

func (b *Backend) claim(ctx context.Context, spec machines.Spec, template, size string) error {
	metadata := map[string]string{metaMachine: spec.ID, metaOwner: spec.Owner}
	for k, v := range spec.Meta {
		metadata[metaClaimPrefix+k] = v
	}
	sandbox, node, err := b.claimFollow(ctx, spec.Tenant, claimRequest{
		Template:   template,
		Net:        netNone,
		Size:       size,
		TTLSeconds: leaseSeconds,
		OnExpire:   onExpire,
		Metadata:   metadata,
	})
	if err != nil {
		return err
	}
	if _, err := b.mutate(ctx, spec.ID, func(e *entry) (*entry, bool) {
		if e == nil || e.row != nil {
			return e, false
		}
		e.row, e.ownerAddr = &row{ID: sandbox, Key: poolKey{Template: template, Net: netNone, Size: size}, Metadata: metadata, ClaimedAt: e.created}, node
		return e, true
	}); err != nil {
		return err
	}
	return b.reread(ctx, spec.ID, node, node, sandbox)
}

// claimFollow claims on the entry node and follows its redirect as the sandboxd SDK does, and returns the claim and its node.
func (b *Backend) claimFollow(ctx context.Context, tenant string, req claimRequest) (string, string, error) {
	resp, err := b.api.claim(ctx, tenant, req)
	if err != nil || len(resp.Redirect) == 0 {
		return resp.ID, cmp.Or(resp.OwnerAddr, b.entryAddr), err
	}
	req.NoRedirect, req.RequirePromoted = true, resp.RequirePromoted
	var lastErr error
	for _, peer := range resp.Redirect {
		r, peerErr := b.claimAt(ctx, peer, tenant, req)
		if peerErr == nil {
			return r.ID, cmp.Or(r.OwnerAddr, peer), nil
		}
		lastErr = peerErr
	}
	if !transient(lastErr) {
		return "", "", fmt.Errorf("all redirect targets failed: %w", lastErr)
	}
	if resp, err = b.claimAt(ctx, b.entryAddr, tenant, req); err != nil {
		return "", "", fmt.Errorf("all redirect targets failed, the entry node too: %w", errors.Join(lastErr, err))
	}
	return resp.ID, cmp.Or(resp.OwnerAddr, b.entryAddr), nil
}

// claimAt claims on node addr with req, which must not be redirected.
func (b *Backend) claimAt(ctx context.Context, addr, tenant string, req claimRequest) (claimResponse, error) {
	node, err := b.node(addr)
	if err != nil {
		return claimResponse{}, err
	}
	resp, err := node.claim(ctx, tenant, req)
	if err == nil && len(resp.Redirect) > 0 {
		err = fmt.Errorf("%s redirected again despite no_redirect", node.base)
	}
	return resp, err
}

func (b *Backend) wake(ctx context.Context, id, from, sandbox string, wakes int) error {
	addr, err := b.atOwner(ctx, id, from, sandbox, func(node *client) error { return node.verb(ctx, sandbox, "wake", nil) })
	if err != nil {
		return err
	}
	if _, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil {
			return e, false
		}
		e.wakes, e.ready = wakes, false
		return e, true
	}); err != nil {
		return err
	}
	return b.reread(ctx, id, from, addr, sandbox)
}

// reread replaces machine id's row with the current one of sandbox on node addr, or on the node the mesh names its owner, while the machine has no row yet or is still on node from.
func (b *Backend) reread(ctx context.Context, id, from, addr, sandbox string) error {
	r, addr, err := b.read(ctx, id, addr, sandbox)
	if err != nil {
		return err
	}
	if r.ID != sandbox || r.Metadata[metaMachine] != id {
		return fmt.Errorf("node %s answered sandbox %s of machine %s with sandbox %s of machine %s", addr, sandbox, id, r.ID, r.Metadata[metaMachine])
	}
	token := r.Token
	r.Token = ""
	applied := false
	if _, err = b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if applied = e != nil && (e.row == nil || e.ownerAddr == from); !applied {
			return e, false
		}
		e.row, e.ownerAddr = &r, addr
		return e, true
	}); err != nil {
		return err
	}
	if applied {
		b.keepToken(id, sandbox, token)
	}
	return nil
}

// read returns the row of sandbox, machine id's claim, and the node that answered: addr, or the node the mesh names its owner.
func (b *Backend) read(ctx context.Context, id, addr, sandbox string) (row, string, error) {
	var r row
	addr, err := b.atOwner(ctx, id, addr, sandbox, func(node *client) error {
		var err error
		r, err = node.get(ctx, sandbox)
		return err
	})
	return r, addr, err
}

// atOwner runs fn on node addr and, when the failure says the claim may have moved, once more on the node the mesh names its owner; it returns the node fn last ran on.
func (b *Backend) atOwner(ctx context.Context, id, addr, sandbox string, fn func(*client) error) (string, error) {
	node, err := b.node(addr)
	if err == nil {
		err = fn(node)
	}
	if err == nil || !b.moved(addr, err) {
		return addr, err
	}
	meshCtx, cancel := context.WithTimeout(ctx, ownerTimeout)
	addrs := b.mesh(meshCtx)
	cancel()
	to, ok := b.follow(ctx, addrs, id, sandbox)
	if !ok {
		return addr, err
	}
	if node, err = b.node(to); err != nil {
		return addr, err
	}
	return to, fn(node)
}

// moved reports whether err from node addr may mean the claim lives on another node: the node does not hold it or has left the mesh.
func (b *Backend) moved(addr string, err error) bool {
	if apiErr, ok := errors.AsType[*apiError](err); ok && apiErr.status == http.StatusNotFound {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, away := b.away[addr]
	return away
}

// follow asks the nodes at addrs which one owns sandbox, machine id's claim, with the claim's token, which lets a node adopt an archived claim.
func (b *Backend) follow(ctx context.Context, addrs []string, id, sandbox string) (string, bool) {
	token := b.token(id, sandbox)
	if token == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, ownerTimeout)
	defer cancel()
	owners := make([]string, len(addrs))
	var g errgroup.Group
	g.SetLimit(probeFanout)
	for i, addr := range addrs {
		g.Go(func() error {
			node, err := b.node(addr)
			if err != nil {
				return nil
			}
			if owner, err := node.owner(ctx, sandbox, token); err == nil {
				owners[i] = cmp.Or(owner, addr)
			}
			return nil
		})
	}
	_ = g.Wait()
	logger := log.WithFunc("sandboxd.Backend.follow").WithField("machine", id)
	owners = slices.DeleteFunc(owners, func(owner string) bool { return owner == "" })
	slices.Sort(owners)
	owners = slices.Compact(owners)
	switch len(owners) {
	case 0:
		return "", false
	case 1:
		logger.Infof(ctx, "%s owns sandbox %s", owners[0], sandbox)
		return owners[0], true
	default:
		logger.Warnf(ctx, "nodes name different owners of sandbox %s: %s; the machine stays", sandbox, strings.Join(owners, ", "))
		return "", false
	}
}

// keepToken holds token for machine id while its claim is sandbox.
func (b *Backend) keepToken(id, sandbox, token string) {
	if token == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[id].sandbox != sandbox {
		b.seen[id] = seenClaim{sandbox: sandbox, token: token, kept: time.Now()}
		return
	}
	s := b.seen[id]
	s.token = token
	b.seen[id] = s
}

func (b *Backend) token(id, sandbox string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.seen[id]; s.sandbox == sandbox {
		return s.token
	}
	return ""
}

// lookup refreshes id's row from the node that holds its claim and returns the entry.
func (b *Backend) lookup(ctx context.Context, id string) (*entry, error) {
	e, err := b.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	sandbox := e.sandbox()
	if sandbox == "" {
		return e, nil
	}
	r, addr, err := b.read(ctx, id, e.ownerAddr, sandbox)
	if errors.Is(err, machines.ErrNotFound) {
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	known := map[string]*entry{id: e}
	if err := b.index(ctx, addr, []row{r}, known, e.ownerAddr); err != nil {
		return nil, err
	}
	if known[id] == nil {
		return nil, machines.ErrNotFound
	}
	return known[id], nil
}

func (b *Backend) stored(ctx context.Context, id string) (*entry, error) {
	rec, err := b.opts.Store.Machine(ctx, id)
	if errors.Is(err, cluster.ErrNotFound) {
		return nil, machines.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return loadEntry(rec), nil
}

func (b *Backend) machine(e *entry) machines.Machine {
	m := e.machine(b.opts.DiskGiB)
	b.mu.Lock()
	defer b.mu.Unlock()
	m.Node = cmp.Or(e.ownerAddr, b.entryAddr)
	m.Squeezed = b.full[m.Node]
	return m
}

func (b *Backend) changes(id string) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return waitOn(b.watchers, id)
}

func (b *Backend) publish(id, owner string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wakeOn(b.watchers, id)
	wakeOn(b.owners, owner)
}

func (b *Backend) publishAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, waits := range []map[string]chan struct{}{b.watchers, b.owners} {
		for _, ch := range waits {
			close(ch)
		}
		clear(waits)
	}
}

func (b *Backend) sandboxOf(ctx context.Context, id string) (*entry, *client, error) {
	e, err := b.lookup(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if e.row == nil {
		return nil, nil, machines.ErrNotFound
	}
	node, err := b.node(e.ownerAddr)
	return e, node, err
}

// refresh re-reads every claim of the mesh, drops machines whose claim is gone and probes agents not yet seen.
func (b *Backend) refresh(ctx context.Context) error {
	known, err := b.snapshot(ctx)
	if err != nil {
		return err
	}
	addrs := b.mesh(ctx)
	lists, err := b.listMesh(ctx, addrs)
	if err != nil {
		return err
	}
	b.trackAway(addrs, lists, known)
	for addr, rows := range lists {
		if err := b.index(ctx, addr, rows, known, ""); err != nil {
			return err
		}
	}
	if err := b.dropUnlisted(ctx, lists, known); err != nil {
		return err
	}
	now := time.Now()
	for id, e := range known {
		if e.op != "" && !e.busy(now) {
			b.endLapsed(ctx, id)
		}
	}
	var g errgroup.Group
	g.SetLimit(probeFanout)
	for _, e := range known {
		if state, _ := e.state(); state == machines.StateStarting && !e.busy(now) && e.row != nil {
			g.Go(func() error {
				b.probe(ctx, e)
				return nil
			})
		}
	}
	_ = g.Wait()
	return nil
}

func (b *Backend) endLapsed(ctx context.Context, id string) {
	if _, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil || e.op == "" || e.busy(time.Now()) {
			return e, false
		}
		e.op = ""
		return e, true
	}); err != nil {
		log.WithFunc("sandboxd.Backend.endLapsed").WithField("machine", id).Warnf(ctx, "end the lapsed op: %v", err)
	}
}

func (b *Backend) snapshot(ctx context.Context) (map[string]*entry, error) {
	read := time.Now()
	recs, err := b.opts.Store.Machines(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]*entry, len(recs))
	for _, rec := range recs {
		known[rec.ID] = loadEntry(rec)
	}
	b.forgetTokens(known, read)
	return known, nil
}

func (b *Backend) forgetTokens(known map[string]*entry, read time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	maps.DeleteFunc(b.seen, func(id string, s seenClaim) bool {
		_, ok := known[id]
		return !ok && s.kept.Before(read)
	})
}

// index applies the rows node addr lists to the machines of known whose claim they are; a row moves a machine to addr only from node from, where the backend itself followed the claim from, and only while the machine is still there.
func (b *Backend) index(ctx context.Context, addr string, rows []row, known map[string]*entry, from string) error {
	node, err := b.node(addr)
	if err != nil {
		return err
	}
	placed := func(e *entry, sandbox string) bool {
		return sandbox != "" && e.sandbox() == sandbox && (e.ownerAddr == addr || from != "" && e.ownerAddr == from)
	}
	for _, r := range rows {
		id := r.Metadata[metaMachine]
		e := known[id]
		if !placed(e, r.ID) {
			continue
		}
		token := r.Token
		r.Token = ""
		if token == "" && b.token(id, r.ID) == "" {
			if full, getErr := node.get(ctx, r.ID); getErr == nil {
				token = full.Token
			}
		}
		if reflect.DeepEqual(*e.row, r) && e.image == r.Key.Template && e.ownerAddr == addr {
			if token == "" || token == b.token(id, r.ID) {
				continue
			}
			if cur, readErr := b.stored(ctx, id); readErr == nil && cur.sandbox() == r.ID && cur.ownerAddr == addr {
				b.keepToken(id, r.ID, token)
			}
			continue
		}
		applied := false
		next, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
			if applied = placed(e, r.ID); !applied {
				return e, false
			}
			cur := r
			e.row, e.image, e.ownerAddr = &cur, r.Key.Template, addr
			return e, true
		})
		if err != nil {
			return err
		}
		if next == nil {
			delete(known, id)
			continue
		}
		known[id] = next
		if applied {
			b.keepToken(id, r.ID, token)
		}
	}
	return nil
}

// dropUnlisted drops the machines of known that their node's list lacks while it answered or stayed out of the mesh for nodeLostAfter, unless a node answers for the claim or lists it.
func (b *Backend) dropUnlisted(ctx context.Context, lists map[string][]row, known map[string]*entry) error {
	present, listed := map[placement]bool{}, map[string]bool{}
	for addr, rows := range lists {
		for _, r := range rows {
			present[placement{addr, r.ID}], listed[r.ID] = true, true
		}
	}
	now := time.Now()
	b.mu.Lock()
	missing := map[string]placement{}
	for id, e := range known {
		if e.busy(now) || e.row == nil || present[placement{e.ownerAddr, e.row.ID}] {
			continue
		}
		if _, answered := lists[e.ownerAddr]; answered || b.lostLocked(e.ownerAddr, now) {
			missing[id] = placement{e.ownerAddr, e.row.ID}
		}
	}
	b.mu.Unlock()
	for id, at := range b.unadopted(ctx, slices.Collect(maps.Keys(lists)), missing) {
		if listed[at.sandbox] {
			log.WithFunc("sandboxd.Backend.dropUnlisted").WithField("machine", id).Warnf(ctx, "another node lists sandbox %s, but no agreed owner took it; the machine stays", at.sandbox)
			continue
		}
		dropped, err := b.drop(ctx, id, func(e *entry) bool { return !e.busy(now) && e.sandbox() == at.sandbox && e.ownerAddr == at.addr })
		if err != nil {
			return err
		}
		if dropped {
			delete(known, id)
		}
	}
	return nil
}

// unadopted returns the machines of missing that no node at addrs answers for, and rereads the others on the node that does.
func (b *Backend) unadopted(ctx context.Context, addrs []string, missing map[string]placement) map[string]placement {
	var mu sync.Mutex
	out := map[string]placement{}
	var g errgroup.Group
	g.SetLimit(probeFanout)
	for id, at := range missing {
		g.Go(func() error {
			if to, ok := b.follow(ctx, addrs, id, at.sandbox); ok && b.reread(ctx, id, at.addr, to, at.sandbox) == nil {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			out[id] = at
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// listMesh lists the claims of every node at addrs, keyed by node; a node that fails is left out, and the list fails only when every node does.
func (b *Backend) listMesh(ctx context.Context, addrs []string) (map[string][]row, error) {
	logger := log.WithFunc("sandboxd.Backend.listMesh")
	lists := make([][]row, len(addrs))
	errs := make([]error, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Go(func() {
			node, err := b.node(addr)
			if err == nil {
				lists[i], err = node.sandboxes(ctx)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	out := make(map[string][]row, len(addrs))
	for i, addr := range addrs {
		if errs[i] != nil {
			logger.Warnf(ctx, "list the claims of %s: %v", addr, errs[i])
			continue
		}
		out[addr] = lists[i]
	}
	if len(out) == 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// readRoom asks every node of the mesh whether it has room for another running sandbox; machines of a full node are squeezed.
func (b *Backend) readRoom(ctx context.Context) error {
	addrs := b.mesh(ctx)
	reasons := make([]string, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Go(func() {
			if node, err := b.node(addr); err == nil {
				reasons[i], _ = node.full(ctx)
			}
		})
	}
	wg.Wait()
	full := map[string]bool{}
	for i, addr := range addrs {
		if reasons[i] != "" {
			full[addr] = true
		}
	}
	b.mu.Lock()
	was := b.full
	b.full = full
	b.mu.Unlock()
	logger := log.WithFunc("sandboxd.Backend.readRoom")
	for i, addr := range addrs {
		switch {
		case full[addr] && !was[addr]:
			logger.Infof(ctx, "%s is full (%s): its idle boxes hibernate sooner", addr, reasons[i])
		case !full[addr] && was[addr]:
			logger.Infof(ctx, "%s has room again", addr)
		}
	}
	return nil
}

// mesh returns the entry node and the peers it names that opts.Nodes allows, the peers it last named when it does not answer.
func (b *Backend) mesh(ctx context.Context) []string {
	peers, err := b.api.peers(ctx)
	b.nodeMu.Lock()
	defer b.nodeMu.Unlock()
	if err != nil {
		log.WithFunc("sandboxd.Backend.mesh").Warnf(ctx, "list the peers: %v", err)
		peers = b.peers
	}
	b.peers = slices.DeleteFunc(peers, func(addr string) bool { return !b.allowed(addr) })
	return slices.Concat([]string{b.entryAddr}, b.peers)
}

// trackAway notes when a node that the claims of known name stopped being in the mesh and answering the list, and forgets nodes no claim names.
func (b *Backend) trackAway(nodes []string, lists map[string][]row, known map[string]*entry) {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	named := map[string]bool{}
	for _, e := range known {
		if _, answered := lists[e.ownerAddr]; e.row == nil || answered || slices.Contains(nodes, e.ownerAddr) {
			continue
		}
		named[e.ownerAddr] = true
		if _, ok := b.away[e.ownerAddr]; !ok {
			b.away[e.ownerAddr] = now
		}
	}
	maps.DeleteFunc(b.away, func(addr string, _ time.Time) bool { return !named[addr] })
}

func (b *Backend) lostLocked(addr string, now time.Time) bool {
	since, ok := b.away[addr]
	return ok && now.Sub(since) >= nodeLostAfter
}

// node returns the client of the node at addr, the entry node's for its own address.
func (b *Backend) node(addr string) (*client, error) {
	if addr == b.entryAddr {
		return b.api, nil
	}
	if !b.allowed(addr) {
		return nil, fmt.Errorf("node %s is not in the sandboxd nodes", addr)
	}
	b.nodeMu.Lock()
	defer b.nodeMu.Unlock()
	if c := b.nodes[addr]; c != nil {
		return c, nil
	}
	c, err := newClient(nodeURL(addr), b.opts.Token)
	if err != nil {
		return nil, err
	}
	b.nodes[addr] = c
	return c, nil
}

// checkNodes refuses an entry node that advertises one of opts.Nodes and a stored machine on a node opts.Nodes leaves out.
func (b *Backend) checkNodes(ctx context.Context, advertised string) error {
	if len(b.opts.Nodes) == 0 {
		return nil
	}
	if slices.Contains(b.opts.Nodes, advertised) {
		return fmt.Errorf("the sandboxd entry node advertises %s, which nodes names as another node", advertised)
	}
	recs, err := b.opts.Store.Machines(ctx)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.OwnerAddr != "" && !b.allowed(rec.OwnerAddr) {
			return fmt.Errorf("machine %s is on node %s, which nodes leaves out", rec.ID, rec.OwnerAddr)
		}
	}
	return nil
}

func (b *Backend) allowed(addr string) bool {
	return addr == b.entryAddr || len(b.opts.Nodes) == 0 || slices.Contains(b.opts.Nodes, addr)
}

// probe marks e ready once its agent answers through the relay, and returns e as it then stands.
func (b *Backend) probe(ctx context.Context, e *entry) *entry {
	if e.row == nil || e.busy(time.Now()) || e.row.Hibernated || e.row.Archived || e.row.Failed != "" ||
		(e.ready && e.readyRestarts == e.row.Restarts) {
		return e
	}
	sandbox, restarts := e.row.ID, e.row.Restarts
	node, err := b.node(e.ownerAddr)
	if err != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := node.dialPort(ctx, sandbox, b.opts.AgentPort)
	if err != nil {
		return e
	}
	_ = conn.Close()
	next, err := b.mutate(ctx, e.id, func(e *entry) (*entry, bool) {
		if e == nil || e.row == nil || e.row.ID != sandbox || e.row.Restarts != restarts {
			return e, false
		}
		e.ready, e.readyRestarts = true, restarts
		return e, true
	})
	if err != nil {
		log.WithFunc("sandboxd.Backend.probe").WithField("machine", e.id).Warnf(ctx, "record the agent ready: %v", err)
	}
	return cmp.Or(next, e)
}

// renew extends the lease of every claim that is not archived, every node at once.
func (b *Backend) renew(ctx context.Context) error {
	known, err := b.snapshot(ctx)
	if err != nil {
		return err
	}
	leases := map[string][]string{}
	for _, e := range known {
		if e.row != nil && !e.row.Archived {
			leases[e.ownerAddr] = append(leases[e.ownerAddr], e.row.ID)
		}
	}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for addr, sandboxes := range leases {
		wg.Go(func() {
			if err := b.renewOn(ctx, addr, sandboxes); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (b *Backend) renewOn(ctx context.Context, addr string, sandboxes []string) error {
	node, err := b.node(addr)
	if err != nil {
		return err
	}
	var (
		mu     sync.Mutex
		failed []error
		g      errgroup.Group
	)
	g.SetLimit(renewFanout)
	for _, sandbox := range sandboxes {
		g.Go(func() error {
			callCtx, cancel := context.WithTimeout(ctx, renewCall)
			defer cancel()
			if err := node.verb(callCtx, sandbox, "renew", renewRequest{TTLSeconds: leaseSeconds, OnExpire: onExpire}); err != nil && !errors.Is(err, machines.ErrConflict) {
				mu.Lock()
				failed = append(failed, err)
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("renew failed for %d of %d leases on node %s: %w", len(failed), len(sandboxes), addr, failed[0])
}

func (b *Backend) loop(ctx context.Context, interval, timeout time.Duration, fn task) {
	tick := time.Tick(interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		if err := fn(callCtx); err != nil {
			log.WithFunc("sandboxd.Backend.loop").Warnf(callCtx, "every %s: %v", interval, err)
		}
		cancel()
	}
}

func send(ctx context.Context, out chan<- machines.Machine, m machines.Machine) bool {
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func secretVar(sec *machines.Secret) *envVar {
	if sec == nil || sec.Value == "" {
		return nil
	}
	v := &envVar{Value: sec.Value, Guest: new(false)}
	if len(sec.Hosts) > 0 {
		v.Inject = &inject{Hosts: sec.Hosts, Header: sec.Header, Query: sec.Query, Placeholder: sec.Placeholder}
	}
	return v
}

// fingerprint identifies an env entry by an HMAC under key, so a reader of the database alone cannot test guesses of its value; nil has none.
func fingerprint(key string, v *envVar) string {
	if v == nil {
		return ""
	}
	raw, _ := json.Marshal(v)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func waitOn(waits map[string]chan struct{}, key string) chan struct{} {
	ch := waits[key]
	if ch == nil {
		ch = make(chan struct{})
		waits[key] = ch
	}
	return ch
}

func wakeOn(waits map[string]chan struct{}, key string) {
	if ch := waits[key]; ch != nil {
		close(ch)
		delete(waits, key)
	}
}
