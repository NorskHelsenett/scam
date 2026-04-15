# Record schema

Every record is a single line of JSON on stdout. Structure:

```json
{"time":"...","level":"INFO","msg":"<event>","kind":"<Kind>","cluster":"...", ...}
```

- `time` — ISO 8601 timestamp, UTC.
- `level` — always `INFO` for records; `WARN` / `ERROR` are operational.
- `msg` — the event label: `INITIAL`, `ADD`, `UPDATE`, or `EXPOSURE`.
- `kind` — what follows: `Container`, `Service`, `Ingress`, `IngressClass`,
  `Gateway`, `GatewayClass`, `HTTPRoute`, `GRPCRoute`, `TLSRoute`,
  `TCPRoute`, `IngressRoute`, `IngressRouteTCP`, `IngressRouteUDP`.
- `cluster` — value of `CLUSTER_NAME` env.

Beyond those common fields, each kind has its own shape.

## Common identifier fields

| Field | Present on | Notes |
|---|---|---|
| `uid` | all except Container | the object's Kubernetes UID |
| `pod_uid` | Container | the Pod's UID (containers have no k8s identity of their own) |
| `namespace` | all namespaced kinds | |
| `name` | all except Container | for Container, use `pod` + `container` + `container_kind` |
| `labels` | most kinds | Kubernetes labels on the object |

`IngressClass` and `GatewayClass` are cluster-scoped (no `namespace`).

---

## Container

One record **per container** inside each Pod, for init, main, and
ephemeral containers alike.

```json
{
  "msg": "INITIAL",
  "kind": "Container",
  "cluster": "talos-proxmox-cluster",
  "namespace": "vaultwarden",
  "pod_uid": "9091ca94-a615-4d70-ad00-527e475f0343",
  "owner_kind": "StatefulSet",
  "owner": "vaultwarden",
  "pod": "vaultwarden-0",
  "pod_labels": {"app": "vaultwarden", ...},
  "container_kind": "main",
  "container": "vaultwarden",
  "registry": "docker.io",
  "image": "vaultwarden/server",
  "tag": "1.35.4",
  "digest": "sha256:43498a...",
  "image_spec": "vaultwarden/server:1.35.4",
  "image_id": "docker.io/vaultwarden/server@sha256:43498a..."
}
```

Fields:

- `owner_kind` / `owner` — direct controllerRef (ReplicaSet, StatefulSet,
  DaemonSet, Job), or `"-"` for standalone pods.
- `container_kind` — `init` | `main` | `ephemeral`.
- `registry` — registry host (`docker.io`, `git.torden.tech`, `quay.io`,
  `ghcr.io`, `public.ecr.aws`, …). Empty when spec was a bare name.
- `image` — path on the registry (e.g. `vaultwarden/server`,
  `jonasbg/spam/trivy-scanner`). Docker Hub's synthetic `library/`
  prefix is stripped; other registries' `library/` is preserved.
- `tag` — may be empty if spec used only a digest.
- `digest` — `sha256:…` if known. May be empty until kubelet finishes
  pulling; a subsequent `UPDATE` fills it in.
- `image_spec` — raw PodSpec `Container.Image` string. Ground truth.
- `image_id` — raw kubelet `ContainerStatus.ImageID`. Ground truth,
  carries the authoritative digest once resolved.

**UPDATE dedup:** a Pod UPDATE only re-emits containers whose
`image_spec` or `image_id` changed. Status-only ticks are suppressed.

---

## Service

One record per Service. ClusterIP services that no Ingress/Route
references are dropped (see `architecture.md#the-exposure-chain-filter`).

```json
{
  "msg": "INITIAL",
  "kind": "Service",
  "cluster": "talos-proxmox-cluster",
  "uid": "...",
  "namespace": "blocky",
  "name": "blocky-tcp",
  "labels": null,
  "service_type": "LoadBalancer",
  "cluster_ips": ["10.111.96.230"],
  "external_ips": null,
  "external_name": "",
  "selector": {"app": "blocky"},
  "ports": [
    {"name":"dnstcp","port":53,"target_port":"53","protocol":"TCP","node_port":31111},
    {"name":"http","port":4000,"target_port":"4000","protocol":"TCP","node_port":30305}
  ],
  "lb_ips": ["10.10.10.53"],
  "lb_hostnames": null
}
```

- `service_type` — `ClusterIP` | `NodePort` | `LoadBalancer` | `ExternalName`.
- `selector` — primary join to Pod.metadata.labels.
- `lb_ips` / `lb_hostnames` — from `status.loadBalancer.ingress`. This is
  the **externally addressable** endpoint; classify these for public/
  private on the `spam` side.
- The `EXPOSURE` event fires when the Service is re-emitted because an
  Ingress/Route handler saw it as a backend — same fields, different `msg`.

---

## Ingress

```json
{
  "msg": "INITIAL",
  "kind": "Ingress",
  "cluster": "...",
  "uid": "dc78fb14-...",
  "namespace": "argocd",
  "name": "argocd-nginx",
  "labels": null,
  "ingress_class": "traefik",
  "rules": [
    {
      "host": "argocd.torden.tech",
      "paths": [
        {"path": "/", "path_type": "Prefix",
         "backend_kind": "Service", "backend_name": "argocd-server", "backend_port": "80"}
      ]
    }
  ],
  "tls": [{"hosts": ["argocd.torden.tech"], "secret": "argocd-tls-nginx"}],
  "lb_ips": ["10.10.10.80"],
  "lb_hostnames": null
}
```

- `backend_kind` is `Service` for the common case, or the CRD `Kind` if
  the backend is a resource (`Backend.Resource`).
- `backend_port` may be a port name or a number rendered as a string.

## IngressClass

```json
{"msg":"INITIAL","kind":"IngressClass","cluster":"...","uid":"...",
 "name":"traefik","labels":null,"controller":"traefik.io/ingress-controller"}
```

---

## Gateway (Gateway API)

```json
{
  "msg": "INITIAL",
  "kind": "Gateway",
  "api_version": "gateway.networking.k8s.io/v1",
  "cluster": "...",
  "uid": "...",
  "namespace": "gw-system",
  "name": "public",
  "labels": {...},
  "gateway_class": "envoy",
  "listeners": [
    {"name": "http", "port": 80, "protocol": "HTTP"},
    {"name": "https", "port": 443, "protocol": "HTTPS", "hostname": "*.torden.tech"}
  ],
  "spec_addresses": [],
  "addresses": [{"type": "IPAddress", "value": "10.10.10.80"}]
}
```

- `spec_addresses` — statically-configured addresses (rare).
- `addresses` — assigned addresses from Gateway `status.addresses`. These
  are what you classify as public/private on the `spam` side.

## GatewayClass

```json
{"msg":"INITIAL","kind":"GatewayClass","api_version":"gateway.networking.k8s.io/v1",
 "cluster":"...","uid":"...","name":"envoy","labels":null,
 "controller":"gateway.envoyproxy.io/gatewayclass-controller"}
```

## HTTPRoute / GRPCRoute / TLSRoute / TCPRoute

All four use the same shape. `hostnames` is empty for `TCPRoute`.

```json
{
  "msg": "INITIAL",
  "kind": "HTTPRoute",
  "api_version": "gateway.networking.k8s.io/v1",
  "cluster": "...",
  "uid": "...",
  "namespace": "default",
  "name": "api",
  "labels": null,
  "parent_refs": [
    {"group": "gateway.networking.k8s.io", "kind": "Gateway",
     "namespace": "gw-system", "name": "public", "section_name": "https"}
  ],
  "hostnames": ["api.example.com"],
  "backends": [
    {"kind": "Service", "namespace": "default", "name": "api", "port": 80, "weight": 1}
  ]
}
```

- `backends` is flattened across all rules. Per-rule grouping is not
  preserved — consumers needing it can re-read the raw object, but
  typically "which services does this route send traffic to" is enough.
- `kind` on a backend defaults to `"Service"` if unset.

---

## IngressRoute / IngressRouteTCP / IngressRouteUDP (Traefik)

Traefik's own route CRDs. Watched under `traefik.io/v1alpha1` (preferred)
or `traefik.containo.us/v1alpha1` (legacy).

```json
{
  "msg": "INITIAL",
  "kind": "IngressRoute",
  "api_version": "traefik.io/v1alpha1",
  "cluster": "...",
  "uid": "...",
  "namespace": "apps",
  "name": "web",
  "labels": null,
  "entry_points": ["websecure"],
  "hosts": ["example.com", "www.example.com"],
  "tls_secret": "example-cert",
  "backends": [{"namespace": "apps", "name": "web", "port": 8080}]
}
```

- `hosts` is extracted by regex from the router `match` language —
  `` Host(`…`) ``, `` HostSNI(`…`) ``, `` HostHeader(`…`) ``,
  `` HostRegexp(`…`) ``. Regex patterns are kept as-is; `spam` decides
  what to do with them.
- `IngressRouteTCP` uses `HostSNI(`…`)`; `IngressRouteUDP` has no hosts.
- `backends` only includes backends with `kind: ""` or `kind: Service`
  (Traefik-native `TraefikService` references are skipped).
