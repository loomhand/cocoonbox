package cocoonset

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/CMGS/cocoonbox/api/machines"
)

const boxID = "cb-2bd806c97f0e"

func TestEnsure(t *testing.T) {
	env := newTestEnv(t)
	spec := machines.Spec{ID: boxID, Owner: "alice", Flavor: machines.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}}

	m, action, err := env.backend.Ensure(t.Context(), spec)
	if err != nil || action != machines.ActionCreated {
		t.Fatalf("Ensure absent = %v, %v", action, err)
	}
	if m.ID != boxID || m.Owner != "alice" || m.State != machines.StateStarting || m.Flavor != spec.Flavor || m.Image != "cocoonbox-desktop-v2" {
		t.Errorf("created machine = %+v", m)
	}
	if _, action, err = env.backend.Ensure(t.Context(), spec); err != nil || action != machines.ActionNone {
		t.Errorf("Ensure existing = %v, %v, want ActionNone", action, err)
	}

	env.putSet(t, testSet(2, true, cocoonv1.CocoonSetPhaseSuspended))
	if _, action, err = env.backend.Ensure(t.Context(), spec); err != nil || action != machines.ActionWoken {
		t.Errorf("Ensure hibernated = %v, %v, want ActionWoken", action, err)
	}
	if env.set(t).Spec.Suspend {
		t.Error("Ensure did not clear spec.suspend")
	}
}

func TestImageOverrideAndDelete(t *testing.T) {
	env := newTestEnv(t)
	spec := machines.Spec{ID: boxID + "-r1", Owner: "alice", Flavor: machines.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}, Image: "cocoonbox-desktop-v7"}
	m, _, err := env.backend.Ensure(t.Context(), spec)
	if err != nil || m.Image != "cocoonbox-desktop-v7" {
		t.Fatalf("Ensure with an image = %+v, %v", m, err)
	}
	if err := env.backend.Delete(t.Context(), spec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := env.backend.Get(t.Context(), spec.ID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Get after Delete: %v, want ErrNotFound", err)
	}
	if err := env.backend.Delete(t.Context(), spec.ID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("second Delete: %v, want ErrNotFound", err)
	}
}

func TestMetaIncarnationAndExec(t *testing.T) {
	env := newTestEnv(t)
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhaseRunning))
	identity := machines.Identity{BoxID: boxID, WakeID: "w1", KeyID: "k1", PublicKey: "pub"}
	if err := env.backend.Deliver(t.Context(), boxID, identity, ""); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Deliver without a pod: %v, want ErrNotFound", err)
	}
	pod := testPod(true, meta.LifecycleStateReady, 1)
	pod.UID = "uid-1"
	meta.VMRuntime{VMID: "vm-1", IP: "10.203.0.10"}.Apply(pod)
	env.putPod(t, pod)
	env.waitAddress(t)

	if err := env.backend.SetMeta(t.Context(), boxID, "wake", "uid-1/vm-1:w1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	m, err := env.backend.Get(t.Context(), boxID)
	if err != nil || m.Meta["wake"] != "uid-1/vm-1:w1" || m.Incarnation != "uid-1/vm-1" {
		t.Errorf("Get = meta %v incarnation %q, %v", m.Meta, m.Incarnation, err)
	}
	if err := env.backend.Deliver(t.Context(), boxID, identity, "k3y"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(env.execs) != 2 || env.execs[0].pod != boxID+"-0" || env.execs[0].stdin != "k3y\n" || !strings.Contains(env.execs[0].cmd[2], "set-agent-env") {
		t.Fatalf("gw key exec = %+v", env.execs)
	}
	if env.execs[1].stdin != `{"box_id":"`+boxID+`","wake_id":"w1","key_id":"k1","public_key":"pub"}` || !strings.Contains(env.execs[1].cmd[2], "mv -f") {
		t.Errorf("identity exec = %+v", env.execs[1])
	}
	env.execs = nil
	if err := env.backend.Deliver(t.Context(), boxID, identity, ""); err != nil || len(env.execs) != 1 {
		t.Errorf("Deliver without a gw key ran %d execs, %v", len(env.execs), err)
	}
}

func TestHibernate(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.backend.Hibernate(t.Context(), boxID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Hibernate absent: %v, want ErrNotFound", err)
	}
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhaseRunning))
	m, err := env.backend.Hibernate(t.Context(), boxID)
	if err != nil || m.State != machines.StateHibernating {
		t.Errorf("Hibernate = %+v, %v", m, err)
	}
	if !env.set(t).Spec.Suspend {
		t.Error("Hibernate did not set spec.suspend")
	}
}

func TestGetAndList(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.backend.Get(t.Context(), boxID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Get absent: %v, want ErrNotFound", err)
	}
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhaseRunning))
	env.putPod(t, testPod(true, meta.LifecycleStateReady, 1))
	env.waitAddress(t)

	m, err := env.backend.Get(t.Context(), boxID)
	if err != nil || m.State != machines.StateRunning {
		t.Errorf("Get = %+v, %v", m, err)
	}
	if list, err := env.backend.List(t.Context(), "alice"); err != nil || len(list) != 1 || list[0].ID != boxID {
		t.Errorf("List(alice) = %+v, %v", list, err)
	}
	if list, err := env.backend.List(t.Context(), "bob"); err != nil || len(list) != 0 {
		t.Errorf("List(bob) = %+v, %v", list, err)
	}
}

func TestWatch(t *testing.T) {
	env := newTestEnv(t)
	env.putSet(t, testSet(1, true, cocoonv1.CocoonSetPhaseSuspending))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	watch := env.backend.Watch(ctx, boxID, 2)

	env.putSet(t, testSet(2, true, cocoonv1.CocoonSetPhaseSuspending))
	until(t, watch, machines.StateHibernating)
	env.putSet(t, testSet(2, true, cocoonv1.CocoonSetPhaseSuspended))
	until(t, watch, machines.StateHibernated)
	if err := env.dyn.Resource(Resource).Namespace("cocoonbox").Delete(t.Context(), boxID, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	until(t, watch, machines.StateGone)
	if _, open := <-watch; open {
		t.Error("watch stayed open after the machine was gone")
	}
}

func TestWatchReportsAMetaChange(t *testing.T) {
	env := newTestEnv(t)
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhaseRunning))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	watch := env.backend.Watch(ctx, boxID, 1)
	<-watch
	if err := env.backend.SetMeta(t.Context(), boxID, "wake", "inc|w1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	for m := range watch {
		if m.Meta["wake"] == "inc|w1" {
			return
		}
	}
	t.Error("watch sent nothing after the meta changed")
}

func TestAddress(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.backend.Address(t.Context(), boxID); !errors.Is(err, machines.ErrNotFound) {
		t.Errorf("Address absent: %v, want ErrNotFound", err)
	}
	env.putSet(t, testSet(1, false, cocoonv1.CocoonSetPhasePending))
	env.putPod(t, testPod(false, meta.LifecycleStateCreating, 1))
	env.waitPod(t)
	var notRunning *machines.NotRunningError
	if _, err := env.backend.Address(t.Context(), boxID); !errors.As(err, &notRunning) || notRunning.State != machines.StateStarting {
		t.Errorf("Address starting: %v, want NotRunningError{Starting}", err)
	}
	env.putPod(t, testPod(true, meta.LifecycleStateReady, 1))
	if addr := env.waitAddress(t); addr != "10.203.0.10:7443" {
		t.Errorf("Address = %q, want 10.203.0.10:7443", addr)
	}
}

type execCall struct {
	pod   string
	cmd   []string
	stdin string
}

type testEnv struct {
	backend *Backend
	dyn     *dynamicfake.FakeDynamicClient
	kube    *kubefake.Clientset
	execs   []execCall
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	opts := Options{Namespace: "cocoonbox", Image: "cocoonbox-desktop-v2", Network: "cb", ProbePort: 7443, AgentPort: 7443}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{Resource: "CocoonSetList"})
	kube := kubefake.NewClientset()
	env := &testEnv{dyn: dyn, kube: kube}
	backend, err := newBackend(t.Context(), opts, dyn, kube, func(_ context.Context, pod string, cmd []string, stdin []byte) ([]byte, error) {
		env.execs = append(env.execs, execCall{pod: pod, cmd: cmd, stdin: string(stdin)})
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatalf("newBackend: %v", err)
	}
	env.backend = backend
	return env
}

func (e *testEnv) set(t *testing.T) *cocoonv1.CocoonSet {
	t.Helper()
	cs, err := e.backend.getSet(t.Context(), boxID)
	if err != nil {
		t.Fatalf("get cocoonset: %v", err)
	}
	return cs
}

// putSet overwrites generation, spec.suspend and status of the stored CocoonSet, creating it when absent.
func (e *testEnv) putSet(t *testing.T, want *cocoonv1.CocoonSet) {
	t.Helper()
	cs, err := e.backend.getSet(t.Context(), boxID)
	if err != nil {
		cs = buildCocoonSet(e.backend.opts, machines.Spec{ID: boxID, Owner: "alice", Flavor: machines.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}})
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
	sets := e.dyn.Resource(Resource).Namespace("cocoonbox")
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

func (e *testEnv) waitPod(t *testing.T) {
	t.Helper()
	poll(t, func() bool { return e.backend.mainPod(boxID) != nil })
}

func (e *testEnv) waitAddress(t *testing.T) string {
	t.Helper()
	var addr string
	poll(t, func() bool {
		var err error
		var ep machines.Endpoint
		ep, err = e.backend.Address(t.Context(), boxID)
		addr = ep.Key
		return err == nil
	})
	return addr
}

func poll(t *testing.T, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatal("cache did not reach the expected state")
		case <-tick.C:
		}
	}
}

// until drains watch up to the first event in state, failing on any event older than revision 2.
func until(t *testing.T, watch <-chan machines.Machine, state machines.State) {
	t.Helper()
	for {
		select {
		case m, ok := <-watch:
			if !ok {
				t.Fatalf("watch closed before state %d", state)
			}
			if m.State != machines.StateGone && m.Revision < 2 {
				t.Fatalf("watch sent revision %d, below the requested 2", m.Revision)
			}
			if m.State == state {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no event in state %d", state)
		}
	}
}
