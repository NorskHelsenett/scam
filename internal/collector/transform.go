package collector

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Transforms strip fields we never read before objects enter the informer
// cache. They mutate in place; the informer owns the object from here on.

func TrimPod(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	p.Annotations = nil
	p.ManagedFields = nil
	p.Finalizers = nil

	p.Spec.Volumes = nil
	p.Spec.NodeSelector = nil
	p.Spec.Tolerations = nil
	p.Spec.Affinity = nil
	p.Spec.TopologySpreadConstraints = nil
	p.Spec.SchedulingGates = nil
	p.Spec.ReadinessGates = nil
	p.Spec.ImagePullSecrets = nil
	p.Spec.HostAliases = nil

	for i := range p.Spec.InitContainers {
		stripContainer(&p.Spec.InitContainers[i])
	}
	for i := range p.Spec.Containers {
		stripContainer(&p.Spec.Containers[i])
	}
	for i := range p.Spec.EphemeralContainers {
		stripEphemeral(&p.Spec.EphemeralContainers[i].EphemeralContainerCommon)
	}

	p.Status.Conditions = nil
	p.Status.PodIPs = nil
	for i := range p.Status.ContainerStatuses {
		p.Status.ContainerStatuses[i].LastTerminationState = corev1.ContainerState{}
	}
	for i := range p.Status.InitContainerStatuses {
		p.Status.InitContainerStatuses[i].LastTerminationState = corev1.ContainerState{}
	}
	for i := range p.Status.EphemeralContainerStatuses {
		p.Status.EphemeralContainerStatuses[i].LastTerminationState = corev1.ContainerState{}
	}
	return p, nil
}

func TrimService(obj any) (any, error) {
	s, ok := obj.(*corev1.Service)
	if !ok {
		return obj, nil
	}
	s.Annotations = nil
	s.ManagedFields = nil
	s.Finalizers = nil
	s.Status.Conditions = nil
	return s, nil
}

func TrimIngress(obj any) (any, error) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		return obj, nil
	}
	i.Annotations = nil
	i.ManagedFields = nil
	i.Finalizers = nil
	return i, nil
}

// TrimReplicaSet strips everything except OwnerReferences — the only field
// PodOwner reads when resolving ReplicaSet → Deployment.
func TrimReplicaSet(obj any) (any, error) {
	rs, ok := obj.(*appsv1.ReplicaSet)
	if !ok {
		return obj, nil
	}
	rs.Annotations = nil
	rs.ManagedFields = nil
	rs.Finalizers = nil
	rs.Labels = nil
	rs.Spec = appsv1.ReplicaSetSpec{}
	rs.Status = appsv1.ReplicaSetStatus{}
	return rs, nil
}

// TrimEndpointSlice strips annotations/managedFields. We keep endpoints,
// ports, labels (for service_name linkage), and addressType.
func TrimEndpointSlice(obj any) (any, error) {
	es, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return obj, nil
	}
	es.Annotations = nil
	es.ManagedFields = nil
	es.Finalizers = nil
	return es, nil
}

// TrimMeta is a generic small transform for objects where we only read
// identity + a couple of spec fields (e.g., IngressClass, GatewayClass).
func TrimMeta(obj any) (any, error) {
	if o, ok := obj.(metav1.Object); ok {
		o.SetAnnotations(nil)
		o.SetManagedFields(nil)
		o.SetFinalizers(nil)
	}
	return obj, nil
}

func TrimUnstructured(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	u.SetAnnotations(nil)
	u.SetManagedFields(nil)
	u.SetFinalizers(nil)
	unstructured.RemoveNestedField(u.Object, "status", "conditions")
	unstructured.RemoveNestedField(u.Object, "status", "listeners")
	unstructured.RemoveNestedField(u.Object, "status", "parents")
	return u, nil
}

func stripContainer(c *corev1.Container) {
	c.Command = nil
	c.Args = nil
	c.Env = nil
	c.EnvFrom = nil
	c.VolumeMounts = nil
	c.VolumeDevices = nil
	c.Resources = corev1.ResourceRequirements{}
	c.LivenessProbe = nil
	c.ReadinessProbe = nil
	c.StartupProbe = nil
	c.Lifecycle = nil
	c.SecurityContext = nil
	c.Ports = nil
}

func stripEphemeral(c *corev1.EphemeralContainerCommon) {
	c.Command = nil
	c.Args = nil
	c.Env = nil
	c.EnvFrom = nil
	c.VolumeMounts = nil
	c.VolumeDevices = nil
	c.Resources = corev1.ResourceRequirements{}
	c.LivenessProbe = nil
	c.ReadinessProbe = nil
	c.StartupProbe = nil
	c.Lifecycle = nil
	c.SecurityContext = nil
	c.Ports = nil
}
