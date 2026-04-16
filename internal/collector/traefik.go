package collector

import (
	"regexp"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"
)

var traefikGroups = []string{"traefik.io", "traefik.containo.us"}
var traefikGroupRank = map[string]int{"traefik.io": 2, "traefik.containo.us": 1}
var traefikResources = []string{"ingressroutes", "ingressroutetcps", "ingressrouteudps"}

var hostMatcherRe = regexp.MustCompile("(?:HostSNI|HostHeader|HostRegexp|Host)\\(([^)]*)\\)")
var backtickedRe = regexp.MustCompile("`([^`]+)`")

// DiscoverTraefik returns the set of Traefik route GVRs present on the server.
func DiscoverTraefik(disc discovery.DiscoveryInterface) []schema.GroupVersionResource {
	best := map[string]schema.GroupVersionResource{}
	rank := map[string]int{}

	for _, group := range traefikGroups {
		for _, version := range []string{"v1", "v1alpha1"} {
			gv := group + "/" + version
			rl, err := disc.ServerResourcesForGroupVersion(gv)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					Log.Debug("traefik discovery", "group_version", gv, "err", err)
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

func DumpTraefik(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer) {
	DumpSorted(traefikKindFromResource(gvr.Resource), UnstructuredList(inf), LessUnstructured,
		func(u *unstructured.Unstructured) int { EmitTraefik("INITIAL", gvr, u); return 1 })
}

func EmitTraefik(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	Log.Info(event,
		"kind", traefikKindFromResource(gvr.Resource),
		"api_version", gvr.GroupVersion().String(),
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"entry_points", UStringSlice(u.Object, "spec", "entryPoints"),
		"hosts", ExtractTraefikHosts(u),
		"tls_secret", UStr(u.Object, "spec", "tls", "secretName"),
		"backends", TraefikBackends(u),
	)
}

// ExtractTraefikHosts pulls literal hosts out of every route's match string.
func ExtractTraefikHosts(u *unstructured.Unstructured) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range USlice(u.Object, "spec", "routes") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		match := UStr(rm, "match")
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

// TraefikBackends flattens services referenced from every route.
func TraefikBackends(u *unstructured.Unstructured) []BackendTarget {
	var out []BackendTarget
	for _, r := range USlice(u.Object, "spec", "routes") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		for _, x := range USlice(rm, "services") {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			if kind := UStr(m, "kind"); kind != "" && kind != "Service" {
				continue
			}
			name := UStr(m, "name")
			if name == "" {
				continue
			}
			ns := UStr(m, "namespace")
			if ns == "" {
				ns = u.GetNamespace()
			}
			out = append(out, BackendTarget{ns, name})
		}
	}
	return out
}

func EmitTraefikDelete(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	Log.Info("DELETE",
		"kind", traefikKindFromResource(gvr.Resource),
		"api_version", gvr.GroupVersion().String(),
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
