# What spam needs to build

The agent is deliberately dumb. All of the following decisions,
resolutions, and joins live server-side in `spam`. This doc lists them so
the `spam` team knows what's on their plate.

See [`records.md`](records.md) for the wire schema.

## 1. Ingest endpoint

There isn't one yet. The agent currently only writes to stdout. The
eventual shape needs to accept batches of records (one kind per record)
and carry cluster identity + an idempotency signal.

Suggested v1 API:

```
POST /api/ingest/records
Authorization: Bearer <per-cluster token>
Content-Type: application/x-ndjson

{ "msg": "INITIAL", "kind": "Container", "cluster": "talos-proxmox-cluster", ... }
{ "msg": "INITIAL", "kind": "Service",   "cluster": "talos-proxmox-cluster", ... }
...
```

Design notes:

- **Newline-delimited JSON** keeps the agent side zero-cost (each
  slog line = one payload line) and streams well through HTTP.
- **Per-cluster bearer token** in a Secret mounted to the agent Pod.
  Rotate by replacing the Secret and restarting.
- **Idempotent ingest** — see §2.
- **Batching** — agent buffers and flushes every N seconds or when
  the buffer hits K KiB. Not yet implemented.

## 2. Deduplication: join by UID, not by name

Every record carries a stable Kubernetes UID. Use it.

| Kind | Primary join key | Fallback for display |
|---|---|---|
| Container | `(cluster, pod_uid, container_kind, container)` | `(cluster, namespace, pod, container_kind, container)` |
| Service / Ingress / Gateway / *Route / IngressRoute* | `(cluster, uid)` | `(cluster, namespace, name)` |
| IngressClass / GatewayClass | `(cluster, uid)` | `(cluster, name)` |

Why the UID is the primary key:

- StatefulSet pod `vaultwarden-0` gets recreated → same name, **new UID**.
- Agent restarts → re-emits every object as `INITIAL` with unchanged
  UIDs. Without UID matching, `spam` would duplicate history.
- `kubectl delete ing foo && kubectl apply -f foo.yaml` produces a new
  UID. Correctly treated as a new resource.

`INITIAL` vs `ADD` only tells you whether the event came from the
startup snapshot or the live stream. It does **not** tell you whether
`spam` has seen the object before — that's what the UID lookup is for.

## 3. Resolving OCI image labels

The agent does **not** talk to registries. It only forwards what the
Kubernetes API reported: `registry`, `image`, `tag`, `digest`,
`image_spec`, `image_id`.

`spam` must:

1. Maintain a cache keyed by `digest` (sha256). OCI image configs are
   immutable per digest — one fetch per unique digest, ever.
2. Pull the image config blob from `{registry}/{image}@{digest}` using
   the OCI Distribution API, extract `config.Labels`, store them.
3. Central registry credentials — ideally one central store, not
   per-cluster. Agent has no creds and doesn't need any.
4. Respect `image_spec` / `image_id` as ground truth if the parsed
   fields look wrong for some exotic reference.

Labels you want:

- `org.opencontainers.image.source` — the git repo URL. Primary
  git-repo ↔ cluster binding signal.
- `org.opencontainers.image.revision` — commit SHA. Lets you bind to a
  specific `RepoCommit`.
- `org.opencontainers.image.version`, `.created`, `.title`,
  `.description`, `.url`, `.licenses` — nice to have.

### Reference: what the labels actually look like

Our own images are a worked example. `skopeo inspect` reads the image
config blob straight from the registry (no pull, no daemon) and emits
`.Labels` as JSON:

```sh
skopeo inspect docker://git.torden.tech/jonasbg/spam | jq '.Labels'
```
```json
{
  "org.opencontainers.image.created":     "2026-03-19T12:51:12.435Z",
  "org.opencontainers.image.description": "",
  "org.opencontainers.image.licenses":    "",
  "org.opencontainers.image.revision":    "b61da4b314b1d9f4bd309a2a1c6a14b4d5808c09",
  "org.opencontainers.image.source":      "https://git.torden.tech/jonasbg/spam",
  "org.opencontainers.image.title":       "spam",
  "org.opencontainers.image.url":         "https://git.torden.tech/jonasbg/spam",
  "org.opencontainers.image.version":     "main"
}
```

Join chain from there:
`.source = https://git.torden.tech/jonasbg/spam` → match against
`ProviderInstance.BaseURL = git.torden.tech` → locate/create
`Repo{provider, org: "jonasbg", slug: "spam"}` → set
`ImageDigest.repo_binding_id = Repo.id`. Then `.revision` gives the commit
SHA for hooking to a `RepoCommit`.

### Implementation options for `OCI_LABEL_RESOLVE`

Pick one, not all. All three produce the same `config.Labels` map:

| Approach | Pros | Cons |
|---|---|---|
| Shell out to `skopeo inspect --no-tags docker://{registry}/{image}@{digest}` | Trivial to prototype; matches the reference snippet above verbatim | External binary dep in the spam runner image; auth via `--creds` / `$REGISTRY_AUTH_FILE`; subprocess overhead |
| Go lib `github.com/google/go-containerregistry` (crane) | Pure Go, tiny, no cgo, same API as `gcrane`/`crane` | Need to wire a `Keychain` for registry auth |
| Go lib `github.com/containers/image` (skopeo's library) | Most feature-complete (signatures, sigstore, rich formats) | Heavier, pulls in a lot of transitive deps |

For spam's central resolver I'd default to `go-containerregistry`. Example
shape for the worker handler:

```go
ref, _ := name.NewDigest(fmt.Sprintf("%s/%s@%s", registry, image, digest))
img, _ := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
cfg, _ := img.ConfigFile()
labels := cfg.Config.Labels  // map[string]string — persist as-is to image_digests.oci_labels
```

Notes:

- **Cache by digest, not tag.** Digests are content-addressed → one fetch
  per unique digest, ever. If a repo pushes a new digest with the same
  tag, the agent emits the new digest; spam resolves the new row.
- **Multi-arch manifests.** A tag can point to a manifest list; the
  agent already emits the per-platform digest the kubelet resolved, so
  config-by-digest fetch gives you the right platform's labels directly.
- **Missing labels are fine.** Not every image sets `image.source` — base
  images, distroless, scratch, third-party tags. Leave `repo_binding_id`
  NULL and expose "unlinked" as a UI filter. Admin can still manually
  bind.
- **Stale `revision` label.** If the image was built from a dirty tree
  the label is whatever the build tool wrote. Trust it for grouping, not
  for provenance claims.

## 4. Exposure graph computation

The agent gives you the pieces. `spam` assembles them.

```
Pod ──matches labels←──── Service.selector ────referenced by────→ (Ingress | *Route | IngressRoute*)
                                                                        │
                                                                        └─ parentRef / ingress_class ─→ Gateway / Controller ──→ IPs
```

Concretely:

1. **Pod ↔ Service:** a Pod belongs to a Service if `Pod.pod_labels ⊇
   Service.selector` and the pod's namespace matches the service's.
   Empty selector means "no pod matching" (manually-managed endpoints —
   rare, not currently tracked by the agent).
2. **Service ↔ Ingress:** an Ingress exposes a Service if one of its
   `rules[].paths[]` has `backend_kind:"Service"` and
   `backend_name == Service.name` (same namespace).
3. **Service ↔ Gateway Route:** any of `HTTPRoute`/`GRPCRoute`/`TLSRoute`/
   `TCPRoute` lists the Service in `backends[]`. Cross-namespace is
   allowed (see the `namespace` field on each backend entry; defaults to
   the Route's namespace).
4. **Service ↔ Traefik Route:** same pattern using `IngressRoute*.backends[]`.
5. **Route ↔ Gateway:** from `parent_refs[]` on the Route, match by
   `(namespace, name)` to a Gateway record.
6. **Ingress ↔ Controller:** `ingress_class` on the Ingress record
   matches the `name` of an IngressClass record.
7. **Entry IPs:**
   - Ingress → `lb_ips` / `lb_hostnames` on the Ingress record itself.
   - Gateway Route → Gateway's `addresses[]`.
   - Traefik IngressRoute → depends on the Traefik LoadBalancer Service
     (which you'll see as a Service record of type LoadBalancer).
   - NodePort / LoadBalancer Service exposed directly → Service's
     `lb_ips` + cluster node IPs.

Cache the graph, invalidate on each UPDATE/EXPOSURE.

## 5. Local vs public IP classification

Agent ships raw IPs. `spam` decides what "local" means. Suggested
rules:

| CIDR | Classification |
|---|---|
| `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | local (RFC1918) |
| `100.64.0.0/10` | local-ish (cgNAT) |
| `169.254.0.0/16` | link-local |
| `fc00::/7` | local (IPv6 ULA) |
| `fe80::/10` | link-local (IPv6) |
| everything else routable | candidate public |

Caveats to capture:

- "Public IP" ≠ "reachable from the internet" — firewalls exist. If
  `spam` can talk to an external probe (e.g. an internet-origin
  prober), it can confirm reachability.
- Hostnames need DNS resolution before classification. Resolve
  server-side, cache with TTL.
- Multiple Ingresses/Gateways can front the same Service, each with
  different IPs — union their classifications.

## 6. Git repo ↔ cluster binding

This is the headline feature. A repo deploys to multiple clusters;
`spam` needs to show that.

Flow:

1. A Container record comes in with digest D.
2. `spam` resolves D's OCI labels (§3). Pulls
   `org.opencontainers.image.source`.
3. Normalize into an existing `Repo` record (matching provider + org +
   slug — see spam's internal/assets/models.go:27-58).
4. Record a `(cluster, namespace, pod_uid) → (repo, commit)` link.
5. Inverse index: per `Repo`, list clusters that are currently running
   any image whose source points to that repo. That's the "this repo
   deploys to these clusters" view.

Gotchas:

- Images without the `source` label: fall back to the `image_spec`
  registry path if it matches a known vanity-URL pattern
  (`git.torden.tech/<org>/<repo>`), otherwise unknown.
- Multi-arch manifests: the label is on the per-arch config, not the
  index. Fetch by digest straight, not via tag.
- Stale labels after rebuild: commit SHA in the label is authoritative
  for the binary you're running, even if the repo has moved on.

## 7. SBOM / vulnerability lookups

Once you have `(digest → repo, commit)`:

- Trigger an SBOM generator per digest (trivy, syft, …) on first sight.
- Persist results under the digest, not the image reference.
- `spam`'s existing `ImageDigest` entity in
  `internal/assets/models.go:27-58` is the right home.
- Vulnerability feed lookups by `purl`s from the SBOM; trivy's DB works.
- Secret scans are best done against the filesystem layers — scan once
  per digest, store findings per digest.

## 8. Known gotchas

- **Late reference.** An Ingress created after its backend ClusterIP
  Service may not force a fresh Service emission — the agent covers
  this by emitting `EXPOSURE` on Ingress/Route add/update, re-fetching
  each referenced Service from the cache. `spam` should handle
  `EXPOSURE` identically to `UPDATE`.
- **Eventual consistency on restart.** If the agent restarts, every
  object is re-emitted as `INITIAL`. `spam` should dedup by UID, not
  treat each `INITIAL` as a new observation.
- **Traefik legacy group.** Clusters with both `traefik.io` and
  `traefik.containo.us` installed will see records on the newer group
  only. `spam` doesn't need to worry about this — it just sees
  `api_version` on each record.
- **Empty digest on ADD.** New pods emit containers with empty
  `digest` / `image_id` until the kubelet finishes pulling. A
  follow-up `UPDATE` fills them in.
- **No EndpointSlice.** The agent drops EndpointSlice watching —
  compute pod↔service backing from Service.selector + Pod labels.
  Headless services with manually-populated endpoints aren't covered;
  if that matters for your use case, reopen the decision.
