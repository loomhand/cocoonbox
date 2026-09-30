package boxes

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/CMGS/cocoonbox/api/config"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
)

const (
	idPrefix = "cb-"
	idHexLen = 12

	mib = 1 << 20
	gib = 1 << 30

	annotationUser = "cocoonbox.example.com/user"
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "cocoonbox-api"
)

// BoxID returns the id of user's box, which is also its CocoonSet name.
func BoxID(user string) string {
	sum := sha256.Sum256([]byte(user))
	return idPrefix + hex.EncodeToString(sum[:])[:idHexLen]
}

type observation struct {
	state  controlv1.BoxState
	detail string
	reason string
}

// observe maps a CocoonSet and its main pod to a box state; gen is the oldest generation the caller accepts as current.
func observe(cs *cocoonv1.CocoonSet, main *corev1.Pod, gen int64) observation {
	obs := observation{state: controlv1.BoxState_BOX_STATE_STARTING, detail: describe(cs, main)}
	statusCurrent := cs.Status.ObservedGeneration >= max(cs.Generation, gen)
	setFailed := statusCurrent && cs.Status.Phase == cocoonv1.CocoonSetPhaseFailed
	switch {
	case cs.Spec.Suspend && setFailed:
		obs.state, obs.reason = controlv1.BoxState_BOX_STATE_FAILED, failureReason(main, "hibernate did not complete")
	case cs.Spec.Suspend && statusCurrent && cs.Status.Phase == cocoonv1.CocoonSetPhaseSuspended:
		obs.state = controlv1.BoxState_BOX_STATE_HIBERNATED
	case cs.Spec.Suspend:
		obs.state = controlv1.BoxState_BOX_STATE_HIBERNATING
	case main != nil && podFailed(main, gen):
		obs.state, obs.reason = controlv1.BoxState_BOX_STATE_FAILED, failureReason(main, "vm failed")
	case main != nil && podRunning(main, gen):
		obs.state = controlv1.BoxState_BOX_STATE_RUNNING
	case setFailed:
		obs.state, obs.reason = controlv1.BoxState_BOX_STATE_FAILED, failureReason(main, "cocoonset failed")
	}
	return obs
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
	return detail
}

func toBox(cs *cocoonv1.CocoonSet, main *corev1.Pod) *controlv1.Box {
	obs := observe(cs, main, 0)
	return &controlv1.Box{
		Id:            cs.Name,
		State:         obs.state,
		Image:         cs.Spec.Agent.Image,
		Flavor:        flavorOf(cs),
		FailureReason: obs.reason,
		CreatedAtMs:   cs.CreationTimestamp.UnixMilli(),
	}
}

func flavorOf(cs *cocoonv1.CocoonSet) *controlv1.Flavor {
	req := cs.Spec.Agent.Resources.Requests
	f := &controlv1.Flavor{
		Vcpus:     uint32(req.Cpu().Value()),          //nolint:gosec // bounded by the flavor this service wrote
		MemoryMib: uint32(req.Memory().Value() / mib), //nolint:gosec // bounded by the flavor this service wrote
	}
	if cs.Spec.Agent.Storage != nil {
		f.DiskGib = uint32(cs.Spec.Agent.Storage.Value() / gib) //nolint:gosec // bounded by the flavor this service wrote
	}
	return f
}

func resolveFlavor(req *controlv1.Flavor, def config.Flavor) *controlv1.Flavor {
	return &controlv1.Flavor{
		Vcpus:     cmp.Or(req.GetVcpus(), def.VCPUs),
		MemoryMib: cmp.Or(req.GetMemoryMib(), def.MemoryMiB),
		DiskGib:   cmp.Or(req.GetDiskGib(), def.DiskGiB),
	}
}

func sameFlavor(a, b *controlv1.Flavor) bool {
	return a.GetVcpus() == b.GetVcpus() && a.GetMemoryMib() == b.GetMemoryMib() && a.GetDiskGib() == b.GetDiskGib()
}

func buildCocoonSet(cfg *config.Config, id, user string, flavor *controlv1.Flavor) *cocoonv1.CocoonSet {
	resources := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(flavor.GetVcpus()), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(flavor.GetMemoryMib())*mib, resource.BinarySI),
	}
	return &cocoonv1.CocoonSet{
		APIVersion:  cocoonv1.GroupVersion.String(),
		Kind:        meta.KindCocoonSet,
		Name:        id,
		Namespace:   cfg.Namespace,
		Labels:      map[string]string{labelManagedBy: managedBy},
		Annotations: map[string]string{annotationUser: user},
		Spec: cocoonv1.CocoonSetSpec{
			Agent: cocoonv1.AgentSpec{
				Image:     cfg.Image,
				Mode:      cocoonv1.AgentModeClone,
				Network:   cfg.Network,
				ProbePort: cfg.ProbePort,
				Storage:   resource.NewQuantity(int64(flavor.GetDiskGib())*gib, resource.BinarySI),
				Resources: corev1.ResourceRequirements{Requests: resources, Limits: resources.DeepCopy()},
			},
		},
	}
}

func mainPodName(id string) string {
	return id + "-0"
}
