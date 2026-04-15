package main

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Transforms strip fields we never read before objects enter the informer
// cache. They mutate in place; the informer owns the object from here on.
//
// Transforms are also called on Update events, which means any field nil'd
// here will already be nil for both the "old" and "new" objects seen by
// UpdateFunc — so comparing old and new only reflects fields we actually keep.

func trimPod(obj any) (any, error) {
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

func trimService(obj any) (any, error) {
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

func trimIngress(obj any) (any, error) {
	i, ok := obj.(*networkingv1.Ingress)
	if !ok {
		return obj, nil
	}
	i.Annotations = nil
	i.ManagedFields = nil
	i.Finalizers = nil
	return i, nil
}

// trimMeta is a generic small transform for objects where we only read
// identity + a couple of spec fields (e.g., IngressClass, GatewayClass).
func trimMeta(obj any) (any, error) {
	if o, ok := obj.(metav1.Object); ok {
		o.SetAnnotations(nil)
		o.SetManagedFields(nil)
		o.SetFinalizers(nil)
	}
	return obj, nil
}

func trimUnstructured(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	u.SetAnnotations(nil)
	u.SetManagedFields(nil)
	u.SetFinalizers(nil)
	// status on routes/gateways churns hard (per-parent conditions, listener
	// attach state). We never emit any of it, so drop it wholesale. Gateway
	// status.addresses is what we read; keep only that.
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
