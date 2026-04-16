package collector

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTrimPod(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-pod",
			Namespace:   "default",
			Annotations: map[string]string{"kubectl.kubernetes.io/restartedAt": "now"},
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kubectl"},
			},
			Labels: map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			Volumes:    []corev1.Volume{{Name: "data"}},
			Containers: []corev1.Container{{Name: "app", Image: "nginx:1.25", Env: []corev1.EnvVar{{Name: "FOO", Value: "bar"}}}},
		},
	}

	out, err := TrimPod(p)
	if err != nil {
		t.Fatal(err)
	}
	pod := out.(*corev1.Pod)

	if pod.Annotations != nil {
		t.Error("annotations should be nil")
	}
	if pod.ManagedFields != nil {
		t.Error("managed fields should be nil")
	}
	if pod.Labels == nil || pod.Labels["app"] != "test" {
		t.Error("labels should be preserved")
	}
	if pod.Spec.Volumes != nil {
		t.Error("volumes should be nil")
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatal("should have 1 container")
	}
	if pod.Spec.Containers[0].Image != "nginx:1.25" {
		t.Error("image should be preserved")
	}
	if pod.Spec.Containers[0].Env != nil {
		t.Error("env should be stripped")
	}
}

func TestTrimReplicaSet(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deploy-abc123",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
			Annotations: map[string]string{
				"deployment.kubernetes.io/revision": "3",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "deploy",
				Controller: boolPtr(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: func() *int32 { n := int32(3); return &n }(),
		},
		Status: appsv1.ReplicaSetStatus{
			Replicas: 3,
		},
	}

	out, err := TrimReplicaSet(rs)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := out.(*appsv1.ReplicaSet)

	if trimmed.Annotations != nil {
		t.Error("annotations should be nil")
	}
	if trimmed.Labels != nil {
		t.Error("labels should be nil")
	}
	// OwnerReferences must be preserved — that's the whole point.
	if len(trimmed.OwnerReferences) != 1 {
		t.Fatal("OwnerReferences must be preserved")
	}
	if trimmed.OwnerReferences[0].Name != "deploy" {
		t.Error("OwnerReference name should be 'deploy'")
	}
	if trimmed.Spec.Replicas != nil {
		t.Error("spec should be zeroed")
	}
	if trimmed.Status.Replicas != 0 {
		t.Error("status should be zeroed")
	}
}

func TestTrimService(t *testing.T) {
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "svc",
			Annotations: map[string]string{"note": "x"},
		},
	}
	out, err := TrimService(s)
	if err != nil {
		t.Fatal(err)
	}
	if out.(*corev1.Service).Annotations != nil {
		t.Error("annotations should be nil")
	}
}

func TestTrimIngress(t *testing.T) {
	i := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "ing",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "helm"}},
		},
	}
	out, err := TrimIngress(i)
	if err != nil {
		t.Fatal(err)
	}
	if out.(*networkingv1.Ingress).ManagedFields != nil {
		t.Error("managed fields should be nil")
	}
}

func TestTrimUnstructured(t *testing.T) {
	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]any{
				"name":      "route",
				"namespace": "default",
				"annotations": map[string]any{
					"note": "x",
				},
			},
			"status": map[string]any{
				"conditions": []any{"ready"},
				"parents":    []any{"gw"},
			},
		},
	}
	out, err := TrimUnstructured(u)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := out.(*unstructured.Unstructured)
	if trimmed.GetAnnotations() != nil {
		t.Error("annotations should be nil")
	}
	_, found, _ := unstructured.NestedSlice(trimmed.Object, "status", "conditions")
	if found {
		t.Error("status.conditions should be removed")
	}
}

func TestTrimNonMatchingType(t *testing.T) {
	// TrimPod should pass through non-Pod objects unchanged.
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc"}}
	out, err := TrimPod(s)
	if err != nil {
		t.Fatal(err)
	}
	if out != s {
		t.Error("non-Pod should pass through unchanged")
	}
}
