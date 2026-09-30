package cocoonset

import (
	"cmp"
	"fmt"
	"slices"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/CMGS/cocoonbox/api/machines"
)

const (
	mib = 1 << 20
	gib = 1 << 30

	annotationOwner = "cocoonbox.example.com/user"
	labelManagedBy  = "app.kubernetes.io/managed-by"
	managedBy       = "cocoonbox-api"
)

// observe maps a CocoonSet and its main pod to a machine; gen is the oldest generation the caller accepts as current.
func observe(cs *cocoonv1.CocoonSet, main *corev1.Pod, gen int64) machines.Machine {
	m := machines.Machine{
		ID:        cs.Name,
		Owner:     cs.Annotations[annotationOwner],
		State:     machines.StateStarting,
		Image:     cs.Spec.Agent.Image,
		Flavor:    flavorOf(cs),
		Detail:    describe(cs, main),
		CreatedAt: cs.CreationTimestamp.Time,
		Revision:  cs.Generation,
	}
	statusCurrent := cs.Status.ObservedGeneration >= max(cs.Generation, gen)
	setFailed := statusCurrent && cs.Status.Phase == cocoonv1.CocoonSetPhaseFailed
	switch {
	case cs.Spec.Suspend && setFailed:
		m.State, m.Reason = machines.StateFailed, failureReason(main, "hibernate did not complete")
	case cs.Spec.Suspend && statusCurrent && cs.Status.Phase == cocoonv1.CocoonSetPhaseSuspended:
		m.State = machines.StateHibernated
	case cs.Spec.Suspend:
		m.State = machines.StateHibernating
	case main != nil && podFailed(main, gen):
		m.State, m.Reason = machines.StateFailed, failureReason(main, "vm failed")
	case main != nil && podRunning(main, gen):
		m.State = machines.StateRunning
	case setFailed:
		m.State, m.Reason = machines.StateFailed, failureReason(main, "cocoonset failed")
	}
	return m
}

func podFailed(pod *corev1.Pod, gen int64) bool {
	lc := meta.ReadLifecycleStatus(pod)
	return meta.IsPodTerminal(pod) || (lc.State == meta.LifecycleStateFailed && lc.ObservedGeneration >= gen)
}

func podRunning(pod *corev1.Pod, gen int64) bool {
	lc := meta.ReadLifecycleStatus(pod)
	return meta.IsPodReady(pod) && !bool(meta.ReadHibernateState(pod)) &&
		lc.State == meta.LifecycleStateReady && lc.ObservedGeneration >= gen
}

func failureReason(pod *corev1.Pod, fallback string) string {
	if pod == nil {
		return fallback
	}
	return cmp.Or(pod.Annotations[meta.AnnotationLifecycleStateMessage], pod.Status.Message, pod.Status.Reason, fallback)
}

func describe(cs *cocoonv1.CocoonSet, main *corev1.Pod) string {
	setPhase := cmp.Or(string(cs.Status.Phase), "-")
	if main == nil {
		return fmt.Sprintf("cocoonset=%s pod=absent", setPhase)
	}
	lc := meta.ReadLifecycleStatus(main)
	detail := fmt.Sprintf("cocoonset=%s pod=%s ready=%t vm=%s", setPhase, cmp.Or(string(main.Status.Phase), "-"),
		meta.IsPodReady(main), cmp.Or(string(lc.State), "-"))
	if lc.Message != "" {
		detail += ": " + lc.Message
	}
	if i := slices.IndexFunc(main.Status.Conditions, unschedulable); i >= 0 {
		detail += "; unschedulable: " + main.Status.Conditions[i].Message
	}
	return detail
}

func unschedulable(c corev1.PodCondition) bool {
	return c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse
}

func flavorOf(cs *cocoonv1.CocoonSet) machines.Flavor {
	req := cs.Spec.Agent.Resources.Requests
	f := machines.Flavor{
		VCPUs:     uint32(req.Cpu().Value()),          //nolint:gosec // bounded by the flavor this backend wrote
		MemoryMiB: uint32(req.Memory().Value() / mib), //nolint:gosec // bounded by the flavor this backend wrote
	}
	if cs.Spec.Agent.Storage != nil {
		f.DiskGiB = uint32(cs.Spec.Agent.Storage.Value() / gib) //nolint:gosec // bounded by the flavor this backend wrote
	}
	return f
}

func buildCocoonSet(opts Options, spec machines.Spec) *cocoonv1.CocoonSet {
	resources := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(spec.Flavor.VCPUs), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(spec.Flavor.MemoryMiB)*mib, resource.BinarySI),
	}
	return &cocoonv1.CocoonSet{
		APIVersion:  cocoonv1.GroupVersion.String(),
		Kind:        meta.KindCocoonSet,
		Name:        spec.ID,
		Namespace:   opts.Namespace,
		Labels:      map[string]string{labelManagedBy: managedBy},
		Annotations: map[string]string{annotationOwner: spec.Owner},
		Spec: cocoonv1.CocoonSetSpec{
			SnapshotPolicy: cocoonv1.SnapshotPolicyNever,
			Agent: cocoonv1.AgentSpec{
				Image:     opts.Image,
				Mode:      cocoonv1.AgentModeClone,
				Network:   opts.Network,
				ProbePort: opts.ProbePort,
				Storage:   resource.NewQuantity(int64(spec.Flavor.DiskGiB)*gib, resource.BinarySI),
				Resources: corev1.ResourceRequirements{Requests: resources, Limits: resources.DeepCopy()},
			},
		},
	}
}

func mainPodName(id string) string {
	return id + "-0"
}
