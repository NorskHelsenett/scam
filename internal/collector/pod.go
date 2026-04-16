package collector

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	coreinformers "k8s.io/client-go/informers/core/v1"
)

// DumpPods emits the initial sorted snapshot for all pods.
func DumpPods(inf coreinformers.PodInformer) {
	pods, err := inf.Lister().List(labels.Everything())
	if err != nil {
		Log.Error("list pods", "err", err)
		return
	}
	DumpSorted("Pod", pods, lessPod, func(p *corev1.Pod) int { return EmitPod("INITIAL", p, nil) })
}

func lessPod(a, b *corev1.Pod) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	ak, an := PodOwner(a)
	bk, bn := PodOwner(b)
	if ak != bk {
		return ak < bk
	}
	if an != bn {
		return an < bn
	}
	return a.Name < b.Name
}

// PodOwner returns the (kind, name) of the pod's controlling owner.
// When the direct owner is a ReplicaSet, it resolves one level up to
// the Deployment so the name matches the backend Service name used in
// Ingress rules rather than the ephemeral ReplicaSet hash suffix.
func PodOwner(p *corev1.Pod) (string, string) {
	kind, name := "-", "-"
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			kind, name = o.Kind, o.Name
			break
		}
	}
	if kind == "-" && len(p.OwnerReferences) > 0 {
		kind, name = p.OwnerReferences[0].Kind, p.OwnerReferences[0].Name
	}
	// Resolve ReplicaSet → Deployment so the owner name matches the
	// Deployment (and therefore the backend Service name used in Ingress
	// rules) rather than the ephemeral ReplicaSet with its hash suffix.
	if kind == "ReplicaSet" && Refs.ReplicaSets != nil {
		if rs, err := Refs.ReplicaSets.Lister().ReplicaSets(p.Namespace).Get(name); err == nil {
			for _, o := range rs.OwnerReferences {
				if o.Controller != nil && *o.Controller {
					kind, name = o.Kind, o.Name
					break
				}
			}
		}
	}
	return kind, name
}

type containerRef struct {
	kind string // init | main | ephemeral
	name string
	spec string // image as-specified in PodSpec
	id   string // imageID as resolved in status
}

func collectContainers(p *corev1.Pod) []containerRef {
	n := len(p.Spec.InitContainers) + len(p.Spec.Containers) + len(p.Spec.EphemeralContainers)
	out := make([]containerRef, 0, n)
	for _, c := range p.Spec.InitContainers {
		out = append(out, containerRef{"init", c.Name, c.Image, statusImageID(p.Status.InitContainerStatuses, c.Name)})
	}
	for _, c := range p.Spec.Containers {
		out = append(out, containerRef{"main", c.Name, c.Image, statusImageID(p.Status.ContainerStatuses, c.Name)})
	}
	for _, c := range p.Spec.EphemeralContainers {
		out = append(out, containerRef{"ephemeral", c.Name, c.Image, statusImageID(p.Status.EphemeralContainerStatuses, c.Name)})
	}
	return out
}

func statusImageID(list []corev1.ContainerStatus, name string) string {
	for i := range list {
		if list[i].Name == name {
			return list[i].ImageID
		}
	}
	return ""
}

// SamePodImages reports whether two Pods' image signatures are identical.
func SamePodImages(a, b *corev1.Pod) bool {
	if a.Status.Phase != b.Status.Phase {
		return false
	}
	if len(a.Spec.InitContainers) != len(b.Spec.InitContainers) ||
		len(a.Spec.Containers) != len(b.Spec.Containers) ||
		len(a.Spec.EphemeralContainers) != len(b.Spec.EphemeralContainers) {
		return false
	}
	for i := range a.Spec.InitContainers {
		if a.Spec.InitContainers[i].Name != b.Spec.InitContainers[i].Name ||
			a.Spec.InitContainers[i].Image != b.Spec.InitContainers[i].Image {
			return false
		}
	}
	for i := range a.Spec.Containers {
		if a.Spec.Containers[i].Name != b.Spec.Containers[i].Name ||
			a.Spec.Containers[i].Image != b.Spec.Containers[i].Image {
			return false
		}
	}
	for i := range a.Spec.EphemeralContainers {
		if a.Spec.EphemeralContainers[i].Name != b.Spec.EphemeralContainers[i].Name ||
			a.Spec.EphemeralContainers[i].Image != b.Spec.EphemeralContainers[i].Image {
			return false
		}
	}
	return sameImageIDs(a.Status.InitContainerStatuses, b.Status.InitContainerStatuses) &&
		sameImageIDs(a.Status.ContainerStatuses, b.Status.ContainerStatuses) &&
		sameImageIDs(a.Status.EphemeralContainerStatuses, b.Status.EphemeralContainerStatuses)
}

func sameImageIDs(a, b []corev1.ContainerStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		id := ""
		for j := range b {
			if a[i].Name == b[j].Name {
				id = b[j].ImageID
				break
			}
		}
		if a[i].ImageID != id {
			return false
		}
	}
	return true
}

// EmitPod logs one line per container and returns how many it emitted.
func EmitPod(event string, p, oldP *corev1.Pod) int {
	if oldP != nil && SamePodImages(oldP, p) {
		return 0
	}
	ok, on := PodOwner(p)
	cur := collectContainers(p)
	phaseChanged := oldP != nil && oldP.Status.Phase != p.Status.Phase
	var prev map[string]containerRef
	if oldP != nil && !phaseChanged {
		old := collectContainers(oldP)
		prev = make(map[string]containerRef, len(old))
		for _, c := range old {
			prev[c.kind+"/"+c.name] = c
		}
	}
	emitted := 0
	for _, c := range cur {
		if prev != nil {
			if q, found := prev[c.kind+"/"+c.name]; found && q.spec == c.spec && q.id == c.id {
				continue
			}
		}
		fullRepo, tag, digest := SplitImage(c.spec, c.id)
		image, registry := SplitImageName(fullRepo)
		Log.Info(event,
			"kind", "Container",
			"namespace", p.Namespace,
			"pod_uid", string(p.UID),
			"pod_phase", string(p.Status.Phase),
			"owner_kind", ok,
			"owner", on,
			"pod", p.Name,
			"pod_labels", p.Labels,
			"container_kind", c.kind,
			"container", c.name,
			"registry", registry,
			"image", image,
			"tag", tag,
			"digest", digest,
			"image_spec", c.spec,
			"image_id", c.id,
		)
		emitted++
	}
	return emitted
}

// EmitPodDelete emits one Container DELETE per container in the pod.
func EmitPodDelete(p *corev1.Pod) {
	ok, on := PodOwner(p)
	uid := string(p.UID)
	for _, c := range p.Spec.InitContainers {
		emitContainerDelete(p.Namespace, uid, ok, on, p.Name, "init", c.Name)
	}
	for _, c := range p.Spec.Containers {
		emitContainerDelete(p.Namespace, uid, ok, on, p.Name, "main", c.Name)
	}
	for _, c := range p.Spec.EphemeralContainers {
		emitContainerDelete(p.Namespace, uid, ok, on, p.Name, "ephemeral", c.Name)
	}
}

func emitContainerDelete(ns, podUID, ownerKind, ownerName, podName, ckind, cname string) {
	Log.Info("DELETE",
		"kind", "Container",
		"namespace", ns,
		"pod_uid", podUID,
		"owner_kind", ownerKind,
		"owner", ownerName,
		"pod", podName,
		"container_kind", ckind,
		"container", cname,
	)
}
