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
	"github.com/loomhand/cocoonbox/api/utils"
)

const startHint = "call box_ensure to bring it up"

var (
	// ErrNoBox answers every box id the caller does not own alike: another user's, a deleted one, an unknown one.
	ErrNoBox = status.Error(codes.NotFound, "no such box for this user (it may have been deleted)")

	errHibernating = errors.New("box is hibernating; call box_ensure again once it is hibernated")
)

// Defaults name new boxes' images and bound how many boxes of one user run at once.
type Defaults struct {
	Image        string
	RebaseImages []string
	MaxRunning   int
}

// offered resolves ref, an image or the family of one (such as cbdesk), to an offered image; empty is the default image.
func (d Defaults) offered(ref string) (string, bool) {
	if ref == "" || ref == d.Image || slices.Contains(d.RebaseImages, ref) {
		return cmp.Or(ref, d.Image), true
	}
	if Family(ref) != ref {
		return "", false
	}
	return d.newest(ref)
}

// kept returns image while it is offered, else what a box on image upgrades to with the user's own image own.
func (d Defaults) kept(own, image string) string {
	if offered, ok := d.offered(image); ok {
		return offered
	}
	return d.upgrade(own, image)
}

// upgrade is the image a box on image upgrades to: own when it is of image's family, else the newest image of that family.
func (d Defaults) upgrade(own, image string) string {
	if own != "" && Family(own) == Family(image) {
		return own
	}
	return d.latest(image)
}

// latest is the newest offered image of image's family, else image itself; empty is the default image.
func (d Defaults) latest(image string) string {
	if image == "" {
		return d.Image
	}
	if newest, ok := d.newest(Family(image)); ok {
		return newest
	}
	return image
}

// newest is the newest offered image of family: the default image for its family, else the first rebase image of it.
func (d Defaults) newest(family string) (string, bool) {
	if Family(d.Image) == family {
		return d.Image, true
	}
	i := slices.IndexFunc(d.RebaseImages, func(r string) bool { return Family(r) == family })
	if i < 0 {
		return "", false
	}
	return d.RebaseImages[i], true
}

func (d Defaults) choices() string {
	return strings.Join(d.images(), ", ") + ", or the family of one (the name before the tag's ':' after the last '/')"
}

// images lists the default image, then each rebase image once.
func (d Defaults) images() []string {
	images := []string{d.Image}
	for _, image := range d.RebaseImages {
		if !slices.Contains(images, image) {
			images = append(images, image)
		}
	}
	return images
}

// Registry keeps each user's boxes and their names; users.Service is the production one.
type Registry interface {
	Boxes(owner string) []users.Box
	HasBox(owner, id string) bool
	AddBox(ctx context.Context, b users.Box) (users.Box, error)
	RenameBox(ctx context.Context, owner, id, name string) (users.Box, error)
	SetBoxFlavor(ctx context.Context, owner, id string, flavor machines.Flavor) error
	RemoveBox(ctx context.Context, owner, id string) error
	// MaxRunning is the owner's own running limit, 0 for MaxRunning of the defaults.
	MaxRunning(owner string) int
	Policy(owner string) users.Policy
	Disabled(owner string) bool
	AccountDisabled(ctx context.Context, owner string) (bool, error)
	OnBoxes(fn func(owner string))
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
	reaches   *reaches
	userLocks *cluster.Store

	starts utils.Locks
}

// New serves the boxes registry keeps on backend with defaults.
func New(backend machines.Machines, defaults Defaults, registry Registry) *Service {
	s := &Service{machines: backend, defaults: defaults, registry: registry, ops: newOperations()}
	registry.OnBoxes(s.ops.watchers.wake)
	return s
}

func (s *Service) Ensure(ctx context.Context, req *controlv1.EnsureRequest) (*controlv1.EnsureResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	unlock, err := s.admitStart(ctx, user)
	if err != nil {
		return nil, err
	}
	defer unlock()
	box := req.GetBoxId()
	if box == "" {
		if box, err = s.firstBox(ctx, user); err != nil {
			return nil, err
		}
	} else if !s.registry.HasBox(user, box) {
		return nil, ErrNoBox
	}
	if s.deleting(box) {
		return nil, status.Error(codes.FailedPrecondition, "box is being deleted")
	}
	logger := log.WithFunc("boxes.Service.Ensure").WithField("box", box)

	tenant, err := s.accounts.Tenant(user)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "read tenant")
	}
	flavor, ok := s.flavorOf(user, box)
	if !ok {
		return nil, ErrNoBox
	}
	spec := machines.Spec{ID: box, Owner: user, Tenant: tenant, Flavor: flavor}
	current, err := s.startable(ctx, user, box)
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
		spec.ID = cur.ID
	default:
		if saved, err = s.savedHead(ctx, user, box); err != nil {
			return nil, serviceError(ctx, logger, err, "read saved state")
		}
		switch {
		case saved.Latest == "":
			spec.Image = s.ownImage(user)
		case saved.Parked:
			spec.ID, spec.Image, spec.Meta = newMachineID(box), s.defaults.kept(s.ownImage(user), saved.Image), map[string]string{metaRestore: saved.Latest}
		default:
			spec.ID, spec.Image, spec.Meta = newMachineID(box), s.defaults.upgrade(s.ownImage(user), saved.Image), map[string]string{metaRestore: saved.Latest}
		}
	}
	m, action, err := s.machines.Ensure(ctx, spec)
	if err != nil {
		return nil, serviceError(ctx, logger, err, "ensure box")
	}
	s.ensureReached(ctx, box, action)
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
	if s.reachable(m) {
		return &controlv1.EnsureResponse{Box: s.toBox(ctx, user, box, m)}, nil
	}
	phase := controlv1.Phase_PHASE_CREATING
	if active := s.ops.activeFor(box); active != nil && active.phase == controlv1.Phase_PHASE_WAKING {
		phase = active.phase
	}
	return s.withOperation(ctx, user, box, m, phase), nil
}

func (s *Service) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	user, err := auth.Caller(ctx)
	if err != nil {
		return nil, err
	}
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.List").WithField("user", user), err, "list boxes")
	}
	resp := &controlv1.ListBoxesResponse{OfferedImages: s.defaults.images()}
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
		if op.gone(0) {
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
		var lease <-chan time.Time
		if op.remote {
			lease = time.After(time.Until(op.leaseEnd()))
		}
		select {
		case <-changed:
		case <-lease:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// Resolve returns the agent endpoint of boxID and its wake id after checking that user owns it and it is reachable.
func (s *Service) Resolve(ctx context.Context, user, boxID string) (machines.Endpoint, string, error) {
	var none machines.Endpoint
	if !s.registry.HasBox(user, boxID) {
		return none, "", ErrNoBox
	}
	cur, err := s.current(ctx, user, boxID)
	var ep machines.Endpoint
	if err == nil && !s.reachable(cur) && cur.State == machines.StateRunning {
		err = &machines.NotRunningError{State: machines.StateStarting}
	}
	if err == nil {
		ep, err = s.machines.Address(ctx, cur.ID)
	}
	switch notRunning, isNotRunning := errors.AsType[*machines.NotRunningError](err); {
	case err == nil:
		return ep, wakeOf(cur), nil
	case errors.Is(err, machines.ErrNotFound):
		return none, "", status.Error(codes.Unavailable, "box is not running; "+startHint)
	case isNotRunning:
		return none, "", status.Errorf(codes.Unavailable, "box is %s; %s", stateWords[notRunning.State], startHint)
	default:
		log.WithFunc("boxes.Service.Resolve").WithField("box", boxID).Error(ctx, err, "resolve box")
		return none, "", status.Error(codes.Unavailable, "resolve box failed")
	}
}

func (s *Service) get(ctx context.Context, box string) (*controlv1.Box, error) {
	user, err := s.ownerOf(ctx, box)
	if err != nil {
		return nil, err
	}
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return nil, serviceError(ctx, log.WithFunc("boxes.Service.get").WithField("box", box), err, "get box")
	}
	return s.toBox(ctx, user, box, current[box]), nil
}

// current returns the serving machine of user's box: the oldest, which a rebase replaces only when it deletes it.
func (s *Service) current(ctx context.Context, user, box string) (machines.Machine, error) {
	current, err := s.currentMachines(ctx, user)
	if err != nil {
		return machines.Machine{}, err
	}
	cur, ok := current[box]
	if !ok {
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
	return serving(ms), nil
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

// flavorOf returns the flavor box records, which every machine of the box takes.
func (s *Service) flavorOf(user, box string) (machines.Flavor, bool) {
	for _, b := range s.registry.Boxes(user) {
		if b.ID == box {
			return b.Flavor, true
		}
	}
	return machines.Flavor{}, false
}

// checkRunning refuses box while the user's other running boxes, parked and agent boxes aside, hold the running limit.
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

func (s *Service) boxName(user, box string) string {
	for _, rec := range s.registry.Boxes(user) {
		if rec.ID == box {
			return rec.Name
		}
	}
	return ""
}

// ownImage is the image the user's policy names while Rebase offers it, else the newest offered of its family, else empty.
func (s *Service) ownImage(user string) string {
	image := s.registry.Policy(user).Image
	if image == "" || image == s.defaults.Image || slices.Contains(s.defaults.RebaseImages, image) {
		return image
	}
	newest, _ := s.defaults.newest(Family(image))
	return newest
}

func (s *Service) ensureReached(ctx context.Context, box string, action machines.Action) {
	switch {
	case ctx.Value(alarmCall{}) != nil:
	case action != machines.ActionNone:
		s.Reached(box)()
	case s.wake != nil:
		s.wake.reach(box)
		if s.reaches != nil {
			s.reaches.reach(ensuredPrefix+box, s.reaches.now())
		}
	}
}

// lockStarts serializes the requests that may start or delete a machine of user, in every api with a shared table.
func (s *Service) lockStarts(ctx context.Context, user string) (func(), error) {
	release := s.starts.Lock(user)
	if s.userLocks == nil {
		return release, nil
	}
	logger := log.WithFunc("boxes.Service.lockStarts").WithField("user", user)
	unlock, err := s.userLocks.Lock(ctx, "starts:"+user)
	what := "lock the starts"
	if err == nil {
		if err = s.syncUser(ctx, user); err != nil {
			unlock()
			what = "read the user's shared state"
		}
	}
	if err != nil {
		release()
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		logger.Error(ctx, err, what)
		return nil, status.Errorf(codes.Unavailable, "%s failed", what)
	}
	held := time.Now()
	return func() {
		// the next holder in another process reads the operations from the table, so the ones written under the lock reach it first.
		if keys := s.ops.heldSince(user, held); len(keys) > 0 {
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedTimeout)
			if err := s.mirror.FlushKeys(flushCtx, keys); err != nil {
				logger.Error(ctx, err, "write the operations of the starts before the lock goes")
			}
			cancel()
		}
		unlock()
		release()
	}, nil
}

func (s *Service) lockShared(ctx context.Context, user string) (func(), error) {
	if s.userLocks == nil {
		return func() {}, nil
	}
	return s.lockStarts(ctx, user)
}

// syncUser reads user's operations and heads from the shared table, so a start under the user's lock never acts on what this process's cache has yet to learn.
func (s *Service) syncUser(ctx context.Context, user string) error {
	if err := s.ops.reload(ctx, user); err != nil {
		return err
	}
	if s.state != nil {
		s.state.forgetHeads(user)
	}
	return nil
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
		if m.State == machines.StateRunning && !s.reachable(m) {
			op.record(observation{state: controlv1.BoxState_BOX_STATE_STARTING, detail: "provisioning"})
			if err := s.provisionMachine(op.ctx, m, false); err != nil {
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
	if m.State == machines.StateRunning && !s.reachable(m) {
		b.State = controlv1.BoxState_BOX_STATE_STARTING
	}
	b.LatestImage = s.defaults.upgrade(s.ownImage(user), m.Image)
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

// Family is the family of image: its name before the tag, the ':' after the last '/'.
func Family(image string) string {
	name := strings.LastIndexByte(image, '/') + 1
	if tag := strings.IndexByte(image[name:], ':'); tag >= 0 {
		return image[:name+tag]
	}
	return image
}

func serviceError(ctx context.Context, logger *log.Fields, err error, action string) error {
	switch {
	case errors.Is(err, machines.ErrNotFound):
		return ErrNoBox
	case errors.Is(err, errHibernating):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, machines.ErrConflict):
		logger.Error(ctx, err, action)
		return status.Errorf(codes.Aborted, "%s: concurrent change, retry", action)
	default:
		logger.Error(ctx, err, action)
		return status.Errorf(codes.Unavailable, "%s failed", action)
	}
}
