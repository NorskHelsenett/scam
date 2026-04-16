package collector

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestExtractTraefikHosts(t *testing.T) {
	tests := []struct {
		name  string
		obj   *unstructured.Unstructured
		hosts []string
	}{
		{
			name: "single Host matcher",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"spec": map[string]any{
					"routes": []any{
						map[string]any{"match": "Host(`example.com`)"},
					},
				},
			}},
			hosts: []string{"example.com"},
		},
		{
			name: "multiple hosts in one matcher",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"spec": map[string]any{
					"routes": []any{
						map[string]any{"match": "Host(`a.com`, `b.com`)"},
					},
				},
			}},
			hosts: []string{"a.com", "b.com"},
		},
		{
			name: "HostSNI matcher",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"spec": map[string]any{
					"routes": []any{
						map[string]any{"match": "HostSNI(`tls.example.com`)"},
					},
				},
			}},
			hosts: []string{"tls.example.com"},
		},
		{
			name: "multiple routes with dedup",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"spec": map[string]any{
					"routes": []any{
						map[string]any{"match": "Host(`shared.com`) && PathPrefix(`/api`)"},
						map[string]any{"match": "Host(`shared.com`) && PathPrefix(`/web`)"},
					},
				},
			}},
			hosts: []string{"shared.com"},
		},
		{
			name: "no routes",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"spec": map[string]any{},
			}},
			hosts: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractTraefikHosts(tt.obj)
			if len(got) != len(tt.hosts) {
				t.Fatalf("got %v, want %v", got, tt.hosts)
			}
			for i, h := range tt.hosts {
				if got[i] != h {
					t.Errorf("host[%d] = %q, want %q", i, got[i], h)
				}
			}
		})
	}
}

func TestTraefikBackends(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"namespace": "web",
		},
		"spec": map[string]any{
			"routes": []any{
				map[string]any{
					"services": []any{
						map[string]any{"name": "frontend"},
						map[string]any{"name": "backend-api", "namespace": "api"},
						map[string]any{"name": "traefik-svc", "kind": "TraefikService"}, // should be skipped
					},
				},
			},
		},
	}}

	backends := TraefikBackends(u)
	if len(backends) != 2 {
		t.Fatalf("expected 2 backends, got %d: %v", len(backends), backends)
	}
	if backends[0].Namespace != "web" || backends[0].Name != "frontend" {
		t.Errorf("backend[0] = %v, want {web, frontend}", backends[0])
	}
	if backends[1].Namespace != "api" || backends[1].Name != "backend-api" {
		t.Errorf("backend[1] = %v, want {api, backend-api}", backends[1])
	}
}
