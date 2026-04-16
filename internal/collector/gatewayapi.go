package collector

import (
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"
)

var gwAPIGroup = "gateway.networking.k8s.io"
var gwAPIVersionRank = map[string]int{"v1": 3, "v1beta1": 2, "v1alpha2": 1}
var gwAPIResources = []string{
	"gateways", "gatewayclasses", "httproutes",
	"grpcroutes", "tlsroutes", "tcproutes",
}

// DiscoverGatewayAPI returns the GVRs that are actually present on the server.
func DiscoverGatewayAPI(disc discovery.DiscoveryInterface) []schema.GroupVersionResource {
	best := map[string]schema.GroupVersionResource{}
	rank := map[string]int{}

	for v := range gwAPIVersionRank {
		gv := gwAPIGroup + "/" + v
		rl, err := disc.ServerResourcesForGroupVersion(gv)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				Log.Debug("gateway api discovery", "group_version", gv, "err", err)
			}
			continue
		}
		if rl == nil {
			continue
		}
		parsed, _ := schema.ParseGroupVersion(gv)
		for _, r := range rl.APIResources {
			if strings.Contains(r.Name, "/") {
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

func GvrStrings(gvrs []schema.GroupVersionResource) []string {
	out := make([]string, 0, len(gvrs))
	for _, g := range gvrs {
		out = append(out, g.GroupVersion().String()+"/"+g.Resource)
	}
	return out
}

func DumpGatewayAPI(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer) {
	DumpSorted(kindFromResource(gvr.Resource), UnstructuredList(inf), LessUnstructured,
		func(u *unstructured.Unstructured) int { EmitGatewayAPI("INITIAL", gvr, u); return 1 })
}

func EmitGatewayAPI(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
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

func EmitGatewayAPIDelete(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	Log.Info("DELETE",
		"kind", kindFromResource(gvr.Resource),
		"api_version", gvr.GroupVersion().String(),
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
	)
}

// --- Gateway ---

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
	Log.Info(event,
		"kind", "Gateway",
		"api_version", gvr.GroupVersion().String(),
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"gateway_class", UStr(u.Object, "spec", "gatewayClassName"),
		"listeners", parseListeners(u),
		"spec_addresses", parseAddresses(u.Object, "spec", "addresses"),
		"addresses", parseAddresses(u.Object, "status", "addresses"),
	)
}

func parseListeners(u *unstructured.Unstructured) []gwListener {
	slice := USlice(u.Object, "spec", "listeners")
	out := make([]gwListener, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, gwListener{
			Name:     UStr(m, "name"),
			Port:     UInt32(m, "port"),
			Protocol: UStr(m, "protocol"),
			Hostname: UStr(m, "hostname"),
		})
	}
	return out
}

func parseAddresses(obj map[string]any, path ...string) []gwAddress {
	slice := USlice(obj, path...)
	out := make([]gwAddress, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, gwAddress{Type: UStr(m, "type"), Value: UStr(m, "value")})
	}
	return out
}

// --- GatewayClass ---

func emitGatewayClass(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	Log.Info(event,
		"kind", "GatewayClass",
		"api_version", gvr.GroupVersion().String(),
		"uid", string(u.GetUID()),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"controller", UStr(u.Object, "spec", "controllerName"),
	)
}

// --- Routes ---

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
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Port      int32  `json:"port,omitempty"`
	Weight    int32  `json:"weight,omitempty"`
}

func emitRoute(event string, gvr schema.GroupVersionResource, u *unstructured.Unstructured, kind string) {
	Log.Info(event,
		"kind", kind,
		"api_version", gvr.GroupVersion().String(),
		"uid", string(u.GetUID()),
		"namespace", u.GetNamespace(),
		"name", u.GetName(),
		"labels", u.GetLabels(),
		"parent_refs", parseParentRefs(u),
		"hostnames", UStringSlice(u.Object, "spec", "hostnames"),
		"backends", collectRouteBackends(u),
	)
}

func parseParentRefs(u *unstructured.Unstructured) []parentRef {
	slice := USlice(u.Object, "spec", "parentRefs")
	out := make([]parentRef, 0, len(slice))
	for _, x := range slice {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, parentRef{
			Group:       UStr(m, "group"),
			Kind:        UStr(m, "kind"),
			Namespace:   UStr(m, "namespace"),
			Name:        UStr(m, "name"),
			SectionName: UStr(m, "sectionName"),
			Port:        UInt32(m, "port"),
		})
	}
	return out
}

func collectRouteBackends(u *unstructured.Unstructured) []backendRef {
	var out []backendRef
	for _, r := range USlice(u.Object, "spec", "rules") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		for _, x := range USlice(rm, "backendRefs") {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, backendRef{
				Group:     UStr(m, "group"),
				Kind:      UStr(m, "kind"),
				Namespace: UStr(m, "namespace"),
				Name:      UStr(m, "name"),
				Port:      UInt32(m, "port"),
				Weight:    UInt32(m, "weight"),
			})
		}
	}
	return out
}

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
