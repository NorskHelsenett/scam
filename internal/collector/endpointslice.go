package collector

import (
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	discoveryinformers "k8s.io/client-go/informers/discovery/v1"
)

type epEndpoint struct {
	Addresses []string `json:"addresses"`
	Ready     *bool    `json:"ready,omitempty"`
	Hostname  string   `json:"hostname,omitempty"`
	NodeName  string   `json:"node_name,omitempty"`
	Zone      string   `json:"zone,omitempty"`
}

type epPort struct {
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

func DumpEndpointSlices(inf discoveryinformers.EndpointSliceInformer) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		Log.Error("list endpointslices", "err", err)
		return
	}
	DumpSorted("EndpointSlice", list,
		func(a, b *discoveryv1.EndpointSlice) bool {
			return nsNameLess(a.Namespace, a.Name, b.Namespace, b.Name)
		},
		func(es *discoveryv1.EndpointSlice) int { EmitEndpointSlice("INITIAL", es); return 1 })
}

func EmitEndpointSlice(event string, es *discoveryv1.EndpointSlice) {
	serviceName := es.Labels["kubernetes.io/service-name"]

	endpoints := make([]epEndpoint, 0, len(es.Endpoints))
	for _, ep := range es.Endpoints {
		e := epEndpoint{
			Addresses: ep.Addresses,
			Ready:     ep.Conditions.Ready,
		}
		if ep.Hostname != nil {
			e.Hostname = *ep.Hostname
		}
		if ep.NodeName != nil {
			e.NodeName = *ep.NodeName
		}
		if ep.Zone != nil {
			e.Zone = *ep.Zone
		}
		endpoints = append(endpoints, e)
	}

	ports := make([]epPort, 0, len(es.Ports))
	for _, p := range es.Ports {
		port := epPort{}
		if p.Name != nil {
			port.Name = *p.Name
		}
		if p.Port != nil {
			port.Port = *p.Port
		}
		if p.Protocol != nil {
			port.Protocol = string(*p.Protocol)
		}
		ports = append(ports, port)
	}

	Log.Info(event,
		"kind", "EndpointSlice",
		"uid", string(es.UID),
		"namespace", es.Namespace,
		"name", es.Name,
		"labels", es.Labels,
		"service_name", serviceName,
		"address_type", string(es.AddressType),
		"endpoints", endpoints,
		"ports", ports,
	)
}

func EmitEndpointSliceDelete(es *discoveryv1.EndpointSlice) {
	Log.Info("DELETE",
		"kind", "EndpointSlice",
		"uid", string(es.UID),
		"namespace", es.Namespace,
		"name", es.Name,
	)
}
