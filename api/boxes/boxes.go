// Package boxes serves cocoonbox.control.v1.Boxes, the boxes of each user, on any machines.Machines backend.
package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/cluster"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/machines"
	"github.com/loomhand/cocoonbox/api/users"
)

// ErrNoBox answers every box id the caller does not own alike: another user's, a deleted one, an unknown one.
var ErrNoBox = status.Error(codes.NotFound, "no such box for this user (it may have been deleted)")

// Defaults size new boxes, name the image they start from, list the other flavors Ensure and images Rebase accept,
// name the users who start from and upgrade to one of those images, and bound how many boxes of one user run at once.
type Defaults struct {
	Flavor       machines.Flavor
	Flavors      []machines.Flavor
	Image        string
	RebaseImages []string
	ImageOwners  map[string][]string
	MaxRunning   int
}

// offered resolves ref, an image or the family of one (such as cbdesk), to an offered image; empty is the default image.
func (d Defaults) offered(ref string) (string, bool) {
	if ref == "" || ref == d.Image || slices.Contains(d.RebaseImages, ref) {
		return cmp.Or(ref, d.Image), true
	}
	if strings.Contains(ref, ":") {
		return "", false
	}
	latest := d.latest(ref + ":")
	return latest, strings.HasPrefix(latest, ref+":")
}

// kept returns image while it is offered, else what user's box on image upgrades to.
func (d Defaults) kept(user, image string) string {
	if offered, ok := d.offered(image); ok {
		return offered
	}
	return d.upgrade(user, image)
}

// upgrade is the image user's box on image upgrades to: the image user owns, else the newest image of image's family.
func (d Defaults) upgrade(user, image string) string {
	return cmp.Or(d.ownImage(user), d.latest(image))
}

// latest is the newest image of image's family (the name before ':'): the default image for its family, else the first rebase image of that family.
func (d Defaults) latest(image string) string {
	family, _, _ := strings.Cut(image, ":")
	if def, _, _ := strings.Cut(d.Image, ":"); image == "" || family == def {
		return d.Image
	}
	if i := slices.IndexFunc(d.RebaseImages, func(r string) bool { return strings.HasPrefix(r, family+":") }); i >= 0 {
		return d.RebaseImages[i]
	}
	return d.Image
}

func (d Defaults) ownImage(user string) string {
	for image, owners := range d.ImageOwners {
		if slices.Contains(owners, user) {
			return image
		}
	}
	return ""
}

// Registry keeps each user's boxes and their names; users.Service is the production one.
type Registry interface {
	Boxes(owner string) []users.Box
	HasBox(owner, id string) bool
	AddBox(ctx context.Context, b users.Box) (users.Box, error)
	RenameBox(ctx context.Context, owner, id, name string) (users.Box, error)
	RemoveBox(ctx context.Context, owner, id string) error
	// MaxRunning is the owner's own running limit, 0 for MaxRunning of the defaults.
	MaxRunning(owner string) int
}

// Service implements controlv1.BoxesServer and resolves boxes for the edge.
type Service struct {
	controlv1.UnimplementedBoxesServer

	machines  machines.Machines
	defaults  Defaults
	registry  Registry
	ops       *operations
	state     *stateLayer
	provision *provisioner
	exit      *exitPolicy
	accounts  Accounts
	connect   Connect
	vault     Vault
	wake      *wakeAlarms
	park      *parking
	usage     *usageMeter
	idle      *idling
	activity  Activity
	ledger    *cluster.Store
	mirror    *cluster.Mirror

	mu     sync.Mutex
	starts map[string]*sync.Mutex
}

// New serves the boxes registry keeps on backend with defaults.
func New(backend machines.Machines, defaults Defaults, registry Registry) *Service {
	return &Service{machines: backend, defaults: defaults, registry: registry, ops: newOperations(), starts: map[string]*sync.Mutex{}}
}

func (s *Service) Ensure(ctx context.Context, req *controlv1.EnsureRequest) (*controlv1.EnsureResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	flavor, err := s.flavor(req.GetFlavor())
	if err != nil {
		return nil, err
	}
	unlock := s.lockStarts(user)
	defer unlock()
	box := req.GetBoxId()
	if box == "" {
		if box, err = s.firstBox(ctx, user); err != nil {
			return nil, err
		}
	} else if !s.registry.HasBox(user, box) {
		return nil, ErrNoBox
	}
	if ctx.Value(alarmCall{}) == nil {
		s.Reached(box)()
	}
	if s.deleting(box) {
		return nil, status.Error(codes.FailedPrecondition, "box is being deleted")
	}
	logger := log.WithFunc("boxes.Service.Ensure").WithField("box", box)

	tenant, err := s.tenantOf(user)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "read tenant")
	}
	spec := machines.Spec{ID: box, Owner: user, Tenant: tenant, Flavor: flavor}
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "get box")
	}
	var saved head
	cur, ok := current[box]
	if !ok || !holdsMemory(cur) || s.parking(box) {
		if err = s.checkRunning(user, box, current); err != nil {
			return nil, err
		}
	}
	switch {
	case ok:
		if active := s.ops.activeFor(box); active != nil && active.phase == controlv1.Phase_PHASE_RESTORING {
			return &controlv1.EnsureResponse{Box: s.toBox(ctx, user, box, cur), Operation: &controlv1.Operation{Id: active.id, BoxId: box}}, nil
		}
		if req.GetFlavor() != nil && cur.Flavor != flavor {
			return nil, status.Errorf(codes.FailedPrecondition, "box exists with flavor vcpus:%d memory_mib:%d disk_gib:%d",
				cur.Flavor.VCPUs, cur.Flavor.MemoryMiB, cur.Flavor.DiskGiB)
		}
		spec.ID = cur.ID
	default:
		if saved, err = s.savedHead(ctx, user, box); err != nil {
			return nil, serviceError(ctx, logger, err, "read saved state")
		}
		switch {
		case saved.Latest == "":
			spec.Image = s.defaults.ownImage(user)
		case saved.Parked:
			spec.ID, spec.Image = newMachineID(box), s.defaults.kept(user, saved.Image)
		default:
			spec.ID, spec.Image = newMachineID(box), s.defaults.upgrade(user, saved.Image)
		}
	}
	m, action, err := s.machines.Ensure(ctx, spec)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "ensure box")
	}
	switch action {
	case machines.ActionCreated:
		if saved.Latest != "" {
			logger.Infof(ctx, "box of user %s has no machine; restoring snapshot %s into %s", user, saved.Latest, m.ID)
			return s.restoreMissing(ctx, user, box, m, saved), nil
		}
		logger.Infof(ctx, "created machine %s for user %s", m.ID, user)
		return s.withOperation(ctx, user, box, m, controlv1.Phase_PHASE_CREATING), nil
	case machines.ActionWoken:
		s.wakeCredentials(ctx, m)
		logger.Info(ctx, "waking box")
		return s.withOperation(ctx, user, box, m, controlv1.Phase_PHASE_WAKING), nil
	}
	if s.ready(m) {
		return &controlv1.EnsureResponse{Box: s.toBox(ctx, user, box, m)}, nil
	}
	phase := controlv1.Phase_PHASE_CREATING
	if active := s.ops.activeFor(box); active != nil && active.phase == controlv1.Phase_PHASE_WAKING {
		phase = active.phase
	}
	return s.withOperation(ctx, user, box, m, phase), nil
}

// Get returns one of the caller's boxes; a box without a machine is BOX_STATE_UNSPECIFIED.
func (s *Service) Get(ctx context.Context, req *controlv1.GetBoxRequest) (*controlv1.Box, error) {
	user, err := s.ownerOf(ctx, req.GetBoxId())
	if err != nil {
		return nil, err
	}
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.Get").WithField("box", req.GetBoxId()), err, "get box")
	}
	return s.toBox(ctx, user, req.GetBoxId(), current[req.GetBoxId()]), nil
}

// List returns the caller's boxes, oldest first; a box without a machine is BOX_STATE_UNSPECIFIED.
func (s *Service) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.List").WithField("user", user), err, "list boxes")
	}
	resp := &controlv1.ListBoxesResponse{}
	for _, b := range s.registry.Boxes(user) {
		resp.Boxes = append(resp.Boxes, s.toBox(ctx, user, b.ID, current[b.ID]))
	}
	return resp, nil
}

func (s *Service) Hibernate(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	box := ref.GetBoxId()
	user, err := s.ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	logger := log.WithFunc("boxes.Service.Hibernate").WithField("box", box)
	cur, err := s.current(ctx, user, box)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "get box")
	}
	switch {
	case s.restoring(box):
		return nil, status.Error(codes.FailedPrecondition, "box is being restored from its saved state")
	case s.parking(box):
		return nil, status.Error(codes.FailedPrecondition, "box is being parked")
	case s.deleting(box):
		return nil, status.Error(codes.FailedPrecondition, "box is being deleted")
	}
	m, err := s.sleep(ctx, cur.ID)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "hibernate box")
	}
	logger.Info(ctx, "hibernating box")
	return s.withOperation(ctx, user, box, m, controlv1.Phase_PHASE_SNAPSHOTTING).GetOperation(), nil
}

func (s *Service) WatchOperation(req *controlv1.WatchOperationRequest, stream grpc.ServerStreamingServer[controlv1.OperationEvent]) error {
	ctx := stream.Context()
	user, err := auth.Caller(ctx)
	if err != nil {
		return err
	}
	op, err := s.ops.get(ctx, req.GetOperationId())
	if err != nil {
		log.WithFunc("boxes.Service.WatchOperation").Error(ctx, err, "read operation "+req.GetOperationId())
		return status.Error(codes.Unavailable, "read operation failed")
	}
	if op == nil || op.user != user {
		return status.Error(codes.NotFound, "operation not found")
	}
	offset := 0
	if key := req.GetFromOffsetKey(); key != "" {
		if offset, err = strconv.Atoi(key); err != nil || offset < 0 {
			return status.Errorf(codes.InvalidArgument, "invalid offset key %q", key)
		}
	}
	for {
		if op.remote && !time.Now().Before(op.expires) {
			op.fail("the api process that ran the operation is gone")
		}
		events, done, changed := op.since(offset)
		for _, ev := range events {
			if err := stream.Send(ev); err != nil {
				return err
			}
			offset = offsetOf(ev.GetOffsetKey())
		}
		if done {
			return nil
		}
		var poll <-chan time.Time
		if op.remote {
			changed, poll = s.ops.changed(op.id), time.After(remotePoll)
		}
		select {
		case <-changed:
		case <-poll:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
		if op.remote {
			switch next, err := s.ops.get(ctx, op.id); {
			case next != nil:
				op = next
			case err == nil:
				return status.Error(codes.NotFound, "operation not found")
			}
		}
	}
}

// Resolve returns the agent endpoint of boxID and its wake id after checking that user owns it and it is ready.
func (s *Service) Resolve(ctx context.Context, user, boxID string) (machines.Endpoint, string, error) {
	var none machines.Endpoint
	if !s.registry.HasBox(user, boxID) {
		return none, "", ErrNoBox
	}
	cur, err := s.current(ctx, user, boxID)
	var ep machines.Endpoint
	if err == nil && !s.ready(cur) && cur.State == machines.StateRunning {
		err = &machines.NotRunningError{State: machines.StateStarting}
	}
	if err == nil {
		ep, err = s.machines.Address(ctx, cur.ID)
	}
	var notRunning *machines.NotRunningError
	switch {
	case err == nil:
		return ep, wakeOf(cur), nil
	case errors.Is(err, machines.ErrNotFound):
		return none, "", ErrNoBox
	case errors.As(err, &notRunning):
		return none, "", status.Errorf(codes.Unavailable, "box is %s", boxStates[notRunning.State])
	default:
		log.WithFunc("boxes.Service.Resolve").WithField("box", boxID).Error(ctx, err, "resolve box")
		return none, "", status.Error(codes.Unavailable, "resolve box failed")
	}
}

// current returns the serving machine of user's box: the oldest, which a rebase replaces only when it deletes it.
func (s *Service) current(ctx context.Context, user, box string) (machines.Machine, error) {
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return machines.Machine{}, err
	}
	var cur machines.Machine
	for _, m := range ms {
		if boxOfMachine(m.ID) == box && (cur.ID == "" || older(m, cur)) {
			cur = m
		}
	}
	if cur.ID == "" {
		return machines.Machine{}, machines.ErrNotFound
	}
	return cur, nil
}

// currentMachines maps each of user's boxes that has a machine to its serving one.
func (s *Service) currentMachines(ctx context.Context, user string) (map[string]machines.Machine, error) {
	ms, err := s.machines.List(ctx, user)
	if err != nil {
		return nil, err
	}
	current := map[string]machines.Machine{}
	for _, m := range ms {
		box := boxOfMachine(m.ID)
		if cur, ok := current[box]; !ok || older(m, cur) {
			current[box] = m
		}
	}
	return current, nil
}

// firstBox returns user's oldest box, adding one when the user has none.
func (s *Service) firstBox(ctx context.Context, user string) (string, error) {
	if list := s.registry.Boxes(user); len(list) > 0 {
		return list[0].ID, nil
	}
	b, err := s.registry.AddBox(ctx, users.Box{ID: newBoxID(), Owner: user, Name: users.FirstBoxName})
	if err != nil {
		return "", registryError(ctx, err, "add box")
	}
	s.ops.watchers.wake(user)
	return b.ID, nil
}

// checkRunning refuses to start box of the user's own while others hold the user's running limit; a box being parked
// holds none, and an agent's boxes are bounded by the agent's caps instead.
func (s *Service) checkRunning(user, box string, current map[string]machines.Machine) error {
	if s.agentBox(user, box) {
		return nil
	}
	var running []string
	for id, m := range current {
		if id != box && holdsMemory(m) && !s.parking(id) && !s.agentBox(user, id) {
			running = append(running, id)
		}
	}
	limit := cmp.Or(s.registry.MaxRunning(user), s.defaults.MaxRunning)
	if len(running) < limit {
		return nil
	}
	slices.Sort(running)
	named := map[string]string{}
	for _, b := range s.registry.Boxes(user) {
		named[b.ID] = b.Name
	}
	names := make([]string, 0, len(running))
	for _, id := range running {
		names = append(names, fmt.Sprintf("%s (%s)", cmp.Or(named[id], "deleted"), id))
	}
	if len(names) > 1 {
		return status.Errorf(codes.FailedPrecondition, "boxes %s are running, and %d may run at a time; hibernate one first", strings.Join(names, ", "), limit)
	}
	return status.Errorf(codes.FailedPrecondition, "box %s is running, and %d may run at a time; hibernate it first", names[0], limit)
}

func (s *Service) flavor(req *controlv1.Flavor) (machines.Flavor, error) {
	flavor := resolveFlavor(req, s.defaults.Flavor)
	if flavor != s.defaults.Flavor && !slices.Contains(s.defaults.Flavors, flavor) {
		return flavor, status.Errorf(codes.InvalidArgument, "flavor vcpus:%d memory_mib:%d disk_gib:%d is not offered",
			flavor.VCPUs, flavor.MemoryMiB, flavor.DiskGiB)
	}
	return flavor, nil
}

func (s *Service) boxName(user, box string) string {
	for _, rec := range s.registry.Boxes(user) {
		if rec.ID == box {
			return rec.Name
		}
	}
	return ""
}

// lockStarts serializes the requests that may start or delete a machine of user, so two cannot both pass the running limit or race a delete.
func (s *Service) lockStarts(user string) func() {
	s.mu.Lock()
	l := s.starts[user]
	if l == nil {
		l = &sync.Mutex{}
		s.starts[user] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (s *Service) withOperation(ctx context.Context, user, box string, m machines.Machine, phase controlv1.Phase) *controlv1.EnsureResponse {
	op, started := s.ops.start(context.WithoutCancel(ctx), user, box, phase, m.Revision, operationTimeout)
	if started {
		go s.follow(op, m.ID)
	}
	return &controlv1.EnsureResponse{
		Box:       s.toBox(ctx, user, box, m),
		Operation: &controlv1.Operation{Id: op.id, BoxId: op.boxID},
	}
}

// follow records the backend's view of machine until op finishes or times out.
func (s *Service) follow(op *operation, machine string) {
	for m := range s.machines.Watch(op.ctx, machine, op.gen) {
		if m.State == machines.StateGone {
			op.fail("box was deleted")
			return
		}
		if m.State == machines.StateFailed {
			log.WithFunc("boxes.Service.follow").WithField("box", op.boxID).Warnf(op.ctx, "machine %s failed: %s (%s)", machine, m.Reason, m.Detail)
		}
		if m.State == machines.StateRunning && !s.ready(m) {
			op.record(observation{state: controlv1.BoxState_BOX_STATE_STARTING, detail: "provisioning"})
			if err := s.provisionMachine(op.ctx, m); err != nil {
				log.WithFunc("boxes.Service.follow").WithField("box", op.boxID).Error(op.ctx, err, "provision "+machine)
				op.fail(cmp.Or(failureWords[op.phase], "machine failed"))
				return
			}
			continue
		}
		op.record(toObservation(m, op.phase))
	}
	if errors.Is(op.ctx.Err(), context.DeadlineExceeded) {
		op.fail(fmt.Sprintf("timed out after %s", operationTimeout))
	}
}

func (s *Service) toBox(ctx context.Context, user, box string, m machines.Machine) *controlv1.Box {
	b := toBox(box, m)
	b.Name = s.boxName(user, box)
	if m.State == machines.StateRunning && !s.ready(m) {
		b.State = controlv1.BoxState_BOX_STATE_STARTING
	}
	b.LatestImage = s.defaults.upgrade(user, m.Image)
	if s.state != nil {
		b.LastSavedAtMs = s.state.lastSaved(ctx, statePath{user: user, box: box})
	}
	return b
}

// ownerOf returns the caller when it owns boxID.
func (s *Service) ownerOf(ctx context.Context, boxID string) (string, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return "", err
	}
	if !s.registry.HasBox(user, boxID) {
		return "", ErrNoBox
	}
	return user, nil
}

func serviceError(ctx context.Context, logger *log.Fields, err error, action string) error {
	switch {
	case errors.Is(err, machines.ErrNotFound):
		return ErrNoBox
	case errors.Is(err, machines.ErrConflict):
		logger.Error(ctx, err, action)
		return status.Errorf(codes.Aborted, "%s: concurrent change, retry", action)
	default:
		logger.Error(ctx, err, action)
		return status.Errorf(codes.Unavailable, "%s failed", action)
	}
}
