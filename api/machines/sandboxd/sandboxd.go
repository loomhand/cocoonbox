// Package sandboxd runs machines as claims of a cocoonbox sandboxd instance.
package sandboxd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"

	"github.com/loomhand/cocoonbox/api/cluster"
	"github.com/loomhand/cocoonbox/api/machines"
)

const (
	netNone                = "none"
	leaseSeconds           = 3600
	onExpire               = "archive"
	identityTimeoutSeconds = 30

	refreshInterval = 2 * time.Second
	renewInterval   = 5 * time.Minute
	watchInterval   = 500 * time.Millisecond
	opTimeout       = 10 * time.Minute
	probeTimeout    = 2 * time.Second
	probeFanout     = 8
	finishTimeout   = 10 * time.Second
	sharedTimeout   = 10 * time.Second
	// opLease bounds how long a shared op outlives its process, which renews it every opRenew while it runs.
	opLease = time.Minute
	opRenew = 20 * time.Second

	renewTimeout     = 4 * time.Minute
	renewCallTimeout = 10 * time.Second
	renewFanout      = 8
	capacityAttempts = 3
	capacityBackoff  = 5 * time.Second
	nodeLostAfter    = 5 * time.Minute
)

var (
	metaKey = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

	writeIdentity = []string{"sh", "-c", `set -e; umask 022; mkdir -p /run/cocoonbox-identity; chmod 0755 /run/cocoonbox-identity
t=$(mktemp /run/cocoonbox-identity/.identity.XXXXXX); printf '%s' "$CB_STDIN" > "$t"; chmod 0644 "$t"; mv -f "$t" /run/cocoonbox-identity/identity`}
)

// Options names the sandboxd instance and the claims the backend makes on it.
type Options struct {
	URL string
	// Token is the instance's root api token.
	Token string
	// Image is the default template.
	Image string
	// Flavors lists every flavor Ensure may request; each must be a sandboxd tier.
	Flavors []machines.Flavor
	// DiskGiB is the disk the node gives each claim.
	DiskGiB           uint32
	AgentPort         int
	TenantEgressClass string
	// Ledger, when set, receives a copy of every machine record for other api processes.
	Ledger Ledger
	// Shared, when set, holds the machine records: every change lands on a row's version and the backend caches the table.
	Shared Shared
	// CapacityAttempts bounds the claims of one machine while the mesh answers at capacity; 0 is capacityAttempts.
	CapacityAttempts int
}

// Ledger keeps the machine records api processes share.
type Ledger interface {
	Machines(ctx context.Context) ([]cluster.Machine, error)
	PutMachine(ctx context.Context, m cluster.Machine) error
	DeleteMachine(ctx context.Context, id string) error
}

// Shared is the machine table api processes share, with the notifies of its changes.
type Shared interface {
	Machines(ctx context.Context) ([]cluster.Machine, error)
	Machine(ctx context.Context, id string) (cluster.Machine, error)
	UpdateMachine(ctx context.Context, id string, edit func(m *cluster.Machine, found bool) (bool, error)) (cluster.Machine, error)
	DeleteMachineIf(ctx context.Context, id string, cond func(m cluster.Machine) bool) (bool, error)
	Listen(ctx context.Context, channel string, reset func(), fn func(payload string))
}

var (
	_ machines.Machines = (*Backend)(nil)
	_ machines.Tenants  = (*Backend)(nil)
)

// Backend implements machines.Machines and machines.Tenants on one sandboxd instance that holds only cocoonbox claims.
type Backend struct {
	*Tenants

	opts   Options
	api    *client
	ctx    context.Context
	mirror *cluster.Mirror
	// self is the entry node's address as its peers name it.
	self string
	// capacityPause is the first pause before a claim at capacity is retried.
	capacityPause time.Duration
	// lostAfter is how long a node that left the mesh and answers no list may stay away before its machines count as gone.
	lostAfter time.Duration

	nodeMu sync.Mutex
	nodes  map[string]*client
	// away maps a node that left the mesh to when it was first missed.
	away map[string]time.Time

	mu      sync.Mutex
	entries map[string]*entry
	owners  map[string]chan struct{}
	views   map[string]view
}

// New checks opts, reads the instance's claims and keeps them indexed until ctx ends.
func New(ctx context.Context, opts Options) (*Backend, error) {
	api, err := newClient(opts.URL, opts.Token)
	if err != nil {
		return nil, err
	}
	for _, f := range opts.Flavors {
		if _, ok := sizeOf(f); !ok {
			return nil, fmt.Errorf("flavor vcpus:%d memory_mib:%d is not a sandboxd tier", f.VCPUs, f.MemoryMiB)
		}
	}
	b := &Backend{
		Tenants:       &Tenants{api: api, egressClass: opts.TenantEgressClass},
		opts:          opts,
		api:           api,
		ctx:           ctx,
		self:          api.addr(),
		capacityPause: capacityBackoff,
		lostAfter:     nodeLostAfter,
		nodes:         map[string]*client{},
		away:          map[string]time.Time{},
		entries:       map[string]*entry{},
		owners:        map[string]chan struct{}{},
		views:         map[string]view{},
	}
	if advertised, err := api.advertise(ctx); err == nil && advertised != "" {
		b.self = nodeURL(advertised)
	}
	switch {
	case opts.Shared != nil:
		if err := b.reload(ctx); err != nil {
			return nil, fmt.Errorf("read the shared machines: %w", err)
		}
		go opts.Shared.Listen(ctx, cluster.ChannelMachines, func() { b.reloadLogged(ctx) }, func(id string) { b.notified(ctx, id) })
	case opts.Ledger != nil:
		b.mirror = cluster.NewMirror()
		go b.mirror.Run(ctx)
	}
	if err := b.refresh(ctx); err != nil {
		return nil, fmt.Errorf("read sandboxd claims: %w", err)
	}
	if opts.Shared == nil {
		go b.Run(ctx)
	}
	return b, nil
}

// Run re-reads the claims every refreshInterval and renews their leases every renewInterval until ctx ends; New runs it
// itself unless a shared table lets one api do it for all.
func (b *Backend) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { b.loop(ctx, refreshInterval, requestTimeout, b.refresh) })
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
	template := cmp.Or(spec.Image, b.opts.Image)
	action, sandbox := machines.ActionNone, ""
	m, err := b.mutate(ctx, spec.ID, func(e *entry) (*entry, bool) {
		now := time.Now()
		action = machines.ActionNone
		switch {
		case e == nil || (e.row == nil && !e.busy(now)):
			e = &entry{id: spec.ID, owner: spec.Owner, image: template, created: now, meta: map[string]string{}}
			e.begin(opClaim, now, b.lease())
			action = machines.ActionCreated
			return e, true
		case e.busy(now):
			return e, false
		}
		if state, _ := e.state(); state == machines.StateHibernated || state == machines.StateFailed {
			sandbox = e.row.ID
			e.begin(opWake, now, b.lease())
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
		b.run(spec.ID, opClaim, m.Revision, func(ctx context.Context) error { return b.claim(ctx, spec.ID, spec.Owner, spec.Tenant, template, size) })
	case machines.ActionWoken:
		b.run(spec.ID, opWake, m.Revision, func(ctx context.Context) error { return b.wake(ctx, spec.ID, sandbox) })
	}
	return m, action, nil
}

func (b *Backend) Get(ctx context.Context, id string) (machines.Machine, error) {
	return b.lookup(ctx, id)
}

func (b *Backend) List(_ context.Context, owner string) ([]machines.Machine, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []machines.Machine
	for _, e := range b.entries {
		if owner == "" || e.owner == owner {
			out = append(out, e.machine(b.opts.DiskGiB))
		}
	}
	slices.SortFunc(out, func(x, y machines.Machine) int { return cmp.Compare(x.ID, y.ID) })
	return out, nil
}

func (b *Backend) Hibernate(ctx context.Context, id string) (machines.Machine, error) {
	if _, err := b.lookup(ctx, id); err != nil {
		return machines.Machine{}, err
	}
	started, missing, sandbox := false, false, ""
	m, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		started, missing = false, e == nil || e.row == nil
		if missing {
			return e, false
		}
		now := time.Now()
		if state, _ := e.state(); state == machines.StateRunning || state == machines.StateStarting && !e.busy(now) {
			sandbox, started = e.row.ID, true
			e.begin(opHibernate, now, b.lease())
			return e, true
		}
		return e, false
	})
	switch {
	case err != nil:
		return machines.Machine{}, err
	case missing:
		return machines.Machine{}, machines.ErrNotFound
	}
	if started {
		b.run(id, opHibernate, m.Revision, func(ctx context.Context) error {
			if err := b.nodeOf(id).verb(ctx, sandbox, "hibernate", nil); err != nil {
				return err
			}
			return b.reread(ctx, id, sandbox)
		})
	}
	return m, nil
}

func (b *Backend) Watch(ctx context.Context, id string, revision int64) <-chan machines.Machine {
	out := make(chan machines.Machine)
	go func() {
		defer close(out)
		ticker := time.NewTicker(watchInterval)
		defer ticker.Stop()
		seen, last := false, ""
		for {
			changed := b.changes(id)
			m, err := b.lookup(ctx, id)
			switch {
			case errors.Is(err, machines.ErrNotFound) && seen:
				send(ctx, out, machines.Machine{ID: id, State: machines.StateGone})
				return
			case err == nil:
				seen = true
				b.probe(ctx, id)
				if m, err = b.cached(id); err == nil && m.Revision >= revision {
					if key := fmt.Sprintf("%d|%s|%s|%d|%v", m.State, m.Reason, m.Incarnation, m.Revision, m.Meta); key != last {
						if !send(ctx, out, m) {
							return
						}
						last = key
					}
				}
			}
			select {
			case <-ticker.C:
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
	ch := b.owners[owner]
	if ch == nil {
		ch = make(chan struct{})
		b.owners[owner] = ch
	}
	return ch
}

func (b *Backend) Address(_ context.Context, id string) (machines.Endpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[id]
	if e == nil {
		return machines.Endpoint{}, machines.ErrNotFound
	}
	if state, _ := e.state(); state != machines.StateRunning {
		return machines.Endpoint{}, &machines.NotRunningError{State: state}
	}
	sandbox, node := e.row.ID, b.node(e.ownerAddr)
	key := net.JoinHostPort(sandbox+".sandboxd", strconv.Itoa(b.opts.AgentPort))
	return machines.Endpoint{Key: key, Dial: func(ctx context.Context) (net.Conn, error) {
		return node.dialPort(ctx, sandbox, b.opts.AgentPort)
	}}, nil
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	now := time.Now()
	unclaimed, err := b.drop(ctx, id, func(e *entry) bool { return e.row == nil && !e.busy(now) })
	if err != nil {
		return err
	}
	sandbox, err := b.sandboxOf(ctx, id)
	if unclaimed && errors.Is(err, machines.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = b.nodeOf(id).verb(ctx, sandbox, "release", nil); err != nil {
		return err
	}
	_, err = b.drop(ctx, id, func(*entry) bool { return true })
	return err
}

func (b *Backend) SetMeta(ctx context.Context, id, key, value string) error {
	if !metaKey.MatchString(key) {
		return fmt.Errorf("meta key %q must match %s", key, metaKey)
	}
	sandbox, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	if err = b.nodeOf(id).patchEnv(ctx, sandbox, map[string]*envVar{envMetaPrefix + key: {Value: value}}); err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}
	if _, err = b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil || e.meta[key] == value {
			return e, false
		}
		e.meta[key] = value
		return e, true
	}); err != nil {
		return err
	}
	b.notify(id)
	return nil
}

func (b *Backend) Deliver(ctx context.Context, id string, identity machines.Identity, keys map[string]*machines.Secret) error {
	sandbox, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	m, err := b.cached(id)
	if err != nil {
		return err
	}
	if m.State != machines.StateRunning {
		return &machines.NotRunningError{State: m.State}
	}
	if len(keys) > 0 {
		vars := make(map[string]*envVar, len(keys))
		for name, sec := range keys {
			vars[name] = secretVar(sec)
		}
		if err = b.nodeOf(id).patchEnv(ctx, sandbox, vars); err != nil {
			return fmt.Errorf("install host keys: %w", err)
		}
	}
	file, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	node := b.nodeOf(id)
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
	sandbox, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	var v *envVar
	if upstream != "" {
		v = &envVar{Value: upstream, Guest: new(false)}
	}
	if err = b.nodeOf(id).patchEnv(ctx, sandbox, map[string]*envVar{envUpstream: v}); err != nil {
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
	sandbox, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	b.mu.Lock()
	e := b.entries[id]
	if e == nil {
		b.mu.Unlock()
		return machines.ErrNotFound
	}
	changes, sums, had := map[string]*envVar{}, map[string]string{}, map[string]string{}
	add := func(name string, v *envVar) {
		sum := fingerprint(v)
		have, ok := e.env[name]
		if (v == nil && !ok) || (v != nil && ok && have == sum) {
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
	b.mu.Unlock()
	if len(changes) == 0 {
		return nil
	}
	err = b.nodeOf(id).patchEnv(ctx, sandbox, changes)
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

// Verify returns the machines whose record differs from the table once queued writes landed, repairing the row or, shared, the cache.
func (b *Backend) Verify(ctx context.Context) ([]string, error) {
	if b.mirror == nil && b.opts.Shared == nil {
		return nil, nil
	}
	differ, err := b.differences(ctx, nil)
	if err != nil || len(differ) == 0 {
		return nil, err
	}
	if differ, err = b.differences(ctx, differ); err != nil {
		return nil, err
	}
	if b.opts.Shared != nil {
		for _, id := range differ {
			b.notified(ctx, id)
		}
		return differ, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range differ {
		if e := b.entries[id]; e != nil {
			e.mirrored = nil
			b.touchLocked(e)
		} else {
			b.dropLocked(id)
		}
	}
	return differ, nil
}

// mutate applies fn to machine id's entry, nil for none, under the lock in memory or on the shared row's version, rerun on a conflict.
func (b *Backend) mutate(ctx context.Context, id string, fn func(e *entry) (*entry, bool)) (machines.Machine, error) {
	if b.opts.Shared == nil {
		b.mu.Lock()
		defer b.mu.Unlock()
		sandbox := b.entries[id].sandbox()
		next, changed := fn(b.entries[id])
		if next == nil {
			return machines.Machine{}, nil
		}
		if changed {
			if next.sandbox() != sandbox {
				next.rowAt = time.Now()
			}
			b.entries[id] = next
			b.touchLocked(next)
		}
		return next.machine(b.opts.DiskGiB), nil
	}
	// A write that committed must reach the cache even when the caller left.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedTimeout)
	defer cancel()
	var kept *entry
	rec, err := b.opts.Shared.UpdateMachine(ctx, id, func(m *cluster.Machine, found bool) (bool, error) {
		var cur *entry
		if found {
			cur = &entry{id: id}
			cur.load(*m)
		}
		next, changed := fn(cur)
		kept = next
		if next == nil || !changed {
			return false, nil
		}
		version := m.Version
		*m = next.record()
		m.Version = version
		return true, nil
	})
	if err != nil {
		return machines.Machine{}, err
	}
	if kept == nil {
		return machines.Machine{}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.loadLocked(rec)
	b.publishLocked()
	return e.machine(b.opts.DiskGiB), nil
}

// drop removes machine id while cond holds for its entry and reports whether it did.
func (b *Backend) drop(ctx context.Context, id string, cond func(e *entry) bool) (bool, error) {
	if b.opts.Shared == nil {
		b.mu.Lock()
		defer b.mu.Unlock()
		if e := b.entries[id]; e != nil && cond(e) {
			b.dropLocked(id)
			return true, nil
		}
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedTimeout)
	defer cancel()
	removed, err := b.opts.Shared.DeleteMachineIf(ctx, id, func(m cluster.Machine) bool {
		e := &entry{id: id}
		e.load(m)
		return cond(e)
	})
	if err != nil && !errors.Is(err, cluster.ErrNotFound) || err == nil && !removed {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, cached := b.entries[id]
	delete(b.entries, id)
	b.publishLocked()
	return removed || cached, nil
}

// run runs fn as op of machine id, begun at revision, off the caller's request, and ends the op when fn returns.
func (b *Backend) run(id, op string, revision int64, fn func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(b.ctx, opTimeout)
		defer cancel()
		logger := log.WithFunc("sandboxd.Backend.run").WithField("machine", id)
		if b.opts.Shared != nil {
			go b.renewOp(ctx, id, op, revision)
		}
		err := fn(ctx)
		if err != nil {
			logger.Errorf(ctx, err, "%s", op)
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
		finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancelFinish()
		if _, err := b.mutate(finishCtx, id, func(e *entry) (*entry, bool) {
			if e == nil || e.op != op || e.revision != revision {
				return e, false
			}
			e.op, e.opErr, e.opUntil = "", opErr, time.Time{}
			return e, true
		}); err != nil {
			logger.Errorf(finishCtx, err, "end %s", op)
		}
		b.notify(id)
	}()
}

// renewOp extends the lease of op on machine id every opRenew until ctx ends or the op does.
func (b *Backend) renewOp(ctx context.Context, id, op string, revision int64) {
	ticker := time.NewTicker(opRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		running := false
		if _, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
			if running = e != nil && e.op == op && e.revision == revision; !running {
				return e, false
			}
			e.opUntil = time.Now().Add(opLease)
			return e, true
		}); err != nil {
			log.WithFunc("sandboxd.Backend.renewOp").WithField("machine", id).Warnf(ctx, "renew the lease of %s: %v", op, err)
		} else if !running {
			return
		}
	}
}

// lease is how long a new op holds its machine before its process must renew it; zero, for ever, in memory.
func (b *Backend) lease() time.Duration {
	if b.opts.Shared == nil {
		return 0
	}
	return opLease
}

func (b *Backend) claim(ctx context.Context, id, owner, tenant, template, size string) error {
	sandbox, node, err := b.claimRetried(ctx, tenant, claimRequest{
		Template:   template,
		Net:        netNone,
		Size:       size,
		TTLSeconds: leaseSeconds,
		OnExpire:   onExpire,
		Metadata:   map[string]string{metaMachine: id, metaOwner: owner},
	})
	if err != nil {
		return err
	}
	if _, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil {
			return e, false
		}
		e.ownerAddr = node
		return e, true
	}); err != nil {
		return err
	}
	return b.reread(ctx, id, sandbox)
}

// claimRetried claims with claimOnce and retries a mesh at capacity with a growing pause.
func (b *Backend) claimRetried(ctx context.Context, tenant string, req claimRequest) (string, string, error) {
	for attempt := 1; ; attempt++ {
		sandbox, node, err := b.claimOnce(ctx, tenant, req)
		if !errors.Is(err, machines.ErrCapacity) || attempt >= cmp.Or(b.opts.CapacityAttempts, capacityAttempts) {
			return sandbox, node, err
		}
		select {
		case <-ctx.Done():
			return "", "", err
		case <-time.After(b.capacityPause * time.Duration(attempt)):
		}
	}
}

// claimOnce claims on the entry node or on the peers it redirects to, and returns the claim and the node that holds it;
// when every peer fails, the entry node claims itself.
func (b *Backend) claimOnce(ctx context.Context, tenant string, req claimRequest) (string, string, error) {
	resp, err := b.api.claim(ctx, tenant, req)
	if err != nil {
		return "", "", err
	}
	if len(resp.Redirect) == 0 {
		return resp.ID, b.nodeAddr(cmp.Or(resp.OwnerAddr, b.self)), nil
	}
	req.NoRedirect, req.RequirePromoted = true, resp.RequirePromoted
	var errs []error
	for _, peer := range resp.Redirect {
		r, peerErr := b.node(peer).claim(ctx, tenant, req)
		if peerErr == nil && len(r.Redirect) == 0 {
			return r.ID, b.nodeAddr(cmp.Or(r.OwnerAddr, peer)), nil
		}
		errs = append(errs, cmp.Or(peerErr, fmt.Errorf("%s redirected again", peer)))
	}
	if resp, err = b.api.claim(ctx, tenant, req); err != nil {
		return "", "", errors.Join(append(errs, err)...)
	}
	return resp.ID, b.nodeAddr(cmp.Or(resp.OwnerAddr, b.self)), nil
}

func (b *Backend) wake(ctx context.Context, id, sandbox string) error {
	if err := b.nodeOf(id).verb(ctx, sandbox, "wake", nil); err != nil {
		return err
	}
	b.mu.Lock()
	wakes := 1
	if e := b.entries[id]; e != nil {
		wakes = e.wakes + 1
	}
	b.mu.Unlock()
	if err := b.nodeOf(id).patchEnv(ctx, sandbox, map[string]*envVar{envWakes: {Value: strconv.Itoa(wakes)}}); err != nil {
		return fmt.Errorf("record wake: %w", err)
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
	return b.reread(ctx, id, sandbox)
}

// reread replaces machine id's row with sandbox's current one.
func (b *Backend) reread(ctx context.Context, id, sandbox string) error {
	r, err := b.nodeOf(id).get(ctx, sandbox)
	if err != nil {
		return err
	}
	r.Token = ""
	_, err = b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil {
			return e, false
		}
		e.row = &r
		return e, true
	})
	return err
}

// lookup refreshes id's row from sandboxd, finding it by metadata when the index lacks it.
func (b *Backend) lookup(ctx context.Context, id string) (machines.Machine, error) {
	b.mu.Lock()
	e := b.entries[id]
	var sandbox string
	if e != nil && e.row != nil {
		sandbox = e.row.ID
	}
	b.mu.Unlock()
	if e != nil && sandbox == "" {
		return b.cached(id)
	}
	var r row
	var err error
	if sandbox != "" {
		node := b.nodeOf(id)
		r, err = node.get(ctx, sandbox)
		r.node = b.nodeAddr(node.addr())
	} else {
		r, err = b.find(ctx, id)
	}
	if errors.Is(err, machines.ErrNotFound) {
		if err = b.forget(ctx, id, sandbox); err != nil {
			return machines.Machine{}, err
		}
		return machines.Machine{}, machines.ErrNotFound
	}
	if err != nil {
		return machines.Machine{}, err
	}
	r.Token = ""
	if err := b.index(ctx, []row{r}); err != nil {
		return machines.Machine{}, err
	}
	return b.cached(id)
}

func (b *Backend) cached(id string) (machines.Machine, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[id]
	if e == nil {
		return machines.Machine{}, machines.ErrNotFound
	}
	return e.machine(b.opts.DiskGiB), nil
}

func (b *Backend) notify(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[id]; e != nil {
		e.notifyLocked()
	}
}

func (b *Backend) changes(id string) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[id]
	if e == nil {
		return nil
	}
	if e.changed == nil {
		e.changed = make(chan struct{})
	}
	return e.changed
}

func (b *Backend) sandboxOf(ctx context.Context, id string) (string, error) {
	if _, err := b.lookup(ctx, id); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[id]; e != nil && e.row != nil {
		return e.row.ID, nil
	}
	return "", machines.ErrNotFound
}

// forget drops id unless an operation still owns it.
func (b *Backend) forget(ctx context.Context, id, sandbox string) error {
	now := time.Now()
	_, err := b.drop(ctx, id, func(e *entry) bool { return !e.busy(now) && (e.row == nil || e.row.ID == sandbox) })
	return err
}

// refresh re-reads every claim, drops machines whose claim is gone and probes agents not yet seen.
func (b *Backend) refresh(ctx context.Context) error {
	listed := time.Now()
	rows, answered, err := b.listMesh(ctx)
	if err != nil {
		return err
	}
	if err := b.index(ctx, rows); err != nil {
		return err
	}
	if err := b.dropUnlisted(ctx, rows, listed, answered); err != nil {
		return err
	}
	now := time.Now()
	b.mu.Lock()
	var unready []string
	for id, e := range b.entries {
		if state, _ := e.state(); state == machines.StateStarting && !e.busy(now) && e.row != nil {
			unready = append(unready, id)
		}
	}
	b.mu.Unlock()
	var g errgroup.Group
	g.SetLimit(probeFanout)
	for _, id := range unready {
		g.Go(func() error {
			b.probe(ctx, id)
			return nil
		})
	}
	_ = g.Wait()
	b.mu.Lock()
	b.publishLocked()
	b.mu.Unlock()
	return nil
}

// index merges rows into the entries, reading the env of claims it has not seen.
func (b *Backend) index(ctx context.Context, rows []row) error {
	for _, r := range rows {
		id := r.Metadata[metaMachine]
		if id == "" {
			continue
		}
		b.mu.Lock()
		e := b.entries[id]
		known := e != nil && e.row != nil && e.row.ID == r.ID
		unchanged := known && sameRow(*e.row, r) && e.owner == cmp.Or(r.Tenant, e.owner) && e.image == r.Key.Template &&
			(r.node == "" || e.ownerAddr == r.node)
		b.mu.Unlock()
		if unchanged {
			continue
		}
		var env map[string]envVar
		if !known {
			var err error
			if env, err = b.node(r.node).env(ctx, r.ID); err != nil {
				return fmt.Errorf("read env of %s: %w", r.ID, err)
			}
		}
		if _, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
			e = cmp.Or(e, &entry{id: id, owner: r.Metadata[metaOwner]})
			// The tenant stamp is sandboxd's own, so it outranks the owner metadata the api wrote.
			e.owner = cmp.Or(r.Tenant, e.owner)
			if env != nil && (e.row == nil || e.row.ID != r.ID) {
				e.loadEnv(env)
			}
			cur := r
			e.row, e.image = &cur, r.Key.Template
			if r.node != "" {
				e.ownerAddr = r.node
			}
			return e, true
		}); err != nil {
			return err
		}
	}
	return nil
}

// dropUnlisted drops the machines whose node answered the list taken at listed without them, and those of a node that
// left the mesh and answered no list for b.lostAfter; a machine whose row this process saw after the list stays.
func (b *Backend) dropUnlisted(ctx context.Context, rows []row, listed time.Time, answered map[string]bool) error {
	present := map[string]bool{}
	for _, r := range rows {
		present[r.Metadata[metaMachine]] = true
	}
	now := time.Now()
	b.mu.Lock()
	gone := map[string]string{}
	for id, e := range b.entries {
		if present[id] || e.busy(now) || e.row == nil || !e.rowAt.Before(listed) {
			continue
		}
		if node := b.nodeAddr(e.ownerAddr); answered[node] || b.lostLocked(node, now) {
			gone[id] = e.row.ID
		}
	}
	b.mu.Unlock()
	for id, sandbox := range gone {
		if _, err := b.drop(ctx, id, func(e *entry) bool { return !e.busy(now) && e.row != nil && e.row.ID == sandbox }); err != nil {
			return err
		}
	}
	return nil
}

// listMesh lists the claims of the entry node and of every peer, each row naming its node, and returns the nodes that
// answered; a failed entry node fails the list, a failed peer only leaves its rows out.
func (b *Backend) listMesh(ctx context.Context) ([]row, map[string]bool, error) {
	peers, err := b.api.peers(ctx)
	if err != nil {
		log.WithFunc("sandboxd.Backend.listMesh").Warnf(ctx, "list the peers of %s: %v", b.self, err)
	}
	nodes := []*client{b.api}
	for _, peer := range peers {
		if node := b.node(peer); node != b.api {
			nodes = append(nodes, node)
		}
	}
	lists := make([][]row, len(nodes))
	errs := make([]error, len(nodes))
	var g errgroup.Group
	for i, node := range nodes {
		g.Go(func() error {
			lists[i], errs[i] = node.list(ctx)
			return nil
		})
	}
	_ = g.Wait()
	if errs[0] != nil {
		return nil, nil, errs[0]
	}
	answered := map[string]bool{}
	var rows []row
	for i, node := range nodes {
		addr := b.nodeAddr(node.addr())
		if errs[i] != nil {
			log.WithFunc("sandboxd.Backend.listMesh").Warnf(ctx, "list the claims of %s: %v", addr, errs[i])
			continue
		}
		answered[addr] = true
		for _, r := range lists[i] {
			r.node = addr
			rows = append(rows, r)
		}
	}
	b.trackAway(peers, answered)
	return rows, answered, nil
}

// find looks machine id up on the entry node, then on every peer.
func (b *Backend) find(ctx context.Context, id string) (row, error) {
	rows, err := b.api.find(ctx, metaMachine, id)
	if err != nil {
		return row{}, err
	}
	if len(rows) > 0 {
		rows[0].node = b.self
		return rows[0], nil
	}
	peers, err := b.api.peers(ctx)
	if err != nil {
		return row{}, err
	}
	for _, peer := range peers {
		node := b.node(peer)
		if rows, err = node.find(ctx, metaMachine, id); err == nil && len(rows) > 0 {
			rows[0].node = b.nodeAddr(node.addr())
			return rows[0], nil
		}
	}
	return row{}, machines.ErrNotFound
}

// node returns the client of the node at addr, the entry node's for "" or its own address.
func (b *Backend) node(addr string) *client {
	addr = b.nodeAddr(addr)
	if addr == b.self {
		return b.api
	}
	b.nodeMu.Lock()
	defer b.nodeMu.Unlock()
	if c := b.nodes[addr]; c != nil {
		return c
	}
	c, err := newClient(addr, b.opts.Token)
	if err != nil {
		return b.api
	}
	b.nodes[addr] = c
	return c
}

// nodeOf returns the client of the node that holds machine id.
func (b *Backend) nodeOf(id string) *client {
	b.mu.Lock()
	var addr string
	if e := b.entries[id]; e != nil {
		addr = e.ownerAddr
	}
	b.mu.Unlock()
	return b.node(addr)
}

// nodeAddr names the node at addr as the backend keeps it, the entry node for "".
func (b *Backend) nodeAddr(addr string) string {
	if addr == "" || nodeURL(addr) == b.api.addr() {
		return b.self
	}
	return nodeURL(addr)
}

// trackAway notes when a node that machines name stopped being a peer and answering the list.
func (b *Backend) trackAway(peers []string, answered map[string]bool) {
	member := map[string]bool{b.self: true}
	for _, peer := range peers {
		member[b.nodeAddr(peer)] = true
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.entries {
		node := b.nodeAddr(e.ownerAddr)
		if _, ok := b.away[node]; !ok && !member[node] && !answered[node] {
			b.away[node] = now
		}
	}
	maps.DeleteFunc(b.away, func(node string, _ time.Time) bool { return member[node] || answered[node] })
}

// lostLocked reports whether node has been away from the mesh for lostAfter.
func (b *Backend) lostLocked(node string, now time.Time) bool {
	since, ok := b.away[node]
	return ok && now.Sub(since) >= b.lostAfter
}

// publishLocked wakes the owners of the machines whose state, image, flavor, incarnation or meta changed since the last call, or that went away.
func (b *Backend) publishLocked() {
	views := make(map[string]view, len(b.entries))
	for id, e := range b.entries {
		m := e.machine(b.opts.DiskGiB)
		views[id] = view{owner: m.Owner, key: fmt.Sprintf("%d|%s|%s|%v|%s|%v", m.State, m.Reason, m.Image, m.Flavor, m.Incarnation, m.Meta)}
		if b.views[id] != views[id] {
			b.wakeLocked(m.Owner)
		}
	}
	for id, v := range b.views {
		if _, ok := views[id]; !ok {
			b.wakeLocked(v.owner)
		}
	}
	b.views = views
}

func (b *Backend) wakeLocked(owner string) {
	if ch := b.owners[owner]; ch != nil {
		close(ch)
		delete(b.owners, owner)
	}
}

// probe marks id ready once its agent answers through the relay.
func (b *Backend) probe(ctx context.Context, id string) {
	b.mu.Lock()
	e := b.entries[id]
	if e == nil || e.row == nil || e.busy(time.Now()) || e.row.Hibernated || e.row.Archived || e.row.Failed != "" ||
		(e.ready && e.readyRestarts == e.row.Restarts) {
		b.mu.Unlock()
		return
	}
	sandbox, restarts := e.row.ID, e.row.Restarts
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := b.nodeOf(id).dialPort(ctx, sandbox, b.opts.AgentPort)
	if err != nil {
		return
	}
	_ = conn.Close()
	if _, err := b.mutate(ctx, id, func(e *entry) (*entry, bool) {
		if e == nil || e.row == nil || e.row.ID != sandbox || e.row.Restarts != restarts {
			return e, false
		}
		e.ready, e.readyRestarts = true, restarts
		return e, true
	}); err != nil {
		log.WithFunc("sandboxd.Backend.probe").WithField("machine", id).Warnf(ctx, "record the agent ready: %v", err)
	}
}

// renew extends the lease of every claim that is not archived, on its node, a few at a time and each with its own deadline.
func (b *Backend) renew(ctx context.Context) error {
	b.mu.Lock()
	leases := map[string]string{}
	for _, e := range b.entries {
		if e.row != nil && !e.row.Archived {
			leases[e.row.ID] = e.ownerAddr
		}
	}
	b.mu.Unlock()
	var mu sync.Mutex
	var errs []error
	var g errgroup.Group
	g.SetLimit(renewFanout)
	for sandbox, node := range leases {
		g.Go(func() error {
			callCtx, cancel := context.WithTimeout(ctx, renewCallTimeout)
			defer cancel()
			if err := b.node(node).verb(callCtx, sandbox, "renew", renewRequest{TTLSeconds: leaseSeconds, OnExpire: onExpire}); err != nil && !isStatus(err, http.StatusConflict) {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return errors.Join(errs...)
}

func (b *Backend) reload(ctx context.Context) error {
	recs, err := b.opts.Shared.Machines(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	present := make(map[string]bool, len(recs))
	for _, rec := range recs {
		present[rec.ID] = true
		b.loadLocked(rec)
	}
	maps.DeleteFunc(b.entries, func(id string, _ *entry) bool { return !present[id] })
	b.publishLocked()
	return nil
}

func (b *Backend) reloadLogged(ctx context.Context) {
	if err := b.reload(ctx); err != nil {
		log.WithFunc("sandboxd.Backend.reloadLogged").Warnf(ctx, "reload the shared machines: %v", err)
	}
}

// notified takes the shared row of machine id into the cache after a notify.
func (b *Backend) notified(ctx context.Context, id string) {
	rec, err := b.opts.Shared.Machine(ctx, id)
	if err != nil && !errors.Is(err, cluster.ErrNotFound) {
		log.WithFunc("sandboxd.Backend.notified").WithField("machine", id).Warnf(ctx, "read the shared machine: %v", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		delete(b.entries, id)
	} else {
		b.loadLocked(rec)
	}
	b.publishLocked()
}

// loadLocked takes rec into the cache unless the cache holds a newer version, and returns the cached entry.
func (b *Backend) loadLocked(rec cluster.Machine) *entry {
	e := b.entries[rec.ID]
	if e == nil {
		e = &entry{id: rec.ID}
		b.entries[rec.ID] = e
	}
	if rec.Version >= e.version {
		busy, opErr, meta, sandbox := e.op != "", e.opErr, e.meta, e.sandbox()
		e.load(rec)
		if e.sandbox() != sandbox {
			e.rowAt = time.Now()
		}
		// Like a change in memory, only an op end, a failure or a meta change wakes the watchers before their next poll.
		if (busy && e.op == "") || opErr != e.opErr || !maps.Equal(meta, e.meta) {
			e.notifyLocked()
		}
	}
	return e
}

// touchLocked queues e's record for the ledger when it changed since the last one queued.
func (b *Backend) touchLocked(e *entry) {
	if b.mirror == nil || b.entries[e.id] != e {
		return
	}
	rec := e.record()
	if e.mirrored != nil && e.mirrored.Equal(rec) {
		return
	}
	e.mirrored = &rec
	b.mirror.Put(e.id, func(ctx context.Context) error { return b.opts.Ledger.PutMachine(ctx, rec) })
}

// dropLocked forgets machine id and queues its removal from the ledger.
func (b *Backend) dropLocked(id string) {
	delete(b.entries, id)
	if b.mirror != nil {
		b.mirror.Put(id, func(ctx context.Context) error { return b.opts.Ledger.DeleteMachine(ctx, id) })
	}
}

// differences flushes the queued writes and returns the ids, of only, or of all when only is nil, whose table row differs from the entry.
func (b *Backend) differences(ctx context.Context, only []string) ([]string, error) {
	var table func(context.Context) ([]cluster.Machine, error)
	if b.mirror != nil {
		if err := b.mirror.Flush(ctx); err != nil {
			return nil, err
		}
		table = b.opts.Ledger.Machines
	} else {
		table = b.opts.Shared.Machines
	}
	b.mu.Lock()
	want := make(map[string]cluster.Machine, len(b.entries))
	for id, e := range b.entries {
		want[id] = e.record()
	}
	b.mu.Unlock()
	rows, err := table(ctx)
	if err != nil {
		return nil, err
	}
	have := make(map[string]cluster.Machine, len(rows))
	for _, r := range rows {
		have[r.ID] = r
	}
	ids := only
	if ids == nil {
		ids = slices.Collect(maps.Keys(want))
		for id := range have {
			if _, ok := want[id]; !ok {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
	}
	var differ []string
	for _, id := range ids {
		w, inMemory := want[id]
		h, inTable := have[id]
		if inMemory != inTable || (inMemory && !w.Equal(h)) {
			differ = append(differ, id)
		}
	}
	return differ, nil
}

func (b *Backend) loop(ctx context.Context, interval, timeout time.Duration, fn func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
	if sec == nil {
		return nil
	}
	v := &envVar{Value: sec.Value, Guest: new(false)}
	if len(sec.Hosts) > 0 {
		v.Inject = &inject{Hosts: sec.Hosts, Header: sec.Header, Query: sec.Query, Placeholder: sec.Placeholder}
	}
	return v
}

// fingerprint identifies an env entry without keeping its value; nil has none.
func fingerprint(v *envVar) string {
	if v == nil {
		return ""
	}
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func envNames(env map[string]envVar) map[string]string {
	names := make(map[string]string, len(env))
	for name := range env {
		names[name] = ""
	}
	return names
}

func sameRow(a, b row) bool {
	return a.ID == b.ID && a.Key == b.Key && a.Tenant == b.Tenant && maps.Equal(a.Metadata, b.Metadata) && a.Hibernated == b.Hibernated &&
		a.Archived == b.Archived && a.Restarts == b.Restarts && a.Failed == b.Failed && a.ClaimedAt.Equal(b.ClaimedAt)
}
