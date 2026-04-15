package main

import (
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"
)

// Gateway API GVRs we care about. For each we prefer the highest server-
// advertised version; v1 beats v1beta1 beats v1alpha2.
var gwAPIGroup = "gateway.networking.k8s.io"
var gwAPIVersionRank = map[string]int{"v1": 3, "v1beta1": 2, "v1alpha2": 1}
var gwAPIResources = []string{
	"gateways",
	"gatewayclasses",
	"httproutes",
	"grpcroutes",
	"tlsroutes",
	"tcproutes",
}

// discoverGatewayAPI returns the GVRs that are actually present on the server.
// Missing group or missing resources are silently skipped.
func discoverGatewayAPI(disc discovery.DiscoveryInterface) []schema.GroupVersionResource {
	best := map[string]schema.GroupVersionResource{} // keyed by resource name
	rank := map[string]int{}

	for v := range gwAPIVersionRank {
		gv := gwAPIGroup + "/" + v
		rl, err := disc.ServerResourcesForGroupVersion(gv)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				// Group exists but this version doesn't, or transient error. Log once per version.
				log.Debug("gateway api discovery", "group_version", gv, "err", err)
			}
			continue
		}
		if rl == nil {
			continue
		}
		parsed, _ := schema.ParseGroupVersion(gv)
		for _, r := range rl.APIResources {
			if strings.Contains(r.Name, "/") { // subresource
				continue
			}
			if !wantResource(r.Name) {
				continue
			}
			if gwAPIVersionRank[parsed.Version] > rank[r.Name] {
				best[r.Name] = parsed.WithResource(r.Name)
				rank[r.Name] = gwAPIVersionRank[parsed.Version]
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

func wantResource(name string) bool {
	for _, r := range gwAPIResources {
		if r == name {
			return true
		}
	}
	return false
}

func gvrStrings(gvrs []schema.GroupVersionResource) []string {
	out := make([]string, 0, len(gvrs))
	for _, g := range gvrs {
		out = append(out, g.GroupVersion().String()+"/"+g.Resource)
	}
	return out
}

// routeBackendKeys is a cache.IndexFunc: maps a route Unstructured to its
// referenced Service keys ("ns/name"). Non-route objects (Gateway,
// GatewayClass) contribute no keys, so the indexer is still safe to attach.
func routeBackendKeys(obj any) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}
	return backendTargetsToKeys(routeBackends(u)), nil
}

func isRouteGVR(gvr schema.GroupVersionResource) bool {
	switch gvr.Resource {
	case "httproutes", "grpcroutes", "tlsroutes", "tcproutes":
		return true
	}
	return false
}

func dumpGatewayAPI(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer) {
	dumpSorted(kindFromResource(gvr.Resource), unstructuredList(inf), lessUnstructured,
		func(u *unstructured.Unstructured) int { emitGatewayAPI("INITIAL", gvr, u); return 1 })
}

func emitGatewayAPI(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	switch gvr.Resource {
	case "gateways":
		emitGateway(event, gvr, u)
	case "gatewayclasses":
		emitGatewayClass(event, gvr, u)
	case "httproutes":
		emitRoute(event, gvr, u, "HTTPRoute")
	case "grpcroutes":
		emitRoute(event, gvr, u, "GRPCRoute")
	case "tlsroutes":
		emitRoute(event, gvr, u, "TLSRoute")
	case "tcproutes":
		emitRoute(event, gvr, u, "TCPRoute")
	}
}

// ---------- Gateway -------------------------------------------------------

type gwListener struct {
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

type gwAddress struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value,omitempty"`
}

func emitGateway(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	log.Info(event,
		"kind", "Gateway",
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"gateway_class", uStr(u.Object, "spec", "gatewayClassName"),
		"listeners", parseListeners(u),
		"spec_addresses", parseAddresses(u.Object, "spec", "addresses"),
		"addresses", parseAddresses(u.Object, "status", "addresses"),
	)
}

func parseListeners(u *unstructured.Unstructured) []gwListener {
	slice := uSlice(u.Object, "spec", "listeners")
	out := make([]gwListener, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, gwListener{
			Name:     uStr(m, "name"),
			Port:     uInt32(m, "port"),
			Protocol: uStr(m, "protocol"),
			Hostname: uStr(m, "hostname"),
		})
	}
	return out
}

func parseAddresses(obj map[string]any, path ...string) []gwAddress {
	slice := uSlice(obj, path...)
	out := make([]gwAddress, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, gwAddress{Type: uStr(m, "type"), Value: uStr(m, "value")})
	}
	return out
}

// ---------- GatewayClass --------------------------------------------------

func emitGatewayClass(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	log.Info(event,
		"kind", "GatewayClass",
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"uid", string(u.GetUID()),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"controller", uStr(u.Object, "spec", "controllerName"),
	)
}

// ---------- HTTPRoute / GRPCRoute / TLSRoute / TCPRoute -------------------

type parentRef struct {
	Group       string `json:"group,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name,omitempty"`
	SectionName string `json:"section_name,omitempty"`
	Port        int32  `json:"port,omitempty"`
}

type backendRef struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind,omitempty"` // empty → defaults to Service
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Port      int32  `json:"port,omitempty"`
	Weight    int32  `json:"weight,omitempty"`
}

func emitRoute(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured, kind string) {
	log.Info(event,
		"kind", kind,
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"parent_refs", parseParentRefs(u),
		"hostnames", uStringSlice(u.Object, "spec", "hostnames"),
		"backends", collectRouteBackends(u),
	)
}

func parseParentRefs(u *unstructured.Unstructured) []parentRef {
	slice := uSlice(u.Object, "spec", "parentRefs")
	out := make([]parentRef, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, parentRef{
			Group:       uStr(m, "group"),
			Kind:        uStr(m, "kind"),
			Namespace:   uStr(m, "namespace"),
			Name:        uStr(m, "name"),
			SectionName: uStr(m, "sectionName"),
			Port:        uInt32(m, "port"),
		})
	}
	return out
}

// collectRouteBackends flattens backendRefs from every rule. The operator is
// a dumb collector: we don't preserve per-rule grouping (that can be done
// server-side from the raw object if needed later). We only need the
// reference set for "which Services get traffic via this route".
func collectRouteBackends(u *unstructured.Unstructured) []backendRef {
	var out []backendRef
	for _, r := range uSlice(u.Object, "spec", "rules") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		for _, x := range uSlice(rm, "backendRefs") {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, backendRef{
				Group:     uStr(m, "group"),
				Kind:      uStr(m, "kind"),
				Namespace: uStr(m, "namespace"),
				Name:      uStr(m, "name"),
				Port:      uInt32(m, "port"),
				Weight:    uInt32(m, "weight"),
			})
		}
	}
	return out
}

// ---------- helpers -------------------------------------------------------

func kindFromResource(r string) string {
	switch r {
	case "gateways":
		return "Gateway"
	case "gatewayclasses":
		return "GatewayClass"
	case "httproutes":
		return "HTTPRoute"
	case "grpcroutes":
		return "GRPCRoute"
	case "tlsroutes":
		return "TLSRoute"
	case "tcproutes":
		return "TCPRoute"
	}
	return r
}
