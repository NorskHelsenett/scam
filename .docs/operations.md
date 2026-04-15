# Operations

How to deploy, configure, observe, and upgrade the agent.

## Configuration

Only what runs in production is listed here.

| Env var | Required | Default | Purpose |
|---|---|---|---|
| `CLUSTER_NAME` | **yes** | `""` | Stamped on every record as `cluster`. Must match the identity `spam` knows this cluster by. Warns loudly if empty. |
| `GOGC` | no | `50` | Aggressive Go GC; trades a little CPU for a smaller heap. |
| `GOMEMLIMIT` | no | `96MiB` | Soft memory ceiling for the Go runtime. |
| `KUBECONFIG` | no | — | Only used when running out-of-cluster. |

Flag:

- `-kubeconfig <path>` — only honoured when not in-cluster. The
  agent tries in-cluster config first and falls back to kubeconfig.

## RBAC

`deploy/rbac.yaml` defines a cluster-scoped `ClusterRole` with
`get/list/watch` on:

- `pods`, `services` (core)
- `ingresses`, `ingressclasses` (`networking.k8s.io`)
- `gateways`, `gatewayclasses`, `httproutes`, `grpcroutes`, `tlsroutes`,
  `tcproutes` (`gateway.networking.k8s.io`) — harmless if the CRDs
  aren't installed; the agent's discovery probe skips missing
  resources.
- `ingressroutes`, `ingressroutetcps`, `ingressrouteudps` (`traefik.io`
  and `traefik.containo.us`) — same deal.

No write permissions, ever.

## Deployment

`deploy/deployment.yaml`:

- **Replicas: 1.** The agent emits events; running two would just
  duplicate the stream without any coordination value.
- **Strategy: Recreate.** Same reason.
- **Security context:** `runAsNonRoot`, UID 65532, read-only root fs,
  `RuntimeDefault` seccomp, dropped capabilities. Plays nicely with
  Pod Security Admission `restricted`.
- **Resource requests/limits:** 10m CPU / 32 MiB request, 128 MiB
  limit. With 66 pods + ~25 services + Gateway API + Traefik CRDs the
  observed RSS sits around 7–8 MiB.
- **Image pull policy: `Always`.** The image tag is `:latest` during
  early development. Pin to a digest before long-lived deployments.

Apply:

```sh
kubectl apply -f deploy/rbac.yaml -f deploy/deployment.yaml
```

## Observing

Everything is JSON on stdout.

- Operational messages (`msg: "waiting for cache sync"`,
  `"streaming events"`, `"initial snapshot"`) — no `kind` field.
- Records — see `records.md`. Every record has `kind`.

Useful greps:

```sh
# Live tail of Container records only
kubectl -n spam-agent logs -f deployment/spam-agent \
  | jq -c 'select(.kind=="Container")'

# Every FQDN the agent has seen
kubectl -n spam-agent logs deployment/spam-agent \
  | jq -r 'select(.kind=="Ingress") | .rules[].host,
           select(.kind=="HTTPRoute" or .kind=="GRPCRoute" or .kind=="TLSRoute") | .hostnames[],
           select(.kind=="IngressRoute" or .kind=="IngressRouteTCP") | .hosts[]' \
  | sort -u

# Kind counts
kubectl -n spam-agent logs --tail=5000 deployment/spam-agent \
  | jq -r 'select(.kind) | .kind' | sort | uniq -c | sort -rn
```

## Upgrading

1. Build and push a new image.
2. `kubectl -n spam-agent rollout restart deployment/spam-agent`.
3. On restart the agent re-emits everything as `INITIAL`. `spam`'s
   ingest must dedup by UID — `spam.md` §2.

No migration dances; no CRDs owned by this agent; nothing stateful.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `load kube config … no in-cluster config and no kubeconfig given` | Running out-of-cluster without `-kubeconfig`. |
| `cache sync aborted` | Context was cancelled (SIGTERM) before the first list+watch cycle completed. Happens on restart under load; retry. |
| `CLUSTER_NAME is empty; …` | `CLUSTER_NAME` env is unset. Records will still flow but carry `cluster:""`. |
| Forbidden on some kind | RBAC gap. `kubectl auth can-i watch <kind> --as=system:serviceaccount:spam-agent:spam-agent`. |
| Gateway / Traefik records missing | CRDs aren't installed, or the discovery probe didn't see the group. Check `kubectl get crd`. |
| Memory climbing | Shouldn't happen; informer caches are bounded by the Kubernetes object graph. If you see it, capture a `pprof` heap profile — there is no pprof endpoint currently; add one behind a flag if you need to debug. |

## Development

Devcontainer: `.devcontainer/devcontainer.json` pins
`mcr.microsoft.com/devcontainers/go:2-1.26-trixie` and adds Helm.

Build inside the devcontainer:

```sh
go build -buildvcs=false -o /tmp/spam-agent .
go vet ./...
```

Image build (amd64, even on arm64 hosts):

```sh
podman build --platform linux/amd64 -t git.torden.tech/jonasbg/spam-agent:latest .
podman push git.torden.tech/jonasbg/spam-agent:latest
```
