package collector

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

type svcPort struct {
	Name       string `json:"name,omitempty"`
	Port       int32  `json:"port"`
	TargetPort string `json:"target_port,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	NodePort   int32  `json:"node_port,omitempty"`
	AppProto   string `json:"app_protocol,omitempty"`
}

// EmitService emits a Service record. All services are included so the
// consuming system can draw the full service → pod chain, not just
// ingress-exposed ones.
func EmitService(event string, s *corev1.Service) {
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
	Log.Info(event,
		"kind", "Service",
		"event_id", NextEventID(),
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

// IngressLbAddresses is a separate type from core's LoadBalancerIngress.
func IngressLbAddresses(in []networkingv1.IngressLoadBalancerIngress) ([]string, []string) {
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

func EmitServiceDelete(s *corev1.Service) {
	Log.Info("DELETE",
		"kind", "Service",
		"event_id", NextEventID(),
		"uid", string(s.UID),
		"namespace", s.Namespace,
		"name", s.Name,
	)
}
