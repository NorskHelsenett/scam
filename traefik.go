package main

import (
	"regexp"
	"sort"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"
)

// Traefik ships its own CRDs for HTTP/TCP/UDP routing. The FQDNs live inside
// Traefik's rule-language `match` string (e.g. `Host(`example.com`)`), not in
// a structured field, so we regex them out.
//
// Two groups are recognised: the current "traefik.io" and the legacy
// "traefik.containo.us". If both are installed, each resource is watched on
// whichever group offers it (preferring traefik.io).

var traefikGroups = []string{"traefik.io", "traefik.containo.us"}
var traefikGroupRank = map[string]int{"traefik.io": 2, "traefik.containo.us": 1}
var traefikResources = []string{"ingressroutes", "ingressroutetcps", "ingressrouteudps"}

// Regex to extract literal hostnames from Traefik's match language.
// Covers Host(`...`), HostSNI(`...`), HostHeader(`...`), HostRegexp(`...`).
// HostRegexp values are often patterns, not literal hosts, but we emit them
// anyway — the consumer can decide what to do with them.
var hostMatcherRe = regexp.MustCompile("(?:HostSNI|HostHeader|HostRegexp|Host)\\(([^)]*)\\)")
var backtickedRe = regexp.MustCompile("`([^`]+)`")

// discoverTraefik returns the set of Traefik route GVRs present on the
// server. Resources are deduplicated across groups, preferring traefik.io.
func discoverTraefik(disc discovery.DiscoveryInterface) []schema.GroupVersionResource {
	best := map[string]schema.GroupVersionResource{} // keyed by resource
	rank := map[string]int{}

	for _, group := range traefikGroups {
		for _, version := range []string{"v1", "v1alpha1"} {
			gv := group + "/" + version
			rl, err := disc.ServerResourcesForGroupVersion(gv)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					log.Debug("traefik discovery", "group_version", gv, "err", err)
				}
				continue
			}
			if rl == nil {
				continue
			}
			parsed, _ := schema.ParseGroupVersion(gv)
			for _, r := range rl.APIResources {
				if !wantTraefikResource(r.Name) {
					continue
				}
				if traefikGroupRank[parsed.Group] > rank[r.Name] {
					best[r.Name] = parsed.WithResource(r.Name)
					rank[r.Name] = traefikGroupRank[parsed.Group]
				}
			}
		}
	}

	out := make([]schema.GroupVersionResource, 0, len(best))
	for _, gvr := range best {
		out = append(out, gvr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

func wantTraefikResource(name string) bool {
	for _, r := range traefikResources {
		if r == name {
			return true
		}
	}
	return false
}

func registerTraefikHandler(inf cache.SharedIndexInformer, gvr schema.GroupVersionResource, synced *atomic.Bool) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				emitTraefik("ADD", gvr, u)
				refreshBackendServices(traefikBackends(u))
			}
		},
		UpdateFunc: func(_, newObj any) {
			if !synced.Load() {
				return
			}
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				emitTraefik("UPDATE", gvr, u)
				refreshBackendServices(traefikBackends(u))
			}
		},
	})
}

func dumpTraefik(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer) {
	objs := inf.GetIndexer().List()
	us := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			us = append(us, u)
		}
	}
	sort.Slice(us, func(i, j int) bool {
		if us[i].GetNamespace() != us[j].GetNamespace() {
			return us[i].GetNamespace() < us[j].GetNamespace()
		}
		return us[i].GetName() < us[j].GetName()
	})
	for _, u := range us {
		emitTraefik("INITIAL", gvr, u)
	}
	log.Info("initial snapshot", "kind", traefikKindFromResource(gvr.Resource), "cached", len(us), "emitted", len(us))
}

func emitTraefik(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	entryPoints, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "entryPoints")
	hosts := extractTraefikHosts(u)
	backends := traefikBackends(u)
	tlsSecret, _, _ := unstructured.NestedString(u.Object, "spec", "tls", "secretName")
	log.Info(event,
		"kind", traefikKindFromResource(gvr.Resource),
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"entry_points", entryPoints,
		"hosts", hosts,
		"tls_secret", tlsSecret,
		"backends", backends,
	)
}

// extractTraefikHosts pulls literal hosts out of every route's match string.
// UDP routes have no match string, so hosts is empty for those.
func extractTraefikHosts(u *unstructured.Unstructured) []string {
	routes, _, _ := unstructured.NestedSlice(u.Object, "spec", "routes")
	seen := map[string]struct{}{}
	var out []string
	for _, r := range routes {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		match, _, _ := unstructured.NestedString(rm, "match")
		if match == "" {
			continue
		}
		for _, call := range hostMatcherRe.FindAllStringSubmatch(match, -1) {
			args := call[1]
			for _, m := range backtickedRe.FindAllStringSubmatch(args, -1) {
				h := m[1]
				if _, dup := seen[h]; dup {
					continue
				}
				seen[h] = struct{}{}
				out = append(out, h)
			}
		}
	}
	return out
}

// traefikBackends flattens services referenced from every route. Traefik
// routes put backends under `services`, unlike Gateway API which uses
// `backendRefs`.
func traefikBackends(u *unstructured.Unstructured) []backendTarget {
	routes, _, _ := unstructured.NestedSlice(u.Object, "spec", "routes")
	var out []backendTarget
	for _, r := range routes {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		svcs, _, _ := unstructured.NestedSlice(rm, "services")
		for _, x := range svcs {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			// Skip Traefik-native references (TraefikService, etc.) — we only
			// care about regular Kubernetes Services here.
			kind, _, _ := unstructured.NestedString(m, "kind")
			if kind != "" && kind != "Service" {
				continue
			}
			name, _, _ := unstructured.NestedString(m, "name")
			if name == "" {
				continue
			}
			ns, _, _ := unstructured.NestedString(m, "namespace")
			if ns == "" {
				ns = u.GetNamespace()
			}
			out = append(out, backendTarget{ns, name})
		}
	}
	return out
}

func traefikKindFromResource(r string) string {
	switch r {
	case "ingressroutes":
		return "IngressRoute"
	case "ingressroutetcps":
		return "IngressRouteTCP"
	case "ingressrouteudps":
		return "IngressRouteUDP"
	}
	return r
}

