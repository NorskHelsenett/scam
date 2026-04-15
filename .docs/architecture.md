# Architecture

Design choices, constraints, and the few non-obvious mechanisms.

## What gets watched

| Resource | Group/Version | Optional? | Why |
|---|---|---|---|
| Pod | `core/v1` | no | images + pod labels + owner ref |
| Service | `core/v1` | no | selector links pods to backends; LB/NodePort/ExternalName IPs |
| Ingress | `networking.k8s.io/v1` | no | host FQDN → service backend |
| IngressClass | `networking.k8s.io/v1` | no | controller identity |
| Gateway | `gateway.networking.k8s.io/v1*` | yes (discovery) | listener hostnames + status addresses |
| GatewayClass | `gateway.networking.k8s.io/v1*` | yes | controller identity |
| HTTPRoute / GRPCRoute / TLSRoute / TCPRoute | `gateway.networking.k8s.io/*` | yes | hostnames + backend refs |
| IngressRoute / IngressRouteTCP / IngressRouteUDP | `traefik.io/v1alpha1` (or `traefik.containo.us`) | yes | Traefik-native routing with FQDNs in `match` strings |

Optional resources are only watched if the CRD group is advertised by the
API server's discovery endpoint — see `discoverGatewayAPI()` in
`gatewayapi.go` and `discoverTraefik()` in `traefik.go`. When both new
(`traefik.io`) and legacy (`traefik.containo.us`) Traefik groups are
installed, the newer wins per resource.

**Deliberately not watched:** EndpointSlices (`spam` can recompute
pod↔service backing from `Service.spec.selector` + `Pod.metadata.labels`),
Secrets (out of scope), ConfigMaps (out of scope).

## Memory strategy

Target: single-digit MiB RSS on a normal cluster.

1. **SharedInformer with `SetTransform`** (`transforms.go`). Every
   cached object is passed through a per-kind transform function that
   nils out fields we never read — annotations, managed fields, volumes,
   env vars, probes, resources, security contexts, affinity, tolerations,
   topology constraints, status conditions. The transform mutates the
   object before it enters the cache, so the memory savings persist for
   the life of the process. Informer cache entries shrink by ~60–80%.
2. **Resync period `0`** — no periodic relists. Watch events are the
   only change signal.
3. **No parallel cache.** The operator owns zero maps/sets of its own.
   All lookups go through the informer caches.
4. **`GOMEMLIMIT=96MiB` + `GOGC=50`** keep the Go runtime conservative.
5. **Scratch base image + `-trimpath -ldflags='-s -w'`** so we start
   small and the container has nothing extraneous.

## The exposure-chain filter

Services of `type: ClusterIP` are cluster-internal noise unless they
back an externally-exposed thing. The operator drops them at emit time
unless at least one of these is true:

- An `Ingress` in the same namespace has a `backend.service.name` match.
- A Gateway API route (`HTTPRoute`/`GRPCRoute`/`TLSRoute`/`TCPRoute`)
  has a `spec.rules[].backendRefs[]` entry whose `name` (+ optional
  `namespace`) match.
- A Traefik `IngressRoute*` has a `spec.routes[].services[]` entry
  whose `name` (+ optional `namespace`) match.

Implemented in `serviceReferenced()` in `collectors.go`. Scans are linear
across the cached route objects — cheap because routes are sparse.

### The EXPOSURE refresh

When an Ingress/Route is added or updated **after** initial sync, we may
have already skipped its backend ClusterIP service. The Ingress/Route
handler therefore calls `refreshBackendServices()` to re-emit those
services with an `EXPOSURE` event label. Consumers treat `EXPOSURE` like
`UPDATE` — same UID, fresh snapshot — but the label tells them *why* the
emit happened, which is useful for debugging.

## Join keys

Every record carries a Kubernetes UID:

- Container records: `pod_uid` (the Pod's UID; container records are
  derived, they don't have their own k8s identity).
- All other records: `uid` (the object's own UID).

Name-based keys (`namespace`+`name`) are **not** reliable for joins
across time — a StatefulSet pod name gets reused with a new UID, a
deleted+recreated Ingress with the same name is a different object.
See `.docs/spam.md` for the full join-key table.

## Event types

- `INITIAL` — first emission for an object, during the startup sorted
  snapshot. Handlers are suppressed during sync so the snapshot is the
  only initial signal.
- `ADD` — object arrived post-sync.
- `UPDATE` — watched field changed.
- `EXPOSURE` — Service re-emitted because a referencing Ingress/Route
  changed (see above).

Pod → Container emission has a small diff: on `UPDATE`, containers whose
`image_spec` + `image_id` didn't change are suppressed, so digest
resolution surfaces cleanly without spam-on-every-status-tick.

## Extending: adding a new resource

1. Write a `trimXxx` transform in `transforms.go` that nils out fields
   you won't read.
2. In `collectors.go` (or a new file for a CRD):
   - `registerXxxHandler` with `AddFunc` / `UpdateFunc` guarded by the
     `synced` atomic.
   - `dumpXxx` for the initial sorted snapshot. Log
     `"initial snapshot" kind=X cached=N emitted=M` at the end.
   - `emitXxx` that produces one `slog.Info` line.
3. In `main.go`:
   - Build the informer (typed or dynamic).
   - `SetTransform(trimXxx)` on it.
   - Register the handler.
   - Add `inf.HasSynced` to the `syncs` slice.
   - Call `dumpXxx` after `WaitForCacheSync`.
4. In `deploy/rbac.yaml`, add `get/list/watch` for the resource.
5. If it references Services, wire it into `serviceReferenced()` +
   `refreshBackendServices()` so the Service filter stays correct.
6. Add a section to `records.md` documenting the new record shape.
