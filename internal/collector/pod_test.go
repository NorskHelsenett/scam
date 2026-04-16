package collector

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	appsinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func boolPtr(b bool) *bool { return &b }

func TestPodOwnerDeployment(t *testing.T) {
	// Simulate: Deployment → ReplicaSet → Pod
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "argocd-server-8b8d65c9",
			Namespace: "argocd",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "argocd-server",
				Controller: boolPtr(true),
			}},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "argocd-server-8b8d65c9-zr5bd",
			Namespace: "argocd",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "argocd-server-8b8d65c9",
				Controller: boolPtr(true),
			}},
		},
	}

	client := fake.NewSimpleClientset(rs)
	factory := appsinformers.NewSharedInformerFactory(client, 0)
	rsInf := factory.Apps().V1().ReplicaSets()
	// Trigger informer creation so Start() knows about it.
	_ = rsInf.Informer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	factory.Start(ctx.Done())

	// WaitForCacheSync needs the HasSynced functions, not just the stop channel.
	if !cache.WaitForCacheSync(ctx.Done(), rsInf.Informer().HasSynced) {
		t.Fatal("cache did not sync in time")
	}

	// Verify the RS is in the cache.
	rsList, _ := rsInf.Lister().ReplicaSets("argocd").List(labels.Everything())
	if len(rsList) != 1 {
		t.Fatalf("expected 1 RS in cache, got %d", len(rsList))
	}

	old := Refs.ReplicaSets
	Refs.ReplicaSets = rsInf
	defer func() { Refs.ReplicaSets = old }()

	kind, name := PodOwner(pod)
	if kind != "Deployment" || name != "argocd-server" {
		t.Errorf("PodOwner() = (%q, %q), want (\"Deployment\", \"argocd-server\")", kind, name)
	}
}

func TestPodOwnerStatefulSet(t *testing.T) {
	// StatefulSet → Pod (no ReplicaSet in the chain)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vaultwarden-0",
			Namespace: "vaultwarden",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "StatefulSet",
				Name:       "vaultwarden",
				Controller: boolPtr(true),
			}},
		},
	}

	// No RS informer needed — owner is already StatefulSet.
	kind, name := PodOwner(pod)
	if kind != "StatefulSet" || name != "vaultwarden" {
		t.Errorf("PodOwner() = (%q, %q), want (\"StatefulSet\", \"vaultwarden\")", kind, name)
	}
}

func TestPodOwnerDaemonSet(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "blocky-abc12",
			Namespace: "blocky",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "DaemonSet",
				Name:       "blocky",
				Controller: boolPtr(true),
			}},
		},
	}

	kind, name := PodOwner(pod)
	if kind != "DaemonSet" || name != "blocky" {
		t.Errorf("PodOwner() = (%q, %q), want (\"DaemonSet\", \"blocky\")", kind, name)
	}
}

func TestPodOwnerStandalone(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "debug", Namespace: "default"},
	}

	kind, name := PodOwner(pod)
	if kind != "-" || name != "-" {
		t.Errorf("PodOwner() = (%q, %q), want (\"-\", \"-\")", kind, name)
	}
}

func TestPodOwnerReplicaSetNoLister(t *testing.T) {
	// When RS lister is nil, should return the raw ReplicaSet owner.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-abc123-xyz",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "app-abc123",
				Controller: boolPtr(true),
			}},
		},
	}

	old := Refs.ReplicaSets
	Refs.ReplicaSets = nil
	defer func() { Refs.ReplicaSets = old }()

	kind, name := PodOwner(pod)
	if kind != "ReplicaSet" || name != "app-abc123" {
		t.Errorf("PodOwner() = (%q, %q), want (\"ReplicaSet\", \"app-abc123\")", kind, name)
	}
}

func TestPodOwnerReplicaSetNotOwnedByDeployment(t *testing.T) {
	// A standalone ReplicaSet (not owned by a Deployment).
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "standalone-rs",
			Namespace: "test",
			// No OwnerReferences — not managed by a Deployment.
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "standalone-rs-abc",
			Namespace: "test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "standalone-rs",
				Controller: boolPtr(true),
			}},
		},
	}

	client := fake.NewSimpleClientset(rs)
	factory := appsinformers.NewSharedInformerFactory(client, 0)
	rsInf := factory.Apps().V1().ReplicaSets()
	_ = rsInf.Informer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), rsInf.Informer().HasSynced) {
		t.Fatal("cache did not sync in time")
	}

	old := Refs.ReplicaSets
	Refs.ReplicaSets = rsInf
	defer func() { Refs.ReplicaSets = old }()

	kind, name := PodOwner(pod)
	// Should remain ReplicaSet since RS has no Deployment owner.
	if kind != "ReplicaSet" || name != "standalone-rs" {
		t.Errorf("PodOwner() = (%q, %q), want (\"ReplicaSet\", \"standalone-rs\")", kind, name)
	}
}

// TestSamePodImagesIdentical verifies that identical pods report same images.
func TestSamePodImagesIdentical(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx:1.25"}},
		},
	}
	if !SamePodImages(pod, pod) {
		t.Error("identical pods should report same images")
	}
}

func TestSamePodImagesPhaseChange(t *testing.T) {
	a := &corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.25"}}},
	}
	b := &corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.25"}}},
	}
	if SamePodImages(a, b) {
		t.Error("phase change should report different images")
	}
}

