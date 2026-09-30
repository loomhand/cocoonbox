// Package boxes serves cocoonbox.control.v1.Boxes on CocoonSets, one per user.
package boxes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/projecteru2/core/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/config"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const cacheSyncTimeout = time.Minute

var (
	// CocoonSetResource is the GroupVersionResource of CocoonSets.
	CocoonSetResource = cocoonv1.GroupVersion.WithResource("cocoonsets")

	suspendPatch   = []byte(`{"spec":{"suspend":true}}`)
	unsuspendPatch = []byte(`{"spec":{"suspend":false}}`)
)

// Service implements controlv1.BoxesServer and resolves boxes for the edge.
type Service struct {
	controlv1.UnimplementedBoxesServer

	cfg       *config.Config
	sets      dynamic.ResourceInterface
	setLister cache.GenericNamespaceLister
	podLister corelisters.PodNamespaceLister
	ops       *operations
}

// New starts the CocoonSet and pod informers of cfg.Namespace and waits for their caches.
func New(ctx context.Context, cfg *config.Config, dyn dynamic.Interface, kube kubernetes.Interface) (*Service, error) {
	setFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, cfg.Namespace, nil)
	setInformer := setFactory.ForResource(CocoonSetResource)
	podFactory := informers.NewSharedInformerFactoryWithOptions(kube, 0,
		informers.WithNamespace(cfg.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = meta.LabelCocoonSet }))
	podInformer := podFactory.Core().V1().Pods()

	s := &Service{
		cfg:       cfg,
		sets:      dyn.Resource(CocoonSetResource).Namespace(cfg.Namespace),
		setLister: setInformer.Lister().ByNamespace(cfg.Namespace),
		podLister: podInformer.Lister().Pods(cfg.Namespace),
		ops:       newOperations(),
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { s.onChange(obj) },
		UpdateFunc: func(_, obj any) { s.onChange(obj) },
		DeleteFunc: func(obj any) { s.onDelete(obj) },
	}
	if _, err := setInformer.Informer().AddEventHandler(handler); err != nil {
		return nil, fmt.Errorf("watch cocoonsets: %w", err)
	}
	if _, err := podInformer.Informer().AddEventHandler(handler); err != nil {
		return nil, fmt.Errorf("watch pods: %w", err)
	}
	setFactory.Start(ctx.Done())
	podFactory.Start(ctx.Done())

	syncCtx, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), setInformer.Informer().HasSynced, podInformer.Informer().HasSynced) {
		return nil, errors.New("sync cocoonset and pod caches: timed out")
	}
	return s, nil
}

func (s *Service) Ensure(ctx context.Context, req *controlv1.EnsureRequest) (*controlv1.EnsureResponse, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return nil, err
	}
	id := BoxID(user)
	flavor := resolveFlavor(req.GetFlavor(), s.cfg.Flavor)
	logger := log.WithFunc("boxes.Service.Ensure").WithField("box", id)

	cs, err := s.getSet(ctx, id)
	if apierrors.IsNotFound(err) {
		created, createErr := s.createSet(ctx, buildCocoonSet(s.cfg, id, user, flavor))
		if createErr != nil {
			return nil, kubeError(ctx, logger, createErr, "create box")
		}
		logger.Infof(ctx, "created cocoonset for user %s", user)
		return s.withOperation(created, controlv1.Phase_PHASE_CREATING), nil
	}
	if err != nil {
		return nil, kubeError(ctx, logger, err, "get box")
	}
	if cs.DeletionTimestamp != nil {
		return nil, status.Error(codes.FailedPrecondition, "box is being deleted")
	}
	if req.GetFlavor() != nil && !sameFlavor(flavorOf(cs), flavor) {
		return nil, status.Errorf(codes.FailedPrecondition, "box exists with flavor %v", flavorOf(cs))
	}
	if cs.Spec.Suspend {
		woken, err := s.patchSet(ctx, id, unsuspendPatch)
		if err != nil {
			return nil, kubeError(ctx, logger, err, "wake box")
		}
		logger.Info(ctx, "waking box")
		return s.withOperation(woken, controlv1.Phase_PHASE_WAKING), nil
	}
	box := toBox(cs, s.mainPod(id))
	if box.State == controlv1.BoxState_BOX_STATE_RUNNING {
		return &controlv1.EnsureResponse{Box: box}, nil
	}
	phase := controlv1.Phase_PHASE_CREATING
	if cur := s.ops.activeFor(id); cur != nil && cur.phase == controlv1.Phase_PHASE_WAKING {
		phase = cur.phase
	}
	return s.withOperation(cs, phase), nil
}

func (s *Service) Get(ctx context.Context, req *controlv1.GetBoxRequest) (*controlv1.Box, error) {
	id, err := ownedBox(ctx, req.GetBoxId())
	if err != nil {
		return nil, err
	}
	cs, err := s.getSet(ctx, id)
	if apierrors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "box not found")
	}
	if err != nil {
		return nil, kubeError(ctx, log.WithFunc("boxes.Service.Get").WithField("box", id), err, "get box")
	}
	return toBox(cs, s.mainPod(id)), nil
}

func (s *Service) List(ctx context.Context, _ *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return nil, err
	}
	id := BoxID(user)
	cs, err := s.getSet(ctx, id)
	if apierrors.IsNotFound(err) {
		return &controlv1.ListBoxesResponse{}, nil
	}
	if err != nil {
		return nil, kubeError(ctx, log.WithFunc("boxes.Service.List").WithField("box", id), err, "list boxes")
	}
	return &controlv1.ListBoxesResponse{Boxes: []*controlv1.Box{toBox(cs, s.mainPod(id))}}, nil
}

func (s *Service) Hibernate(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error) {
	id, err := ownedBox(ctx, ref.GetBoxId())
	if err != nil {
		return nil, err
	}
	logger := log.WithFunc("boxes.Service.Hibernate").WithField("box", id)
	cs, err := s.getSet(ctx, id)
	if apierrors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "box not found")
	}
	if err != nil {
		return nil, kubeError(ctx, logger, err, "get box")
	}
	if cs.DeletionTimestamp != nil {
		return nil, status.Error(codes.FailedPrecondition, "box is being deleted")
	}
	if !cs.Spec.Suspend {
		if cs, err = s.patchSet(ctx, id, suspendPatch); err != nil {
			return nil, kubeError(ctx, logger, err, "hibernate box")
		}
		logger.Info(ctx, "hibernating box")
	}
	return s.withOperation(cs, controlv1.Phase_PHASE_SNAPSHOTTING).GetOperation(), nil
}

func (s *Service) WatchOperation(req *controlv1.WatchOperationRequest, stream grpc.ServerStreamingServer[controlv1.OperationEvent]) error {
	ctx := stream.Context()
	user, err := callerUser(ctx)
	if err != nil {
		return err
	}
	op := s.ops.get(req.GetOperationId())
	if op == nil || op.boxID != BoxID(user) {
		return status.Error(codes.NotFound, "operation not found")
	}
	offset := 0
	if key := req.GetFromOffsetKey(); key != "" {
		if offset, err = strconv.Atoi(key); err != nil || offset < 0 {
			return status.Errorf(codes.InvalidArgument, "invalid offset key %q", key)
		}
	}
	for {
		events, done, changed := op.since(offset)
		for _, ev := range events {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		offset += len(events)
		if done {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// Resolve returns the agent address of boxID after checking that user owns it and it runs.
func (s *Service) Resolve(_ context.Context, user, boxID string) (string, error) {
	if boxID != BoxID(user) {
		return "", status.Error(codes.PermissionDenied, "box belongs to another user")
	}
	cs, err := s.cachedSet(boxID)
	if err != nil {
		return "", err
	}
	main := s.mainPod(boxID)
	if obs := observe(cs, main, 0); obs.state != controlv1.BoxState_BOX_STATE_RUNNING {
		return "", status.Errorf(codes.Unavailable, "box is %s", obs.state)
	}
	ip := cmp.Or(main.Status.PodIP, meta.ParseVMRuntime(main).IP)
	if ip == "" {
		return "", status.Error(codes.Unavailable, "box has no address yet")
	}
	return net.JoinHostPort(ip, strconv.Itoa(s.cfg.AgentPort)), nil
}

func (s *Service) withOperation(cs *cocoonv1.CocoonSet, phase controlv1.Phase) *controlv1.EnsureResponse {
	op, _ := s.ops.start(cs.Name, phase, cs.Generation)
	s.track(op)
	return &controlv1.EnsureResponse{
		Box:       toBox(cs, s.mainPod(cs.Name)),
		Operation: &controlv1.Operation{Id: op.id, BoxId: op.boxID},
	}
}

func (s *Service) onChange(obj any) {
	var id string
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		id = o.GetName()
	case *corev1.Pod:
		id = o.Labels[meta.LabelCocoonSet]
	}
	if op := s.ops.activeFor(id); op != nil {
		s.track(op)
	}
}

func (s *Service) onDelete(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		s.onChange(obj)
		return
	}
	if op := s.ops.activeFor(u.GetName()); op != nil {
		op.fail("box was deleted")
	}
}

// track records the cached state of op's box, skipping a cache that predates op.
func (s *Service) track(op *operation) {
	cs, err := s.cachedSet(op.boxID)
	if err != nil || cs.Generation < op.gen {
		return
	}
	op.record(observe(cs, s.mainPod(op.boxID), op.gen))
}

func (s *Service) getSet(ctx context.Context, id string) (*cocoonv1.CocoonSet, error) {
	u, err := s.sets.Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructured(u)
}

func (s *Service) createSet(ctx context.Context, cs *cocoonv1.CocoonSet) (*cocoonv1.CocoonSet, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cs)
	if err != nil {
		return nil, fmt.Errorf("encode cocoonset: %w", err)
	}
	u, err := s.sets.Create(ctx, &unstructured.Unstructured{Object: raw}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructured(u)
}

func (s *Service) patchSet(ctx context.Context, id string, patch []byte) (*cocoonv1.CocoonSet, error) {
	u, err := s.sets.Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return fromUnstructured(u)
}

func (s *Service) cachedSet(id string) (*cocoonv1.CocoonSet, error) {
	obj, err := s.setLister.Get(id)
	if apierrors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "box not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "read box cache")
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected cached object %T", obj)
	}
	cs, err := fromUnstructured(u)
	if err != nil {
		return nil, status.Error(codes.Internal, "decode box")
	}
	return cs, nil
}

func (s *Service) mainPod(id string) *corev1.Pod {
	pod, err := s.podLister.Get(mainPodName(id))
	if err != nil || pod.DeletionTimestamp != nil {
		return nil
	}
	return pod
}

func fromUnstructured(u *unstructured.Unstructured) (*cocoonv1.CocoonSet, error) {
	var cs cocoonv1.CocoonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cs); err != nil {
		return nil, fmt.Errorf("decode cocoonset %s: %w", u.GetName(), err)
	}
	return &cs, nil
}

func callerUser(ctx context.Context) (string, error) {
	if user := auth.User(ctx); user != "" {
		return user, nil
	}
	return "", status.Error(codes.Unauthenticated, "unauthenticated")
}

func ownedBox(ctx context.Context, boxID string) (string, error) {
	user, err := callerUser(ctx)
	if err != nil {
		return "", err
	}
	if boxID != BoxID(user) {
		return "", status.Error(codes.PermissionDenied, "box belongs to another user")
	}
	return boxID, nil
}

func kubeError(ctx context.Context, logger *log.Fields, err error, action string) error {
	logger.Error(ctx, err, action)
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return status.Errorf(codes.Aborted, "%s: concurrent change, retry", action)
	}
	return status.Errorf(codes.Unavailable, "%s failed", action)
}
