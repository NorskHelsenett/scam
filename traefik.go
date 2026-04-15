package main

import (
	"regexp"
	"sort"

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

// traefikBackendKeys is a cache.IndexFunc: maps a Traefik route to its
// referenced Service keys ("ns/name"). Used by serviceReferenced to do an
// O(1) "is this Service exposed via Traefik?" lookup.
func traefikBackendKeys(obj any) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}
	return backendTargetsToKeys(traefikBackends(u)), nil
}

func dumpTraefik(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer) {
	dumpSorted(traefikKindFromResource(gvr.Resource), unstructuredList(inf), lessUnstructured,
		func(u *unstructured.Unstructured) int { emitTraefik("INITIAL", gvr, u); return 1 })
}

func emitTraefik(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	log.Info(event,
		"kind", traefikKindFromResource(gvr.Resource),
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"entry_points", uStringSlice(u.Object, "spec", "entryPoints"),
		"hosts", extractTraefikHosts(u),
		"tls_secret", uStr(u.Object, "spec", "tls", "secretName"),
		"backends", traefikBackends(u),
	)
}

// extractTraefikHosts pulls literal hosts out of every route's match string.
// UDP routes have no match string, so hosts is empty for those.
func extractTraefikHosts(u *unstructured.Unstructured) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range uSlice(u.Object, "spec", "routes") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		match := uStr(rm, "match")
		if match == "" {
			continue
		}
		for _, call := range hostMatcherRe.FindAllStringSubmatch(match, -1) {
			for _, m := range backtickedRe.FindAllStringSubmatch(call[1], -1) {
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
	var out []backendTarget
	for _, r := range uSlice(u.Object, "spec", "routes") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		for _, x := range uSlice(rm, "services") {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			// Skip Traefik-native references (TraefikService, etc.) — we only
			// care about regular Kubernetes Services here.
			if kind := uStr(m, "kind"); kind != "" && kind != "Service" {
				continue
			}
			name := uStr(m, "name")
			if name == "" {
				continue
			}
			ns := uStr(m, "namespace")
			if ns == "" {
				ns = u.GetNamespace()
			}
			out = append(out, backendTarget{ns, name})
		}
	}
	return out
}

// emitTraefikDelete emits a compact DELETE record for any Traefik route kind.
func emitTraefikDelete(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	log.Info("DELETE",
		"kind", traefikKindFromResource(gvr.Resource),
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
	)
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
