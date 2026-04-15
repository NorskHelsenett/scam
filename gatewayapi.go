package main

import (
	"sort"
	"strings"
	"sync/atomic"

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

func registerGatewayAPIHandler(inf cache.SharedIndexInformer, gvr schema.GroupVersionResource, synced *atomic.Bool) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				emitGatewayAPI("ADD", gvr, u)
				if isRouteGVR(gvr) {
					refreshBackendServices(routeBackends(u))
				}
			}
		},
		UpdateFunc: func(_, newObj any) {
			if !synced.Load() {
				return
			}
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				emitGatewayAPI("UPDATE", gvr, u)
				if isRouteGVR(gvr) {
					refreshBackendServices(routeBackends(u))
				}
			}
		},
	})
}

func isRouteGVR(gvr schema.GroupVersionResource) bool {
	switch gvr.Resource {
	case "httproutes", "grpcroutes", "tlsroutes", "tcproutes":
		return true
	}
	return false
}

func dumpGatewayAPI(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer) {
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
	log.Info("initial snapshot", "kind", kindFromResource(gvr.Resource), "count", len(us))
	for _, u := range us {
		emitGatewayAPI("INITIAL", gvr, u)
	}
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
	class, _, _ := unstructured.NestedString(u.Object, "spec", "gatewayClassName")
	listeners := parseListeners(u)
	// Gateway status.addresses holds the actual assigned IPs/hostnames.
	addrs := parseAddresses(u.Object, "status", "addresses")
	// Also read spec.addresses for static assignments.
	specAddrs := parseAddresses(u.Object, "spec", "addresses")
	log.Info(event,
		"kind", "Gateway",
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"gateway_class", class,
		"listeners", listeners,
		"spec_addresses", specAddrs,
		"addresses", addrs,
	)
}

func parseListeners(u *unstructured.Unstructured) []gwListener {
	slice, _, _ := unstructured.NestedSlice(u.Object, "spec", "listeners")
	out := make([]gwListener, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		var l gwListener
		l.Name, _, _ = unstructured.NestedString(m, "name")
		if p, found, _ := unstructured.NestedInt64(m, "port"); found {
			l.Port = int32(p)
		}
		l.Protocol, _, _ = unstructured.NestedString(m, "protocol")
		l.Hostname, _, _ = unstructured.NestedString(m, "hostname")
		out = append(out, l)
	}
	return out
}

func parseAddresses(obj map[string]any, path ...string) []gwAddress {
	slice, _, _ := unstructured.NestedSlice(obj, path...)
	out := make([]gwAddress, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		var a gwAddress
		a.Type, _, _ = unstructured.NestedString(m, "type")
		a.Value, _, _ = unstructured.NestedString(m, "value")
		out = append(out, a)
	}
	return out
}

// ---------- GatewayClass --------------------------------------------------

func emitGatewayClass(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	controller, _, _ := unstructured.NestedString(u.Object, "spec", "controllerName")
	log.Info(event,
		"kind", "GatewayClass",
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"controller", controller,
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
	parents := parseParentRefs(u)
	hostnames, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "hostnames")
	backends := collectRouteBackends(u)
	log.Info(event,
		"kind", kind,
		"api_version", gvr.GroupVersion().String(),
		"cluster", cluster,
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"parent_refs", parents,
		"hostnames", hostnames,
		"backends", backends,
	)
}

func parseParentRefs(u *unstructured.Unstructured) []parentRef {
	slice, _, _ := unstructured.NestedSlice(u.Object, "spec", "parentRefs")
	out := make([]parentRef, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		var p parentRef
		p.Group, _, _ = unstructured.NestedString(m, "group")
		p.Kind, _, _ = unstructured.NestedString(m, "kind")
		p.Namespace, _, _ = unstructured.NestedString(m, "namespace")
		p.Name, _, _ = unstructured.NestedString(m, "name")
		p.SectionName, _, _ = unstructured.NestedString(m, "sectionName")
		if n, found, _ := unstructured.NestedInt64(m, "port"); found {
			p.Port = int32(n)
		}
		out = append(out, p)
	}
	return out
}

// collectRouteBackends flattens backendRefs from every rule. The operator is
// a dumb collector: we don't preserve per-rule grouping (that can be done
// server-side from the raw object if needed later). We only need the
// reference set for "which Services get traffic via this route".
func collectRouteBackends(u *unstructured.Unstructured) []backendRef {
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	var out []backendRef
	for _, r := range rules {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		refs, _, _ := unstructured.NestedSlice(rm, "backendRefs")
		for _, x := range refs {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			var b backendRef
			b.Group, _, _ = unstructured.NestedString(m, "group")
			b.Kind, _, _ = unstructured.NestedString(m, "kind")
			b.Namespace, _, _ = unstructured.NestedString(m, "namespace")
			b.Name, _, _ = unstructured.NestedString(m, "name")
			if n, found, _ := unstructured.NestedInt64(m, "port"); found {
				b.Port = int32(n)
			}
			if n, found, _ := unstructured.NestedInt64(m, "weight"); found {
				b.Weight = int32(n)
			}
			out = append(out, b)
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
