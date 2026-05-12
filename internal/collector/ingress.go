package collector

import (
	networkingv1 "k8s.io/api/networking/v1"
)

type ingPath struct {
	Path        string `json:"path,omitempty"`
	PathType    string `json:"path_type,omitempty"`
	BackendKind string `json:"backend_kind,omitempty"`
	BackendName string `json:"backend_name,omitempty"`
	BackendPort string `json:"backend_port,omitempty"`
}

type ingRule struct {
	Host  string    `json:"host,omitempty"`
	Paths []ingPath `json:"paths,omitempty"`
}

type ingTLS struct {
	Hosts  []string `json:"hosts,omitempty"`
	Secret string   `json:"secret,omitempty"`
}

func EmitIngress(event string, i *networkingv1.Ingress) {
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
						pp.BackendPort = Itoa(int64(path.Backend.Service.Port.Number))
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
	lbIPs, lbHosts := IngressLbAddresses(i.Status.LoadBalancer.Ingress)
	Log.Info(event,
		"kind", "Ingress",
		"event_id", NextEventID(),
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

func EmitIngressDelete(i *networkingv1.Ingress) {
	Log.Info("DELETE",
		"kind", "Ingress",
		"event_id", NextEventID(),
		"uid", string(i.UID),
		"namespace", i.Namespace,
		"name", i.Name,
	)
}

// --- IngressClass ---

func emitIngressClass(event string, ic *networkingv1.IngressClass) {
	Log.Info(event,
		"kind", "IngressClass",
		"event_id", NextEventID(),
		"uid", string(ic.UID),
		"name", ic.Name,
		"labels", ic.Labels,
		"controller", ic.Spec.Controller,
	)
}

func EmitIngressClass(event string, ic *networkingv1.IngressClass) {
	emitIngressClass(event, ic)
}

func EmitIngressClassDelete(ic *networkingv1.IngressClass) {
	Log.Info("DELETE",
		"kind", "IngressClass",
		"event_id", NextEventID(),
		"uid", string(ic.UID),
		"name", ic.Name,
	)
}
