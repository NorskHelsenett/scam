// spam-operator watches Pods cluster-wide and logs the image, repo and digest
// for every container (init, regular, ephemeral). Existing pods are dumped
// sorted by namespace / owner-kind:owner-name / pod-name at startup; after
// that, new pod events and image-resolution updates are streamed as they
// happen.
//
// Memory is kept low by:
//   - disabling resync (no periodic full relists)
//   - transforming cached Pods to strip fields we never read (annotations,
//     managed fields, volumes, env, probes, resources, ...)
//   - never keeping our own parallel cache of pod state
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	var kubeconfig string
	defaultKC := ""
	if h := homedir.HomeDir(); h != "" {
		defaultKC = filepath.Join(h, ".kube", "config")
	}
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKC, "path to kubeconfig (ignored in-cluster)")
	flag.Parse()

	cfg, err := loadConfig(kubeconfig)
	if err != nil {
		log.Error("load kube config", "err", err)
		os.Exit(1)
	}
	// Cap client-side resources. The informer will fan requests through this
	// single client, so we don't need high QPS.
	cfg.QPS = 5
	cfg.Burst = 10

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("build clientset", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		0, // resyncPeriod 0 -> no periodic relist
		informers.WithTransform(trimPod),
	)
	podInf := factory.Core().V1().Pods()

	var synced atomic.Bool

	_, err = podInf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return // absorbed by the sorted initial dump below
			}
			if p, ok := obj.(*corev1.Pod); ok {
				emit("ADD", p, nil)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if !synced.Load() {
				return
			}
			oldP, _ := oldObj.(*corev1.Pod)
			newP, _ := newObj.(*corev1.Pod)
			if newP == nil {
				return
			}
			emit("UPDATE", newP, oldP)
		},
	})
	if err != nil {
		log.Error("register handler", "err", err)
		os.Exit(1)
	}

	factory.Start(ctx.Done())
	log.Info("waiting for cache sync")
	if !cache.WaitForCacheSync(ctx.Done(), podInf.Informer().HasSynced) {
		log.Error("cache sync aborted")
		os.Exit(1)
	}

	// Sorted dump of everything the cache saw during initial ListAndWatch.
	pods, err := podInf.Lister().List(labels.Everything())
	if err != nil {
		log.Error("list cache", "err", err)
		os.Exit(1)
	}
	sort.Slice(pods, func(i, j int) bool { return lessPod(pods[i], pods[j]) })
	log.Info("initial snapshot", "pods", len(pods))
	for _, p := range pods {
		emit("INITIAL", p, nil)
	}
	synced.Store(true)
	log.Info("streaming new pod events")

	<-ctx.Done()
	log.Info("shutdown")
}

// loadConfig tries in-cluster first, then falls back to kubeconfig.
func loadConfig(kubeconfig string) (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	if kubeconfig == "" {
		return nil, fmt.Errorf("no in-cluster config and no kubeconfig given")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// trimPod drops Pod fields we never read so the informer cache stays small.
// It mutates in place; the informer owns the object from here on.
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

	// Status holds a lot we don't need; keep only imageID-bearing slices.
	p.Status.Conditions = nil
	p.Status.PodIPs = nil
	for i := range p.Status.ContainerStatuses {
		stripStatus(&p.Status.ContainerStatuses[i])
	}
	for i := range p.Status.InitContainerStatuses {
		stripStatus(&p.Status.InitContainerStatuses[i])
	}
	for i := range p.Status.EphemeralContainerStatuses {
		stripStatus(&p.Status.EphemeralContainerStatuses[i])
	}
	return p, nil
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

func stripStatus(s *corev1.ContainerStatus) {
	s.LastTerminationState = corev1.ContainerState{}
}

// owner returns (kind, name) for the pod's controller, or ("-", "-") if none.
func owner(p *corev1.Pod) (string, string) {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return o.Kind, o.Name
		}
	}
	if len(p.OwnerReferences) > 0 {
		return p.OwnerReferences[0].Kind, p.OwnerReferences[0].Name
	}
	return "-", "-"
}

func lessPod(a, b *corev1.Pod) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	ak, an := owner(a)
	bk, bn := owner(b)
	if ak != bk {
		return ak < bk
	}
	if an != bn {
		return an < bn
	}
	return a.Name < b.Name
}

// splitImage extracts repo, tag, digest. spec is what the user wrote in the
// PodSpec; imageID is the resolved reference from ContainerStatus (may carry
// the sha256 digest even when the spec only had a tag).
func splitImage(spec, imageID string) (repo, tag, digest string) {
	s := spec
	if at := strings.LastIndex(s, "@"); at >= 0 {
		digest = s[at+1:]
		s = s[:at]
	}
	// A tag uses ':' and must come after the last '/'; this avoids mistaking
	// a port number in the registry for a tag.
	if slash := strings.LastIndex(s, "/"); colonAfter(s, slash) >= 0 {
		c := colonAfter(s, slash)
		tag = s[c+1:]
		s = s[:c]
	}
	repo = s
	if digest == "" && imageID != "" {
		id := strings.TrimPrefix(imageID, "docker-pullable://")
		if at := strings.LastIndex(id, "@"); at >= 0 {
			digest = id[at+1:]
			// If spec had no registry, imageID often does; prefer it for repo.
			if !strings.Contains(repo, "/") || !strings.Contains(repo, ".") {
				if cand := id[:at]; cand != "" {
					repo = cand
				}
			}
		}
	}
	return
}

// colonAfter returns the index of the first ':' strictly after `from`, or -1.
func colonAfter(s string, from int) int {
	for i := from + 1; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// shortName strips the registry host from a repo and drops Docker Hub's
// synthetic "library/" namespace, so repos land in their human-readable form:
//   docker.io/library/postgres    -> postgres
//   docker.io/vaultwarden/server  -> vaultwarden/server
//   git.torden.tech/jonasbg/foo   -> jonasbg/foo
//   localhost:5000/foo/bar        -> foo/bar
//   myimage                       -> myimage
func shortName(repo string) string {
	s := repo
	if i := strings.IndexByte(s, '/'); i > 0 {
		first := s[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			s = s[i+1:]
		}
	}
	return strings.TrimPrefix(s, "library/")
}

type containerRef struct {
	kind string // init | main | ephemeral
	name string
	spec string // image from PodSpec
	id   string // imageID from status
}

func collect(p *corev1.Pod) []containerRef {
	n := len(p.Spec.InitContainers) + len(p.Spec.Containers) + len(p.Spec.EphemeralContainers)
	out := make([]containerRef, 0, n)
	// Build quick lookup of statuses by container name.
	status := func(kind, name string) string {
		var list []corev1.ContainerStatus
		switch kind {
		case "init":
			list = p.Status.InitContainerStatuses
		case "main":
			list = p.Status.ContainerStatuses
		case "ephemeral":
			list = p.Status.EphemeralContainerStatuses
		}
		for i := range list {
			if list[i].Name == name {
				return list[i].ImageID
			}
		}
		return ""
	}
	for _, c := range p.Spec.InitContainers {
		out = append(out, containerRef{"init", c.Name, c.Image, status("init", c.Name)})
	}
	for _, c := range p.Spec.Containers {
		out = append(out, containerRef{"main", c.Name, c.Image, status("main", c.Name)})
	}
	for _, c := range p.Spec.EphemeralContainers {
		out = append(out, containerRef{"ephemeral", c.Name, c.Image, status("ephemeral", c.Name)})
	}
	return out
}

// emit logs one line per container. For UPDATE events, if oldPod is set we
// suppress entries whose image + imageID are unchanged, so rolling imageID
// resolution is visible without spamming on every status tick.
func emit(event string, p, oldP *corev1.Pod) {
	ok, on := owner(p)
	cur := collect(p)
	var prev map[string]containerRef
	if oldP != nil {
		old := collect(oldP)
		prev = make(map[string]containerRef, len(old))
		for _, c := range old {
			prev[c.kind+"/"+c.name] = c
		}
	}
	for _, c := range cur {
		if prev != nil {
			if q, ok := prev[c.kind+"/"+c.name]; ok && q.spec == c.spec && q.id == c.id {
				continue
			}
		}
		repo, tag, digest := splitImage(c.spec, c.id)
		image := shortName(repo)
		log.Info(event,
			"namespace", p.Namespace,
			"owner_kind", ok,
			"owner", on,
			"pod", p.Name,
			"container_kind", c.kind,
			"container", c.name,
			"image", image,
			"repo", repo,
			"tag", tag,
			"digest", digest,
		)
	}
}
