package collector

import (
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ServiceReferenced reports whether any Ingress or Route points at the service.
func ServiceReferenced(ns, name string) bool {
	key := ns + "/" + name
	if Refs.Ingresses != nil {
		if objs, _ := Refs.Ingresses.Informer().GetIndexer().ByIndex(BackendIndexName, key); len(objs) > 0 {
			return true
		}
	}
	for _, inf := range Refs.GWInformers {
		if objs, _ := inf.GetIndexer().ByIndex(BackendIndexName, key); len(objs) > 0 {
			return true
		}
	}
	for _, inf := range Refs.TRInformers {
		if objs, _ := inf.GetIndexer().ByIndex(BackendIndexName, key); len(objs) > 0 {
			return true
		}
	}
	return false
}

// --- indexer key extractors ---

func IngressBackendKeys(obj any) ([]string, error) {
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

func BackendTargetsToKeys(targets []BackendTarget) []string {
	if len(targets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		k := t.Namespace + "/" + t.Name
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

func IngressBackends(ing *networkingv1.Ingress) []BackendTarget {
	var out []BackendTarget
	if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
		out = append(out, BackendTarget{ing.Namespace, ing.Spec.DefaultBackend.Service.Name})
	}
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			if p.Backend.Service != nil {
				out = append(out, BackendTarget{ing.Namespace, p.Backend.Service.Name})
			}
		}
	}
	return out
}

func RouteBackends(u *unstructured.Unstructured) []BackendTarget {
	var out []BackendTarget
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

// RefreshBackendServices re-emits each referenced Service.
func RefreshBackendServices(targets []BackendTarget) {
	if Refs.Services == nil || len(targets) == 0 {
		return
	}
	var seen map[BackendTarget]struct{}
	if len(targets) > 1 {
		seen = make(map[BackendTarget]struct{}, len(targets))
	}
	for _, t := range targets {
		if seen != nil {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
		}
		svc, err := Refs.Services.Lister().Services(t.Namespace).Get(t.Name)
		if err != nil || svc == nil {
			continue
		}
		EmitServiceForce("EXPOSURE", svc)
	}
}

// RouteBackendKeys is a cache.IndexFunc for Gateway API routes.
func RouteBackendKeys(obj any) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}
	return BackendTargetsToKeys(RouteBackends(u)), nil
}

// TraefikBackendKeys is a cache.IndexFunc for Traefik routes.
func TraefikBackendKeys(obj any) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}
	return BackendTargetsToKeys(TraefikBackends(u)), nil
}

// IsRouteGVR returns true for Gateway API route resources.
func IsRouteGVR(gvr schema.GroupVersionResource) bool {
	switch gvr.Resource {
	case "httproutes", "grpcroutes", "tlsroutes", "tcproutes":
		return true
	}
	return false
}
