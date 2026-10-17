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
	"reflect"
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
	Ledger            *cluster.Store
}

// Check reports whether New refuses opts before it reads the instance.
func (o Options) Check() error {
	_, err := o.client()
	return err
}

func (o Options) client() (*client, error) {
	for _, f := range o.Flavors {
		if _, ok := sizeOf(f); !ok {
			return nil, fmt.Errorf("flavor vcpus:%d memory_mib:%d is not a sandboxd tier", f.VCPUs, f.MemoryMiB)
		}
	}
	return newClient(o.URL, o.Token)
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

	mu      sync.Mutex
	entries map[string]*entry
	owners  map[string]chan struct{}
	views   map[string]view
}

// New checks opts, reads the instance's claims and keeps them indexed until ctx ends.
func New(ctx context.Context, opts Options) (*Backend, error) {
	api, err := opts.client()
	if err != nil {
		return nil, err
	}
	b := &Backend{
		Tenants: &Tenants{api: api, egressClass: opts.TenantEgressClass},
		opts:    opts,
		api:     api,
		ctx:     ctx,
		entries: map[string]*entry{},
		owners:  map[string]chan struct{}{},
		views:   map[string]view{},
	}
	if opts.Ledger != nil {
		b.mirror = cluster.NewMirror()
		go b.mirror.Run(ctx)
	}
	if err := b.refresh(ctx); err != nil {
		return nil, fmt.Errorf("read sandboxd claims: %w", err)
	}
	go b.loop(refreshInterval, b.refresh)
	go b.loop(renewInterval, b.renew)
	return b, nil
}

func (b *Backend) Ensure(ctx context.Context, spec machines.Spec) (machines.Machine, machines.Action, error) {
	size, ok := sizeOf(spec.Flavor)
	if !ok {
		return machines.Machine{}, machines.ActionNone, fmt.Errorf("flavor vcpus:%d memory_mib:%d is not a sandboxd tier", spec.Flavor.VCPUs, spec.Flavor.MemoryMiB)
	}
	if _, err := b.lookup(ctx, spec.ID); err != nil && !errors.Is(err, machines.ErrNotFound) {
		return machines.Machine{}, machines.ActionNone, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[spec.ID]
	switch {
	case e == nil || (e.row == nil && e.op == ""):
		template := cmp.Or(spec.Image, b.opts.Image)
		e = &entry{id: spec.ID, owner: spec.Owner, image: template, created: time.Now(), meta: map[string]string{}}
		b.entries[spec.ID] = e
		b.startLocked(e, opClaim, func(ctx context.Context) error { return b.claim(ctx, e, spec.Tenant, template, size) })
		return e.machine(b.opts.DiskGiB), machines.ActionCreated, nil
	case e.op != "":
		return e.machine(b.opts.DiskGiB), machines.ActionNone, nil
	}
	if state, _ := e.state(); state == machines.StateHibernated || state == machines.StateFailed {
		sandbox := e.row.ID
		b.startLocked(e, opWake, func(ctx context.Context) error { return b.wake(ctx, e, sandbox) })
		return e.machine(b.opts.DiskGiB), machines.ActionWoken, nil
	}
	return e.machine(b.opts.DiskGiB), machines.ActionNone, nil
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
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[id]
	if e == nil || e.row == nil {
		return machines.Machine{}, machines.ErrNotFound
	}
	if state, _ := e.state(); state == machines.StateRunning || state == machines.StateStarting && e.op == "" {
		sandbox := e.row.ID
		b.startLocked(e, opHibernate, func(ctx context.Context) error {
			if err := b.api.verb(ctx, sandbox, "hibernate", nil); err != nil {
				return err
			}
			return b.reread(ctx, e, sandbox)
		})
	}
	return e.machine(b.opts.DiskGiB), nil
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
	sandbox := e.row.ID
	key := net.JoinHostPort(sandbox+".sandboxd", strconv.Itoa(b.opts.AgentPort))
	return machines.Endpoint{Key: key, Dial: func(ctx context.Context) (net.Conn, error) {
		return b.api.dialPort(ctx, sandbox, b.opts.AgentPort)
	}}, nil
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	b.mu.Lock()
	e := b.entries[id]
	unclaimed := e != nil && e.row == nil && e.op == ""
	if unclaimed {
		b.dropLocked(id)
	}
	b.mu.Unlock()
	sandbox, err := b.sandboxOf(ctx, id)
	if unclaimed && errors.Is(err, machines.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := b.api.verb(ctx, sandbox, "release", nil); err != nil {
		return err
	}
	b.mu.Lock()
	b.dropLocked(id)
	b.mu.Unlock()
	return nil
}

func (b *Backend) SetMeta(ctx context.Context, id, key, value string) error {
	if !metaKey.MatchString(key) {
		return fmt.Errorf("meta key %q must match %s", key, metaKey)
	}
	sandbox, err := b.sandboxOf(ctx, id)
	if err != nil {
		return err
	}
	if err := b.api.patchEnv(ctx, sandbox, map[string]*envVar{envMetaPrefix + key: {Value: value}}); err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[id]; e != nil {
		e.meta[key] = value
		e.notifyLocked()
		b.touchLocked(e)
	}
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
		if err = b.api.patchEnv(ctx, sandbox, vars); err != nil {
			return fmt.Errorf("install host keys: %w", err)
		}
	}
	file, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	r, err := b.api.get(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("read sandbox token: %w", err)
	}
	resp, err := b.api.exec(ctx, sandbox, r.Token, execRequest{Argv: writeIdentity, Env: map[string]string{"CB_STDIN": string(file)}, TimeoutSeconds: identityTimeoutSeconds})
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
	if err := b.api.patchEnv(ctx, sandbox, map[string]*envVar{envUpstream: v}); err != nil {
		return fmt.Errorf("set egress upstream: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[id]; e != nil {
		e.routed = upstream != ""
		b.touchLocked(e)
	}
	return nil
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
	changes, sums := map[string]*envVar{}, map[string]string{}
	add := func(name string, v *envVar) {
		sum := fingerprint(v)
		if have, ok := e.env[name]; (v == nil && !ok) || (v != nil && ok && have == sum) {
			return
		}
		changes[name], sums[name] = v, sum
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
	err = b.api.patchEnv(ctx, sandbox, changes)
	if err != nil && !errors.Is(err, machines.ErrRefused) {
		return fmt.Errorf("set credentials: %w", err)
	}
	// A refused write counts as written, so a pool that cannot inject is retried only when the values change.
	b.mu.Lock()
	if e.env == nil {
		e.env = map[string]string{}
	}
	for name, sum := range sums {
		if changes[name] == nil {
			delete(e.env, name)
		} else {
			e.env[name] = sum
		}
	}
	b.mu.Unlock()
	if err != nil {
		return fmt.Errorf("set credentials: %w", err)
	}
	return nil
}

// Verify rewrites the ledger rows that still differ from the machines once the queued writes landed, and returns their ids.
func (b *Backend) Verify(ctx context.Context) ([]string, error) {
	differ, err := b.mirror.Verify(ctx, b.differences)
	if err != nil {
		return nil, err
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

// startLocked marks e as running op and runs fn off the caller's request.
func (b *Backend) startLocked(e *entry, op string, fn func(context.Context) error) {
	e.op, e.opErr = op, ""
	e.revision++
	b.touchLocked(e)
	go func() {
		ctx, cancel := context.WithTimeout(b.ctx, opTimeout)
		defer cancel()
		err := fn(ctx)
		if err != nil {
			log.WithFunc("sandboxd.Backend.startLocked").WithField("machine", e.id).Errorf(ctx, err, "%s", op)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		e.op = ""
		e.notifyLocked()
		switch {
		case errors.Is(err, machines.ErrCapacity):
			e.opErr = machines.ErrCapacity.Error()
		case err != nil:
			e.opErr = op + " failed"
		}
		b.touchLocked(e)
	}()
}

func (b *Backend) claim(ctx context.Context, e *entry, tenant, template, size string) error {
	sandbox, err := b.api.claim(ctx, tenant, claimRequest{
		Template:   template,
		Net:        netNone,
		Size:       size,
		TTLSeconds: leaseSeconds,
		OnExpire:   onExpire,
		Metadata:   map[string]string{metaMachine: e.id, metaOwner: e.owner},
	})
	if err != nil {
		return err
	}
	return b.reread(ctx, e, sandbox)
}

func (b *Backend) wake(ctx context.Context, e *entry, sandbox string) error {
	if err := b.api.verb(ctx, sandbox, "wake", nil); err != nil {
		return err
	}
	b.mu.Lock()
	wakes := e.wakes + 1
	b.mu.Unlock()
	if err := b.api.patchEnv(ctx, sandbox, map[string]*envVar{envWakes: {Value: strconv.Itoa(wakes)}}); err != nil {
		return fmt.Errorf("record wake: %w", err)
	}
	b.mu.Lock()
	e.wakes, e.ready = wakes, false
	b.touchLocked(e)
	b.mu.Unlock()
	return b.reread(ctx, e, sandbox)
}

// reread replaces e's row with sandbox's current one.
func (b *Backend) reread(ctx context.Context, e *entry, sandbox string) error {
	r, err := b.api.get(ctx, sandbox)
	if err != nil {
		return err
	}
	r.Token = ""
	b.mu.Lock()
	defer b.mu.Unlock()
	e.row = &r
	b.touchLocked(e)
	return nil
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
		r, err = b.api.get(ctx, sandbox)
	} else {
		var rows []row
		if rows, err = b.api.find(ctx, metaMachine, id); err == nil && len(rows) == 0 {
			err = machines.ErrNotFound
		} else if err == nil {
			r = rows[0]
		}
	}
	if errors.Is(err, machines.ErrNotFound) {
		b.forget(id, sandbox)
		return machines.Machine{}, machines.ErrNotFound
	}
	if err != nil {
		return machines.Machine{}, err
	}
	r.Token = ""
	if err := b.index(ctx, []row{r}, false); err != nil {
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
func (b *Backend) forget(id, sandbox string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.entries[id]; e != nil && e.op == "" && (e.row == nil || e.row.ID == sandbox) {
		b.dropLocked(id)
	}
}

// refresh re-reads every claim, drops machines whose claim is gone and probes agents not yet seen.
func (b *Backend) refresh(ctx context.Context) error {
	rows, err := b.api.list(ctx)
	if err != nil {
		return err
	}
	if err := b.index(ctx, rows, true); err != nil {
		return err
	}
	b.mu.Lock()
	var unready []string
	for id, e := range b.entries {
		if state, _ := e.state(); state == machines.StateStarting && e.op == "" && e.row != nil {
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

// index merges rows into the entries, reading the env of claims it has not seen; complete drops the entries rows lack.
func (b *Backend) index(ctx context.Context, rows []row, complete bool) error {
	present := map[string]bool{}
	for _, r := range rows {
		id := r.Metadata[metaMachine]
		if id == "" {
			continue
		}
		present[id] = true
		b.mu.Lock()
		e := b.entries[id]
		known := e != nil && e.row != nil && e.row.ID == r.ID
		unchanged := known && reflect.DeepEqual(*e.row, r) && e.owner == cmp.Or(r.Tenant, e.owner) && e.image == r.Key.Template
		b.mu.Unlock()
		if unchanged {
			continue
		}
		var env map[string]envVar
		if !known {
			var err error
			if env, err = b.api.env(ctx, r.ID); err != nil {
				return fmt.Errorf("read env of %s: %w", r.ID, err)
			}
		}
		b.mu.Lock()
		if e = b.entries[id]; e == nil {
			e = &entry{id: id, owner: r.Metadata[metaOwner]}
			b.entries[id] = e
		}
		// The tenant stamp is sandboxd's own, so it outranks the owner metadata the api wrote.
		e.owner = cmp.Or(r.Tenant, e.owner)
		if !known {
			e.loadEnv(env)
		}
		cur := r
		e.row, e.image = &cur, r.Key.Template
		b.touchLocked(e)
		b.mu.Unlock()
	}
	if !complete {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, e := range b.entries {
		if !present[id] && e.op == "" && e.row != nil {
			b.dropLocked(id)
		}
	}
	return nil
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
	if e == nil || e.row == nil || e.op != "" || e.row.Hibernated || e.row.Archived || e.row.Failed != "" ||
		(e.ready && e.readyRestarts == e.row.Restarts) {
		b.mu.Unlock()
		return
	}
	sandbox, restarts := e.row.ID, e.row.Restarts
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := b.api.dialPort(ctx, sandbox, b.opts.AgentPort)
	if err != nil {
		return
	}
	_ = conn.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.row != nil && e.row.ID == sandbox && e.row.Restarts == restarts {
		e.ready, e.readyRestarts = true, restarts
		b.touchLocked(e)
	}
}

// renew extends the lease of every claim that is not archived.
func (b *Backend) renew(ctx context.Context) error {
	b.mu.Lock()
	var sandboxes []string
	for _, e := range b.entries {
		if e.row != nil && !e.row.Archived {
			sandboxes = append(sandboxes, e.row.ID)
		}
	}
	b.mu.Unlock()
	var errs []error
	for _, sandbox := range sandboxes {
		if err := b.api.verb(ctx, sandbox, "renew", renewRequest{TTLSeconds: leaseSeconds, OnExpire: onExpire}); err != nil && !isStatus(err, http.StatusConflict) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

// differences returns the ids whose ledger row differs from the entry.
func (b *Backend) differences(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	entries := make([]entry, 0, len(b.entries))
	for _, e := range b.entries {
		copied := *e
		copied.meta = maps.Clone(e.meta)
		entries = append(entries, copied)
	}
	b.mu.Unlock()
	want := make(map[string]cluster.Machine, len(entries))
	for _, e := range entries {
		want[e.id] = e.record()
	}
	rows, err := b.opts.Ledger.Machines(ctx)
	if err != nil {
		return nil, err
	}
	have := make(map[string]cluster.Machine, len(rows))
	for _, r := range rows {
		have[r.ID] = r
	}
	ids := slices.Collect(maps.Keys(want))
	for id := range have {
		if _, ok := want[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var differ []string
	for _, id := range ids {
		w, inMemory := want[id]
		h, inLedger := have[id]
		if inMemory != inLedger || (inMemory && !w.Equal(h)) {
			differ = append(differ, id)
		}
	}
	return differ, nil
}

func (b *Backend) loop(interval time.Duration, fn func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(b.ctx, requestTimeout)
		if err := fn(ctx); err != nil {
			log.WithFunc("sandboxd.Backend.loop").Warnf(ctx, "every %s: %v", interval, err)
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
