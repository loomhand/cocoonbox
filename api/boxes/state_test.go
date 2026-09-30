package boxes

import (
	"regexp"
	"testing"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/CMGS/cocoonbox/api/config"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

func TestBoxID(t *testing.T) {
	id := BoxID("alice")
	if !regexp.MustCompile(`^cb-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("BoxID(alice) = %q, want cb-<12 hex>", id)
	}
	if id != "cb-2bd806c97f0e" {
		t.Errorf("BoxID(alice) = %q, want cb-2bd806c97f0e", id)
	}
	if BoxID("alice") != id || BoxID("bob") == id {
		t.Error("BoxID must be deterministic and differ per user")
	}
}

func TestObserve(t *testing.T) {
	tests := []struct {
		name       string
		set        *cocoonv1.CocoonSet
		pod        *corev1.Pod
		gen        int64
		wantState  controlv1.BoxState
		wantReason string
	}{
		{name: "no pod yet", set: testSet(1, false, cocoonv1.CocoonSetPhasePending), wantState: controlv1.BoxState_BOX_STATE_STARTING},
		{name: "pod creating", set: testSet(1, false, cocoonv1.CocoonSetPhasePending), pod: testPod(false, meta.LifecycleStateCreating, 1), wantState: controlv1.BoxState_BOX_STATE_STARTING},
		{name: "ready", set: testSet(1, false, cocoonv1.CocoonSetPhaseRunning), pod: testPod(true, meta.LifecycleStateReady, 1), wantState: controlv1.BoxState_BOX_STATE_RUNNING},
		{name: "ready but lifecycle predates operation", set: testSet(2, false, cocoonv1.CocoonSetPhaseRunning), pod: testPod(true, meta.LifecycleStateReady, 1), gen: 2, wantState: controlv1.BoxState_BOX_STATE_STARTING},
		{name: "ready but hibernate annotation still set", set: testSet(3, false, cocoonv1.CocoonSetPhaseSuspended), pod: withHibernate(testPod(true, meta.LifecycleStateReady, 3)), wantState: controlv1.BoxState_BOX_STATE_STARTING},
		{name: "suspending", set: testSet(2, true, cocoonv1.CocoonSetPhaseSuspending), pod: testPod(true, meta.LifecycleStateReady, 1), wantState: controlv1.BoxState_BOX_STATE_HIBERNATING},
		{name: "suspended", set: testSet(2, true, cocoonv1.CocoonSetPhaseSuspended), pod: testPod(false, meta.LifecycleStateHibernated, 2), wantState: controlv1.BoxState_BOX_STATE_HIBERNATED},
		{name: "suspended status is stale", set: staleStatus(testSet(4, true, cocoonv1.CocoonSetPhaseSuspended)), wantState: controlv1.BoxState_BOX_STATE_HIBERNATING},
		{name: "suspend timed out", set: testSet(2, true, cocoonv1.CocoonSetPhaseFailed), pod: testPod(false, meta.LifecycleStateHibernating, 2), wantState: controlv1.BoxState_BOX_STATE_FAILED, wantReason: "hibernate did not complete"},
		{name: "vm failed", set: testSet(1, false, cocoonv1.CocoonSetPhaseFailed), pod: withMessage(testPod(false, meta.LifecycleStateFailed, 1), "clone: snapshot not found"), wantState: controlv1.BoxState_BOX_STATE_FAILED, wantReason: "clone: snapshot not found"},
		{name: "failed lifecycle from an older generation", set: testSet(2, false, cocoonv1.CocoonSetPhasePending), pod: testPod(false, meta.LifecycleStateFailed, 1), gen: 2, wantState: controlv1.BoxState_BOX_STATE_STARTING},
		{name: "pod phase failed", set: testSet(1, false, cocoonv1.CocoonSetPhaseFailed), pod: withPhase(testPod(false, "", 1), corev1.PodFailed), wantState: controlv1.BoxState_BOX_STATE_FAILED, wantReason: "vm failed"},
		{name: "stale failed set status", set: staleStatus(testSet(3, false, cocoonv1.CocoonSetPhaseFailed)), wantState: controlv1.BoxState_BOX_STATE_STARTING},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := observe(tt.set, tt.pod, tt.gen)
			if got.state != tt.wantState || got.reason != tt.wantReason {
				t.Errorf("observe = (%s, %q), want (%s, %q)", got.state, got.reason, tt.wantState, tt.wantReason)
			}
			if got.detail == "" {
				t.Error("observe detail is empty")
			}
		})
	}
}

func TestDescribeUnschedulable(t *testing.T) {
	pod := testPod(false, "", 0)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Message: "0/2 nodes are available: 1 Insufficient ephemeral-storage",
	}}
	got := describe(testSet(1, false, cocoonv1.CocoonSetPhasePending), pod)
	if want := "cocoonset=Pending pod=Pending ready=false vm=-; unschedulable: 0/2 nodes are available: 1 Insufficient ephemeral-storage"; got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}
}

func TestBuildCocoonSet(t *testing.T) {
	cfg := &config.Config{Namespace: "cocoonbox", Image: "cocoonbox-desktop-v1", Network: "cb", ProbePort: 22}
	flavor := &controlv1.Flavor{Vcpus: 4, MemoryMib: 8192, DiskGib: 40}
	cs := buildCocoonSet(cfg, "cb-2bd806c97f0e", "alice", flavor)

	agent := cs.Spec.Agent
	if cs.Name != "cb-2bd806c97f0e" || cs.Namespace != "cocoonbox" || cs.Annotations[annotationUser] != "alice" {
		t.Errorf("metadata = %s/%s user %q", cs.Namespace, cs.Name, cs.Annotations[annotationUser])
	}
	if agent.Image != "cocoonbox-desktop-v1" || agent.Mode != cocoonv1.AgentModeClone || agent.Network != "cb" || agent.ProbePort != 22 {
		t.Errorf("agent = %+v", agent)
	}
	if got := agent.Resources.Requests.Memory().String(); got != "8Gi" {
		t.Errorf("memory request = %s, want 8Gi", got)
	}
	if got := agent.Resources.Limits.Cpu().String(); got != "4" {
		t.Errorf("cpu limit = %s, want 4", got)
	}
	if got := agent.Storage.String(); got != "40Gi" {
		t.Errorf("storage = %s, want 40Gi", got)
	}
	if got := flavorOf(cs); !sameFlavor(got, flavor) {
		t.Errorf("flavorOf round trip = %v, want %v", got, flavor)
	}
}

func TestResolveFlavor(t *testing.T) {
	def := config.Flavor{VCPUs: 4, MemoryMiB: 8192, DiskGiB: 40}
	if got := resolveFlavor(nil, def); !sameFlavor(got, &controlv1.Flavor{Vcpus: 4, MemoryMib: 8192, DiskGib: 40}) {
		t.Errorf("resolveFlavor(nil) = %v", got)
	}
	if got := resolveFlavor(&controlv1.Flavor{Vcpus: 2}, def); !sameFlavor(got, &controlv1.Flavor{Vcpus: 2, MemoryMib: 8192, DiskGib: 40}) {
		t.Errorf("resolveFlavor(vcpus=2) = %v", got)
	}
}

func testSet(gen int64, suspend bool, phase cocoonv1.CocoonSetPhase) *cocoonv1.CocoonSet {
	return &cocoonv1.CocoonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cb-2bd806c97f0e", Namespace: "cocoonbox", Generation: gen},
		Spec:       cocoonv1.CocoonSetSpec{Suspend: suspend},
		Status:     cocoonv1.CocoonSetStatus{ObservedGeneration: gen, Phase: phase},
	}
}

func staleStatus(cs *cocoonv1.CocoonSet) *cocoonv1.CocoonSet {
	cs.Status.ObservedGeneration = cs.Generation - 1
	return cs
}

func testPod(ready bool, state meta.LifecycleState, observedGen int64) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cb-2bd806c97f0e-0",
			Namespace: "cocoonbox",
			Labels:    map[string]string{meta.LabelCocoonSet: "cb-2bd806c97f0e"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.203.0.10"},
	}
	if state != "" {
		meta.LifecycleStatus{State: state, ObservedGeneration: observedGen}.Apply(pod)
	}
	if ready {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func withHibernate(pod *corev1.Pod) *corev1.Pod {
	meta.HibernateState(true).Apply(pod)
	return pod
}

func withMessage(pod *corev1.Pod, msg string) *corev1.Pod {
	pod.Annotations[meta.AnnotationLifecycleStateMessage] = msg
	return pod
}

func withPhase(pod *corev1.Pod, phase corev1.PodPhase) *corev1.Pod {
	pod.Status.Phase = phase
	return pod
}
