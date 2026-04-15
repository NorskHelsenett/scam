package main

import (
	"sort"
	"strings"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	coreinformers "k8s.io/client-go/informers/core/v1"
	netinformers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/tools/cache"
)

// Every handler ignores events while initial sync is in progress — the
// sorted initial dump covers those. Post-sync, Add/Update events stream.

// refs holds informer references used for cross-resource lookups
// (e.g., "is this Service referenced by any Ingress/Route?"). Populated
// once from main() after all informers are created.
var refs struct {
	services    coreinformers.ServiceInformer
	ingresses   netinformers.IngressInformer
	gwInformers map[string]cache.SharedIndexInformer // Gateway API, keyed by gvr.String()
	trInformers map[string]cache.SharedIndexInformer // Traefik, keyed by gvr.String()
}

// ---------- Pods / Containers ---------------------------------------------

func registerPodHandler(inf coreinformers.PodInformer, synced *atomic.Bool) {
	_, _ = inf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if p, ok := obj.(*corev1.Pod); ok {
				emitPod("ADD", p, nil)
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
			emitPod("UPDATE", newP, oldP)
		},
	})
}

func dumpPods(inf coreinformers.PodInformer) {
	pods, err := inf.Lister().List(labels.Everything())
	if err != nil {
		log.Error("list pods", "err", err)
		return
	}
	sort.Slice(pods, func(i, j int) bool { return lessPod(pods[i], pods[j]) })
	emitted := 0
	for _, p := range pods {
		emitted += emitPod("INITIAL", p, nil)
	}
	// Pods have a 1:N relationship with emitted records (one per container),
	// so we report both cached pods and emitted container records.
	log.Info("initial snapshot", "kind", "Pod", "cached", len(pods), "emitted_containers", emitted)
}

func lessPod(a, b *corev1.Pod) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	ak, an := podOwner(a)
	bk, bn := podOwner(b)
	if ak != bk {
		return ak < bk
	}
	if an != bn {
		return an < bn
	}
	return a.Name < b.Name
}

func podOwner(p *corev1.Pod) (string, string) {
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

// samePodImages reports whether two Pods' container specs and resolved
// imageIDs are identical — the only facts emitPod actually emits. Lets us
// short-circuit pod status churn (IP flips, condition ticks, etc.) without
// allocating collectContainers + its prev map.
func samePodImages(a, b *corev1.Pod) bool {
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
	// Status entries aren't guaranteed to be in the same order as spec, so
	// match by name via a linear scan. Container counts per pod are tiny.
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

// emitPod logs one line per container and returns how many it emitted. On
// UPDATE, containers whose image+imageID didn't change are suppressed so
// rolling digest resolution is visible without spamming on every status tick.
func emitPod(event string, p, oldP *corev1.Pod) int {
	// Fast path: most pod UPDATEs are status churn (IP flips, condition
	// ticks). If the image set and resolved imageIDs haven't moved, we have
	// nothing to emit — bail before allocating anything.
	if oldP != nil && samePodImages(oldP, p) {
		return 0
	}
	ok, on := podOwner(p)
	cur := collectContainers(p)
	var prev map[string]containerRef
	if oldP != nil {
		old := collectContainers(oldP)
		prev = make(map[string]containerRef, len(old))
		for _, c := range old {
			prev[c.kind+"/"+c.name] = c
		}
	}
	emitted := 0
	for _, c := range cur {
		if prev != nil {
			if q, ok := prev[c.kind+"/"+c.name]; ok && q.spec == c.spec && q.id == c.id {
				continue
			}
		}
		fullRepo, tag, digest := splitImage(c.spec, c.id)
		image, registry := splitImageName(fullRepo)
		// image_spec is the raw PodSpec string (e.g., "vaultwarden/server:1.35.4").
		// image_id is the resolved reference the kubelet reports (may be empty
		// if the image hasn't been pulled yet). Both are preserved alongside
		// our parsed registry/image/tag/digest as ground truth — if the parse
		// is wrong for some weird ref, consumers can fall back to these.
		log.Info(event,
			"kind", "Container",
			"cluster", cluster,
			"namespace", p.Namespace,
			"pod_uid", string(p.UID),
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

// ---------- Services ------------------------------------------------------

type svcPort struct {
	Name       string `json:"name,omitempty"`
	Port       int32  `json:"port"`
	TargetPort string `json:"target_port,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	NodePort   int32  `json:"node_port,omitempty"`
	AppProto   string `json:"app_protocol,omitempty"`
}

func registerServiceHandler(inf coreinformers.ServiceInformer, synced *atomic.Bool) {
	_, _ = inf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if s, ok := obj.(*corev1.Service); ok {
				emitService("ADD", s)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if !synced.Load() {
				return
			}
			if s, ok := newObj.(*corev1.Service); ok {
				emitService("UPDATE", s)
			}
		},
	})
}

func dumpServices(inf coreinformers.ServiceInformer) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		log.Error("list services", "err", err)
		return
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Namespace != list[j].Namespace {
			return list[i].Namespace < list[j].Namespace
		}
		return list[i].Name < list[j].Name
	})
	emitted := 0
	for _, s := range list {
		if emitService("INITIAL", s) {
			emitted++
		}
	}
	log.Info("initial snapshot", "kind", "Service", "cached", len(list), "emitted", emitted)
}

// emitService applies the "drop cluster-internal ClusterIPs" filter before
// delegating to emitServiceRaw. Returns true if the Service was emitted.
// Callers that need to emit regardless (for services referenced by
// Ingresses/Routes) should use emitServiceForce.
func emitService(event string, s *corev1.Service) bool {
	if s.Spec.Type == corev1.ServiceTypeClusterIP && !serviceReferenced(s.Namespace, s.Name) {
		return false
	}
	emitServiceRaw(event, s)
	return true
}

func emitServiceRaw(event string, s *corev1.Service) {
	ports := make([]svcPort, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		ports = append(ports, svcPort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: p.TargetPort.String(),
			Protocol:   string(p.Protocol),
			NodePort:   p.NodePort,
			AppProto:   derefStr(p.AppProtocol),
		})
	}
	lbIPs, lbHosts := lbAddresses(s.Status.LoadBalancer.Ingress)
	log.Info(event,
		"kind", "Service",
		"cluster", cluster,
		"uid", string(s.UID),
		"namespace", s.Namespace,
		"name", s.Name,
		"labels", s.Labels,
		"service_type", string(s.Spec.Type),
		"cluster_ips", s.Spec.ClusterIPs,
		"external_ips", s.Spec.ExternalIPs,
		"external_name", s.Spec.ExternalName,
		"selector", s.Spec.Selector,
		"ports", ports,
		"lb_ips", lbIPs,
		"lb_hostnames", lbHosts,
	)
}

func lbAddresses(in []corev1.LoadBalancerIngress) ([]string, []string) {
	var ips, hosts []string
	for _, lb := range in {
		if lb.IP != "" {
			ips = append(ips, lb.IP)
		}
		if lb.Hostname != "" {
			hosts = append(hosts, lb.Hostname)
		}
	}
	return ips, hosts
}

// IngressLoadBalancerIngress is a separate type from core's LoadBalancerIngress
// despite having the same shape, so we need a second extractor.
func ingressLbAddresses(in []networkingv1.IngressLoadBalancerIngress) ([]string, []string) {
	var ips, hosts []string
	for _, lb := range in {
		if lb.IP != "" {
			ips = append(ips, lb.IP)
		}
		if lb.Hostname != "" {
			hosts = append(hosts, lb.Hostname)
		}
	}
	return ips, hosts
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------- Ingress -------------------------------------------------------

type ingPath struct {
	Path        string `json:"path,omitempty"`
	PathType    string `json:"path_type,omitempty"`
	BackendKind string `json:"backend_kind,omitempty"` // Service or Resource (CRD)
	BackendName string `json:"backend_name,omitempty"`
	BackendPort string `json:"backend_port,omitempty"` // number or name
}

type ingRule struct {
	Host  string    `json:"host,omitempty"`
	Paths []ingPath `json:"paths,omitempty"`
}

type ingTLS struct {
	Hosts  []string `json:"hosts,omitempty"`
	Secret string   `json:"secret,omitempty"`
}

func registerIngressHandler(inf netinformers.IngressInformer, synced *atomic.Bool) {
	_, _ = inf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if i, ok := obj.(*networkingv1.Ingress); ok {
				emitIngress("ADD", i)
				refreshBackendServices(ingressBackends(i))
			}
		},
		UpdateFunc: func(_, newObj any) {
			if !synced.Load() {
				return
			}
			if i, ok := newObj.(*networkingv1.Ingress); ok {
				emitIngress("UPDATE", i)
				refreshBackendServices(ingressBackends(i))
			}
		},
	})
}

func dumpIngresses(inf netinformers.IngressInformer) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		log.Error("list ingresses", "err", err)
		return
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Namespace != list[j].Namespace {
			return list[i].Namespace < list[j].Namespace
		}
		return list[i].Name < list[j].Name
	})
	for _, i := range list {
		emitIngress("INITIAL", i)
	}
	log.Info("initial snapshot", "kind", "Ingress", "cached", len(list), "emitted", len(list))
}

func emitIngress(event string, i *networkingv1.Ingress) {
	class := ""
	if i.Spec.IngressClassName != nil {
		class = *i.Spec.IngressClassName
	}
	rules := make([]ingRule, 0, len(i.Spec.Rules))
	for _, r := range i.Spec.Rules {
		rule := ingRule{Host: r.Host}
		if r.HTTP != nil {
			for _, path := range r.HTTP.Paths {
				pp := ingPath{Path: path.Path}
				if path.PathType != nil {
					pp.PathType = string(*path.PathType)
				}
				if path.Backend.Service != nil {
					pp.BackendKind = "Service"
					pp.BackendName = path.Backend.Service.Name
					if path.Backend.Service.Port.Name != "" {
						pp.BackendPort = path.Backend.Service.Port.Name
					} else if path.Backend.Service.Port.Number != 0 {
						pp.BackendPort = itoa(int64(path.Backend.Service.Port.Number))
					}
				} else if path.Backend.Resource != nil {
					pp.BackendKind = path.Backend.Resource.Kind
					pp.BackendName = path.Backend.Resource.Name
				}
				rule.Paths = append(rule.Paths, pp)
			}
		}
		rules = append(rules, rule)
	}
	tls := make([]ingTLS, 0, len(i.Spec.TLS))
	for _, t := range i.Spec.TLS {
		tls = append(tls, ingTLS{Hosts: t.Hosts, Secret: t.SecretName})
	}
	lbIPs, lbHosts := ingressLbAddresses(i.Status.LoadBalancer.Ingress)
	log.Info(event,
		"kind", "Ingress",
		"cluster", cluster,
		"uid", string(i.UID),
		"namespace", i.Namespace,
		"name", i.Name,
		"labels", i.Labels,
		"ingress_class", class,
		"rules", rules,
		"tls", tls,
		"lb_ips", lbIPs,
		"lb_hostnames", lbHosts,
	)
}

// ---------- IngressClass --------------------------------------------------

func registerIngressClassHandler(inf netinformers.IngressClassInformer, synced *atomic.Bool) {
	_, _ = inf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if ic, ok := obj.(*networkingv1.IngressClass); ok {
				emitIngressClass("ADD", ic)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if !synced.Load() {
				return
			}
			if ic, ok := newObj.(*networkingv1.IngressClass); ok {
				emitIngressClass("UPDATE", ic)
			}
		},
	})
}

func dumpIngressClasses(inf netinformers.IngressClassInformer) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		log.Error("list ingressclasses", "err", err)
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, ic := range list {
		emitIngressClass("INITIAL", ic)
	}
	log.Info("initial snapshot", "kind", "IngressClass", "cached", len(list), "emitted", len(list))
}

func emitIngressClass(event string, ic *networkingv1.IngressClass) {
	log.Info(event,
		"kind", "IngressClass",
		"cluster", cluster,
		"uid", string(ic.UID),
		"name", ic.Name,
		"labels", ic.Labels,
		"controller", ic.Spec.Controller,
	)
}

// ---------- exposure-chain reference checks ------------------------------
//
// A Service is "exposure-relevant" if it's non-ClusterIP (directly addressable
// from outside the cluster) OR if any Ingress / Gateway API route points to it.
// ClusterIP services that nobody exposes are cluster-internal noise we drop.

// backendIndexName is the name of the cache.Indexer attached to each
// Ingress/route informer that maps "ns/svcName" keys → referring objects.
// serviceReferenced is then an O(1) lookup.
const backendIndexName = "byBackend"

func serviceReferenced(ns, name string) bool {
	key := ns + "/" + name
	if refs.ingresses != nil {
		if objs, _ := refs.ingresses.Informer().GetIndexer().ByIndex(backendIndexName, key); len(objs) > 0 {
			return true
		}
	}
	// Gateway API routes. Non-route kinds (Gateway, GatewayClass) simply have
	// no entries under this index, so the lookup is cheap regardless.
	for _, inf := range refs.gwInformers {
		if objs, _ := inf.GetIndexer().ByIndex(backendIndexName, key); len(objs) > 0 {
			return true
		}
	}
	for _, inf := range refs.trInformers {
		if objs, _ := inf.GetIndexer().ByIndex(backendIndexName, key); len(objs) > 0 {
			return true
		}
	}
	return false
}

// ---------- indexer key extractors (one per informer) ---------------------

func ingressBackendKeys(obj any) ([]string, error) {
	ing, ok := obj.(*networkingv1.Ingress)
	if !ok {
		return nil, nil
	}
	var keys []string
	seen := make(map[string]struct{})
	add := func(svcName string) {
		if svcName == "" {
			return
		}
		k := ing.Namespace + "/" + svcName
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
		add(ing.Spec.DefaultBackend.Service.Name)
	}
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			if p.Backend.Service != nil {
				add(p.Backend.Service.Name)
			}
		}
	}
	return keys, nil
}

func backendTargetsToKeys(targets []backendTarget) []string {
	if len(targets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		k := t.namespace + "/" + t.name
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// backendTarget identifies a Service a given Ingress/Route points at.
type backendTarget struct{ namespace, name string }

func ingressBackends(ing *networkingv1.Ingress) []backendTarget {
	var out []backendTarget
	if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
		out = append(out, backendTarget{ing.Namespace, ing.Spec.DefaultBackend.Service.Name})
	}
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			if p.Backend.Service != nil {
				out = append(out, backendTarget{ing.Namespace, p.Backend.Service.Name})
			}
		}
	}
	return out
}

func routeBackends(u *unstructured.Unstructured) []backendTarget {
	var out []backendTarget
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	for _, r := range rules {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		brefs, _, _ := unstructured.NestedSlice(rm, "backendRefs")
		for _, x := range brefs {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			kind, _, _ := unstructured.NestedString(m, "kind")
			if kind != "" && kind != "Service" {
				continue
			}
			rn, _, _ := unstructured.NestedString(m, "name")
			if rn == "" {
				continue
			}
			rns, _, _ := unstructured.NestedString(m, "namespace")
			if rns == "" {
				rns = u.GetNamespace()
			}
			out = append(out, backendTarget{rns, rn})
		}
	}
	return out
}

// refreshBackendServices re-emits each referenced Service. Called when an
// Ingress/Route appears or changes so that ClusterIP services in the exposure
// chain — which emitService would otherwise filter out — still make it into
// the record stream.
func refreshBackendServices(targets []backendTarget) {
	if refs.services == nil || len(targets) == 0 {
		return
	}
	// A single Ingress/Route routinely points at the same Service from
	// multiple rules/paths. Dedup before lister lookups + emits.
	var seen map[backendTarget]struct{}
	if len(targets) > 1 {
		seen = make(map[backendTarget]struct{}, len(targets))
	}
	for _, t := range targets {
		if seen != nil {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
		}
		svc, err := refs.services.Lister().Services(t.namespace).Get(t.name)
		if err != nil || svc == nil {
			continue
		}
		emitServiceForce("EXPOSURE", svc)
	}
}

// emitServiceForce emits regardless of the "internal ClusterIP" filter.
// Used from Ingress/Route handlers to keep referenced services visible.
func emitServiceForce(event string, s *corev1.Service) {
	emitServiceRaw(event, s)
}

// ---------- image-spec parsing -------------------------------------------

// splitImage extracts repo, tag, digest. spec is what the user wrote in the
// PodSpec; imageID is the resolved reference from ContainerStatus (may carry
// the sha256 digest even when the spec only had a tag).
func splitImage(spec, imageID string) (repo, tag, digest string) {
	s := spec
	if at := strings.LastIndex(s, "@"); at >= 0 {
		digest = s[at+1:]
		s = s[:at]
	}
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
			if !strings.Contains(repo, "/") || !strings.Contains(repo, ".") {
				if cand := id[:at]; cand != "" {
					repo = cand
				}
			}
		}
	}
	return
}

func colonAfter(s string, from int) int {
	for i := from + 1; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// splitImageName separates the registry host from the image path on that
// registry. The first '/'-separated segment counts as a registry only if it
// contains '.' or ':' or equals "localhost". Docker Hub's synthetic
// "library/" namespace is dropped from the image path.
//
//	docker.io/library/postgres             -> registry=docker.io,        image=postgres
//	docker.io/vaultwarden/server           -> registry=docker.io,        image=vaultwarden/server
//	git.torden.tech/jonasbg/spam-operator  -> registry=git.torden.tech,  image=jonasbg/spam-operator
//	git.torden.tech/jonasbg/spam/trivy-... -> registry=git.torden.tech,  image=jonasbg/spam/trivy-scanner
//	quay.io/argoproj/argocd                -> registry=quay.io,          image=argoproj/argocd
//	docker.io/traefik                      -> registry=docker.io,        image=traefik
//	myimage                                -> registry="",               image=myimage
func splitImageName(full string) (image, registry string) {
	if i := strings.IndexByte(full, '/'); i > 0 {
		first := full[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry = first
			full = full[i+1:]
		}
	}
	if registry == "docker.io" {
		full = strings.TrimPrefix(full, "library/")
	}
	return full, registry
}

// ---------- misc ----------------------------------------------------------

func itoa(n int64) string {
	// small, alloc-free int->string for cases where strconv would pull in more
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
