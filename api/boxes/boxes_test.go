package boxes

import (
	"context"
	"testing"
	"time"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/CMGS/cocoonbox/api/auth"
	"github.com/CMGS/cocoonbox/api/config"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	alice    = "alice"
	aliceBox = "cb-2bd806c97f0e"
)

func TestEnsureCreatesBox(t *testing.T) {
	env := newTestEnv(t)
	ctx := auth.WithUser(t.Context(), alice)

	resp, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if resp.GetBox().GetId() != aliceBox || resp.GetBox().GetState() != controlv1.BoxState_BOX_STATE_STARTING {
		t.Errorf("box = %v", resp.GetBox())
	}
	if resp.GetOperation().GetId() == "" || resp.GetOperation().GetBoxId() != aliceBox {
		t.Fatalf("operation = %v", resp.GetOperation())
	}
	cs := env.set(t)
	if cs.Spec.Agent.Image != "cocoonbox-desktop-v1" || cs.Spec.Agent.Mode != cocoonv1.AgentModeClone || cs.Annotations[annotationUser] != alice {
		t.Errorf("cocoonset = %+v", cs)
	}

	again, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if again.GetOperation().GetId() != resp.GetOperation().GetId() {
		t.Errorf("second Ensure started operation %s, want %s", again.GetOperation().GetId(), resp.GetOperation().GetId())
	}

	env.putPod(t, testPod(true, meta.LifecycleStateReady, 0))
	events := env.watch(t, ctx, resp.GetOperation().GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_DONE || last.GetBoxState() != controlv1.BoxState_BOX_STATE_RUNNING {
		t.Errorf("last event = %v, want DONE/RUNNING", last)
	}
	for i, ev := range events[:len(events)-1] {
		if ev.GetPhase() != controlv1.Phase_PHASE_CREATING {
			t.Errorf("event %d phase = %s, want CREATING", i, ev.GetPhase())
		}
	}

	running, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure running: %v", err)
	}
	if running.GetOperation() != nil || running.GetBox().GetState() != controlv1.BoxState_BOX_STATE_RUNNING {
		t.Errorf("Ensure on a running box = %v", running)
	}
}

func TestHibernateAndWake(t *testing.T) {
	env := newTestEnv(t)
	ctx := auth.WithUser(t.Context(), alice)
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhaseRunning))
	env.putPod(t, testPod(true, meta.LifecycleStateReady, 1))
	env.waitRunning(t)

	op, err := env.svc.Hibernate(ctx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if !env.set(t).Spec.Suspend {
		t.Fatal("Hibernate did not set spec.suspend")
	}
	env.putPod(t, withHibernate(testPod(false, meta.LifecycleStateHibernated, 2)))
	env.putSet(t, testSet(2, true, cocoonv1.CocoonSetPhaseSuspended))
	events := env.watch(t, ctx, op.GetId(), "")
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE || last.GetBoxState() != controlv1.BoxState_BOX_STATE_HIBERNATED {
		t.Errorf("last event = %v, want DONE/HIBERNATED", last)
	}
	resumed := env.watch(t, ctx, op.GetId(), events[0].GetOffsetKey())
	if len(resumed) != len(events)-1 || resumed[0].GetOffsetKey() != events[1].GetOffsetKey() {
		t.Errorf("resume after %s returned %d events, want %d", events[0].GetOffsetKey(), len(resumed), len(events)-1)
	}

	wake, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure wake: %v", err)
	}
	if env.set(t).Spec.Suspend {
		t.Fatal("Ensure did not clear spec.suspend")
	}
	env.putSet(t, testSet(3, false, cocoonv1.CocoonSetPhaseRunning))
	env.putPod(t, testPod(true, meta.LifecycleStateReady, 3))
	events = env.watch(t, ctx, wake.GetOperation().GetId(), "")
	if events[0].GetPhase() != controlv1.Phase_PHASE_WAKING {
		t.Errorf("first wake event phase = %s, want WAKING", events[0].GetPhase())
	}
	if last := events[len(events)-1]; last.GetPhase() != controlv1.Phase_PHASE_DONE || last.GetBoxState() != controlv1.BoxState_BOX_STATE_RUNNING {
		t.Errorf("last wake event = %v, want DONE/RUNNING", last)
	}
}

func TestOperationFails(t *testing.T) {
	env := newTestEnv(t)
	ctx := auth.WithUser(t.Context(), alice)
	resp, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	env.putPod(t, withMessage(testPod(false, meta.LifecycleStateFailed, 0), "clone: snapshot not found"))
	events := env.watch(t, ctx, resp.GetOperation().GetId(), "")
	last := events[len(events)-1]
	if last.GetPhase() != controlv1.Phase_PHASE_FAILED || last.GetDetail() != "clone: snapshot not found" {
		t.Errorf("last event = %v, want FAILED with the lifecycle message", last)
	}
	box, err := env.svc.Get(ctx, &controlv1.GetBoxRequest{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if box.GetState() != controlv1.BoxState_BOX_STATE_FAILED || box.GetFailureReason() != "clone: snapshot not found" {
		t.Errorf("box = %v", box)
	}
}

func TestOwnership(t *testing.T) {
	env := newTestEnv(t)
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhaseRunning))
	env.putPod(t, testPod(true, meta.LifecycleStateReady, 1))
	env.waitRunning(t)
	bob := auth.WithUser(t.Context(), "bob")

	if _, err := env.svc.Get(bob, &controlv1.GetBoxRequest{BoxId: aliceBox}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Get other's box: %v, want PermissionDenied", err)
	}
	if _, err := env.svc.Hibernate(bob, &controlv1.BoxRef{BoxId: aliceBox}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Hibernate other's box: %v, want PermissionDenied", err)
	}
	if _, err := env.svc.Resolve(bob, "bob", aliceBox); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Resolve other's box: %v, want PermissionDenied", err)
	}
	list, err := env.svc.List(bob, &controlv1.ListBoxesRequest{})
	if err != nil || len(list.GetBoxes()) != 0 {
		t.Errorf("List for bob = %v, %v, want empty", list, err)
	}
	if _, err := env.svc.Get(bob, &controlv1.GetBoxRequest{BoxId: BoxID("bob")}); status.Code(err) != codes.NotFound {
		t.Errorf("Get own absent box: %v, want NotFound", err)
	}
	if _, err := env.svc.Get(t.Context(), &controlv1.GetBoxRequest{BoxId: aliceBox}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Get without user: %v, want Unauthenticated", err)
	}

	aliceCtx := auth.WithUser(t.Context(), alice)
	resp, err := env.svc.Ensure(aliceCtx, &controlv1.EnsureRequest{})
	if err != nil {
		t.Fatalf("Ensure alice: %v", err)
	}
	op, err := env.svc.Hibernate(aliceCtx, &controlv1.BoxRef{BoxId: aliceBox})
	if err != nil {
		t.Fatalf("Hibernate alice: %v", err)
	}
	stream := &eventStream{ctx: bob}
	if err := env.svc.WatchOperation(&controlv1.WatchOperationRequest{OperationId: op.GetId()}, stream); status.Code(err) != codes.NotFound {
		t.Errorf("WatchOperation of other's operation: %v, want NotFound", err)
	}
	if resp.GetOperation() != nil {
		t.Errorf("Ensure on running box returned operation %v", resp.GetOperation())
	}
}

func TestEnsureFlavorMismatch(t *testing.T) {
	env := newTestEnv(t)
	ctx := auth.WithUser(t.Context(), alice)
	if _, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{Flavor: &controlv1.Flavor{Vcpus: 2}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Ensure with another flavor: %v, want FailedPrecondition", err)
	}
	if _, err := env.svc.Ensure(ctx, &controlv1.EnsureRequest{Flavor: &controlv1.Flavor{Vcpus: 4}}); err != nil {
		t.Errorf("Ensure with the default flavor spelled out: %v", err)
	}
}

func TestResolve(t *testing.T) {
	env := newTestEnv(t)
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhasePending))
	env.putPod(t, testPod(false, meta.LifecycleStateCreating, 1))
	env.waitPod(t, func(p *corev1.Pod) bool { return p != nil })
	if _, err := env.svc.Resolve(t.Context(), alice, aliceBox); status.Code(err) != codes.Unavailable {
		t.Errorf("Resolve starting box: %v, want Unavailable", err)
	}
	env.putPod(t, testPod(true, meta.LifecycleStateReady, 1))
	env.waitRunning(t)
	addr, err := env.svc.Resolve(t.Context(), alice, aliceBox)
	if err != nil || addr != "10.203.0.10:7443" {
		t.Errorf("Resolve = %q, %v, want 10.203.0.10:7443", addr, err)
	}
}

type testEnv struct {
	svc  *Service
	dyn  *dynamicfake.FakeDynamicClient
	kube *kubefake.Clientset
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := &config.Config{
		Namespace: "cocoonbox", Image: "cocoonbox-desktop-v1", Network: "cb", ProbePort: 22, AgentPort: 7443,
		Flavor: config.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40},
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{CocoonSetResource: "CocoonSetList"})
	kube := kubefake.NewClientset()
	svc, err := New(t.Context(), cfg, dyn, kube)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testEnv{svc: svc, dyn: dyn, kube: kube}
}

func (e *testEnv) set(t *testing.T) *cocoonv1.CocoonSet {
	t.Helper()
	cs, err := e.svc.getSet(t.Context(), aliceBox)
	if err != nil {
		t.Fatalf("get cocoonset: %v", err)
	}
	return cs
}

// putSet overwrites spec and status of the stored CocoonSet, creating it when absent.
func (e *testEnv) putSet(t *testing.T, want *cocoonv1.CocoonSet) {
	t.Helper()
	sets := e.dyn.Resource(CocoonSetResource).Namespace("cocoonbox")
	cs, err := e.svc.getSet(t.Context(), aliceBox)
	if err != nil {
		cs = buildCocoonSet(e.svc.cfg, aliceBox, alice, resolveFlavor(nil, e.svc.cfg.Flavor))
	}
	cs.Generation = want.Generation
	cs.Spec.Suspend = want.Spec.Suspend
	cs.Status = want.Status
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cs)
	if err != nil {
		t.Fatalf("encode cocoonset: %v", err)
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(cocoonv1.GroupVersion.WithKind(meta.KindCocoonSet))
	if _, err := sets.Update(t.Context(), u, metav1.UpdateOptions{}); err == nil {
		return
	}
	if _, err := sets.Create(t.Context(), u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("put cocoonset: %v", err)
	}
}

func (e *testEnv) putPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	pods := e.kube.CoreV1().Pods("cocoonbox")
	if _, err := pods.Update(t.Context(), pod, metav1.UpdateOptions{}); err == nil {
		return
	}
	if _, err := pods.Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("put pod: %v", err)
	}
}

func (e *testEnv) waitRunning(t *testing.T) {
	t.Helper()
	e.waitPod(t, func(*corev1.Pod) bool {
		cs, err := e.svc.cachedSet(aliceBox)
		return err == nil && observe(cs, e.svc.mainPod(aliceBox), 0).state == controlv1.BoxState_BOX_STATE_RUNNING
	})
}

func (e *testEnv) waitPod(t *testing.T, cond func(*corev1.Pod) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond(e.svc.mainPod(aliceBox)) {
		select {
		case <-ctx.Done():
			t.Fatal("cache did not reach the expected state")
		case <-tick.C:
		}
	}
}

func (e *testEnv) watch(t *testing.T, ctx context.Context, opID, from string) []*controlv1.OperationEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stream := &eventStream{ctx: ctx}
	if err := e.svc.WatchOperation(&controlv1.WatchOperationRequest{OperationId: opID, FromOffsetKey: from}, stream); err != nil {
		t.Fatalf("WatchOperation: %v (events so far %v)", err, stream.events)
	}
	if len(stream.events) == 0 {
		t.Fatal("WatchOperation returned no events")
	}
	return stream.events
}

type eventStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*controlv1.OperationEvent
}

func (s *eventStream) Context() context.Context { return s.ctx }

func (s *eventStream) Send(ev *controlv1.OperationEvent) error {
	s.events = append(s.events, ev)
	return nil
}
