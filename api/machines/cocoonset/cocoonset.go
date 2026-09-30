// Package cocoonset runs machines as CocoonSets scheduled by cocoon-operator onto vk-cocoon.
package cocoonset

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/CMGS/cocoonbox/api/machines"
)

const cacheSyncTimeout = time.Minute

var (
	// Resource is the GroupVersionResource of CocoonSets.
	Resource = cocoonv1.GroupVersion.WithResource("cocoonsets")

	suspendPatch   = []byte(`{"spec":{"suspend":true}}`)
	unsuspendPatch = []byte(`{"spec":{"suspend":false}}`)
)

// Options configures the cluster access and the CocoonSets the backend creates.
type Options struct {
	// Kubeconfig is the kubeconfig path; empty uses $KUBECONFIG or the in-cluster config.
	Kubeconfig string
	Namespace  string
	Image      string
	Network    string
	ProbePort  int32
	AgentPort  int
}

var _ machines.Machines = (*Backend)(nil)

// Backend implements machines.Machines on CocoonSets in one namespace.
type Backend struct {
	opts      Options
	sets      dynamic.ResourceInterface
	setLister cache.GenericNamespaceLister
	podLister corelisters.PodNamespaceLister
	exec      podExecFunc

	mu       sync.Mutex
	watchers map[string]map[chan struct{}]struct{}
}

// New connects to the cluster, starts the CocoonSet and pod informers of opts.Namespace and waits for their caches.
func New(ctx context.Context, opts Options) (*Backend, error) {
	restCfg, err := clientcmd.BuildConfigFromFlags("", cmp.Or(opts.Kubeconfig, os.Getenv("KUBECONFIG")))
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return newBackend(ctx, opts, dyn, kube, newPodExec(restCfg, kube, opts.Namespace))
}

func newBackend(ctx context.Context, opts Options, dyn dynamic.Interface, kube kubernetes.Interface, exec podExecFunc) (*Backend, error) {
	setFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, opts.Namespace, nil)
	setInformer := setFactory.ForResource(Resource)
	podFactory := informers.NewSharedInformerFactoryWithOptions(kube, 0,
		informers.WithNamespace(opts.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = meta.LabelCocoonSet }))
	podInformer := podFactory.Core().V1().Pods()

	b := &Backend{
		opts:      opts,
		sets:      dyn.Resource(Resource).Namespace(opts.Namespace),
		setLister: setInformer.Lister().ByNamespace(opts.Namespace),
		podLister: podInformer.Lister().Pods(opts.Namespace),
		exec:      exec,
		watchers:  map[string]map[chan struct{}]struct{}{},
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    b.onChange,
		UpdateFunc: func(_, obj any) { b.onChange(obj) },
		DeleteFunc: b.onChange,
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
	return b, nil
}

func (b *Backend) Ensure(ctx context.Context, spec machines.Spec) (machines.Machine, machines.Action, error) {
	cs, err := b.getSet(ctx, spec.ID)
	if errors.Is(err, machines.ErrNotFound) {
		created, createErr := b.createSet(ctx, buildCocoonSet(b.opts, spec))
		if createErr != nil {
			return machines.Machine{}, machines.ActionNone, createErr
		}
		return b.machine(created, 0), machines.ActionCreated, nil
	}
	if err != nil {
		return machines.Machine{}, machines.ActionNone, err
	}
	if cs.DeletionTimestamp != nil {
		return machines.Machine{}, machines.ActionNone, machines.ErrDeleting
	}
	if !cs.Spec.Suspend {
		return b.machine(cs, 0), machines.ActionNone, nil
	}
	woken, err := b.patchSet(ctx, spec.ID, unsuspendPatch)
	if err != nil {
		return machines.Machine{}, machines.ActionNone, err
	}
	return b.machine(woken, 0), machines.ActionWoken, nil
}

func (b *Backend) Get(ctx context.Context, id string) (machines.Machine, error) {
	cs, err := b.getSet(ctx, id)
	if err != nil {
		return machines.Machine{}, err
	}
	return b.machine(cs, 0), nil
}

func (b *Backend) List(_ context.Context, owner string) ([]machines.Machine, error) {
	objs, err := b.setLister.List(labels.SelectorFromSet(labels.Set{labelManagedBy: managedBy}))
	if err != nil {
		return nil, fmt.Errorf("list cocoonsets: %w", err)
	}
	var out []machines.Machine
	for _, obj := range objs {
		cs, err := decode(obj)
		if err != nil {
			return nil, err
		}
		if cs.DeletionTimestamp == nil && (owner == "" || cs.Annotations[annotationOwner] == owner) {
			out = append(out, b.machine(cs, 0))
		}
	}
	slices.SortFunc(out, func(x, y machines.Machine) int { return cmp.Compare(x.ID, y.ID) })
	return out, nil
}

func (b *Backend) Hibernate(ctx context.Context, id string) (machines.Machine, error) {
	cs, err := b.getSet(ctx, id)
	if err != nil {
		return machines.Machine{}, err
	}
	if cs.DeletionTimestamp != nil {
		return machines.Machine{}, machines.ErrDeleting
	}
	if !cs.Spec.Suspend {
		if cs, err = b.patchSet(ctx, id, suspendPatch); err != nil {
			return machines.Machine{}, err
		}
	}
	return b.machine(cs, 0), nil
}

func (b *Backend) Watch(ctx context.Context, id string, revision int64) <-chan machines.Machine {
	out := make(chan machines.Machine)
	changed := make(chan struct{}, 1)
	b.subscribe(id, changed)
	go func() {
		defer close(out)
		defer b.unsubscribe(id, changed)
		seen := false
		for {
			cs, err := b.cachedSet(id)
			switch {
			case errors.Is(err, machines.ErrNotFound) && seen:
				send(ctx, out, machines.Machine{ID: id, State: machines.StateGone})
				return
			case err == nil:
				seen = true
				if cs.Generation >= revision && !send(ctx, out, b.machine(cs, revision)) {
					return
				}
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (b *Backend) Address(_ context.Context, id string) (string, error) {
	cs, err := b.cachedSet(id)
	if err != nil {
		return "", err
	}
	main := b.mainPod(id)
	if m := observe(cs, main, 0); m.State != machines.StateRunning {
		return "", &machines.NotRunningError{State: m.State}
	}
	ip := cmp.Or(main.Status.PodIP, meta.ParseVMRuntime(main).IP)
	if ip == "" {
		return "", machines.ErrNoAddress
	}
	return net.JoinHostPort(ip, strconv.Itoa(b.opts.AgentPort)), nil
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	if err := b.sets.Delete(ctx, id, metav1.DeleteOptions{}); err != nil {
		return kubeError(err, "delete cocoonset")
	}
	return nil
}

func (b *Backend) SetMeta(ctx context.Context, id, key, value string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{metaPrefix + key: value}}})
	if err != nil {
		return fmt.Errorf("encode meta patch: %w", err)
	}
	_, err = b.patchSet(ctx, id, patch)
	return err
}

func (b *Backend) Deliver(ctx context.Context, id string, identity machines.Identity, gwKey string) error {
	main := b.mainPod(id)
	if main == nil {
		return machines.ErrNotFound
	}
	if gwKey != "" {
		if _, err := b.exec(ctx, main.Name, writeGWKey, []byte(gwKey+"\n")); err != nil {
			return fmt.Errorf("install gw key: %w", err)
		}
	}
	file, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	if _, err := b.exec(ctx, main.Name, writeIdentity, file); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	return nil
}

func (b *Backend) machine(cs *cocoonv1.CocoonSet, gen int64) machines.Machine {
	return observe(cs, b.mainPod(cs.Name), gen)
}

func (b *Backend) subscribe(id string, changed chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watchers[id] == nil {
		b.watchers[id] = map[chan struct{}]struct{}{}
	}
	b.watchers[id][changed] = struct{}{}
}

func (b *Backend) unsubscribe(id string, changed chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.watchers[id], changed)
	if len(b.watchers[id]) == 0 {
		delete(b.watchers, id)
	}
}

func (b *Backend) onChange(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	var id string
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		id = o.GetName()
	case *corev1.Pod:
		id = o.Labels[meta.LabelCocoonSet]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for changed := range b.watchers[id] {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
}

func (b *Backend) getSet(ctx context.Context, id string) (*cocoonv1.CocoonSet, error) {
	u, err := b.sets.Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, kubeError(err, "get cocoonset")
	}
	return decode(u)
}

func (b *Backend) createSet(ctx context.Context, cs *cocoonv1.CocoonSet) (*cocoonv1.CocoonSet, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cs)
	if err != nil {
		return nil, fmt.Errorf("encode cocoonset: %w", err)
	}
	u, err := b.sets.Create(ctx, &unstructured.Unstructured{Object: raw}, metav1.CreateOptions{})
	if err != nil {
		return nil, kubeError(err, "create cocoonset")
	}
	return decode(u)
}

func (b *Backend) patchSet(ctx context.Context, id string, patch []byte) (*cocoonv1.CocoonSet, error) {
	u, err := b.sets.Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, kubeError(err, "patch cocoonset")
	}
	return decode(u)
}

func (b *Backend) cachedSet(id string) (*cocoonv1.CocoonSet, error) {
	obj, err := b.setLister.Get(id)
	if err != nil {
		return nil, kubeError(err, "read cocoonset cache")
	}
	return decode(obj)
}

func (b *Backend) mainPod(id string) *corev1.Pod {
	pod, err := b.podLister.Get(mainPodName(id))
	if err != nil || pod.DeletionTimestamp != nil {
		return nil
	}
	return pod
}

func send(ctx context.Context, out chan<- machines.Machine, m machines.Machine) bool {
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func decode(obj runtime.Object) (*cocoonv1.CocoonSet, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("decode cocoonset: unexpected %T", obj)
	}
	var cs cocoonv1.CocoonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cs); err != nil {
		return nil, fmt.Errorf("decode cocoonset %s: %w", u.GetName(), err)
	}
	return &cs, nil
}

func kubeError(err error, action string) error {
	switch {
	case apierrors.IsNotFound(err):
		return machines.ErrNotFound
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%s: %w", action, machines.ErrConflict)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
