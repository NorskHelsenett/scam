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
POST /api/ingest/agent/records
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

## 2. Storage model: JSONB-first, views for everything else

The agent emits a high-volume, evolving-schema stream. Forcing it through a
fully normalised per-kind schema up front means migrations every time the
agent adds a field, and join code that grows with every new resource kind.

Instead: persist the raw stream as-is into one append-only JSONB table, and
express everything else (pods, services, ingresses, routes, gateways, the
exposure graph) as **views** over it. The normalised shape lives in your
query layer, not your storage layer.

### The three tables that stay normalised

Only the things with real foreign-key relationships and a stable schema:

```sql
-- Append-only audit log. Source of truth.
CREATE TABLE ingest_events (
  id          bigserial PRIMARY KEY,
  received_at timestamptz NOT NULL DEFAULT now(),
  cluster_id  uuid NOT NULL REFERENCES clusters(id),
  kind        text NOT NULL,       -- record->>'kind'     (materialised for index)
  event       text NOT NULL,       -- record->>'msg'      (INITIAL/ADD/UPDATE/DELETE/EXPOSURE)
  uid         text,                -- record->>'uid' or 'pod_uid'
  record      jsonb NOT NULL
) PARTITION BY RANGE (received_at);

-- Monthly partitions; rotate out cold partitions to archive or drop.
-- Hot retention: 90 days on the primary. Cold: S3 or a parquet export.

CREATE INDEX ON ingest_events (cluster_id, kind, uid, received_at DESC);
CREATE INDEX ON ingest_events USING gin (record jsonb_path_ops);

CREATE TABLE clusters (
  id           uuid PRIMARY KEY,
  name         text UNIQUE NOT NULL,
  status       text NOT NULL CHECK (status IN ('pending','approved','offline','archived')),
  iss          text,                -- claimed JWT issuer from first contact
  sub          text,                -- claimed SA subject
  last_seen_at timestamptz,
  approved_at  timestamptz,
  approved_by  uuid REFERENCES users(id)
);

-- Existing table; just two extra columns.
ALTER TABLE image_digests ADD COLUMN oci_labels     jsonb;
ALTER TABLE image_digests ADD COLUMN repo_binding_id bigint REFERENCES repos(id);
```

That's it. No `pods`, no `pod_containers`, no `cluster_ingresses`, no
per-route table. They're projections.

### Views, one per logical entity

```sql
-- Current state of every container.
CREATE VIEW current_containers AS
SELECT DISTINCT ON
    (cluster_id, record->>'pod_uid', record->>'container_kind', record->>'container')
  cluster_id,
  (record->>'pod_uid')        AS pod_uid,
  (record->>'namespace')      AS namespace,
  (record->>'pod')            AS pod_name,
  (record->>'container_kind') AS container_kind,
  (record->>'container')      AS container_name,
  (record->>'digest')         AS digest,
  (record->>'registry')       AS registry,
  (record->>'image')          AS image,
  (record->>'tag')            AS tag,
  (record->>'pod_phase')      AS pod_phase,
  (record->>'owner_kind')     AS owner_kind,
  (record->>'owner')          AS owner,
  record->'pod_labels'        AS pod_labels,
  event,
  received_at
FROM ingest_events
WHERE kind = 'Container'
ORDER BY cluster_id, record->>'pod_uid', record->>'container_kind',
         record->>'container', received_at DESC;

-- "Running NOW" — the source of truth for CVE-response workflows.
CREATE VIEW running_now AS
SELECT * FROM current_containers
WHERE event <> 'DELETE'
  AND pod_phase IN ('Running','Pending');

-- Analogous views per kind: current_services, current_ingresses,
-- current_routes, current_gateways, current_ingress_routes. Each uses
-- DISTINCT ON (cluster_id, uid) ORDER BY received_at DESC.
```

Start as **plain views** — zero refresh cost, always fresh. Promote hot
views to **materialised views** only when query load demands it. See §9
for the refresh strategy.

### Why this shape

- **No migrations for schema drift.** If the agent adds `pod_qos_class`
  next week, spam's views start returning it on the same day — no DB
  migration, just a view change.
- **Full audit retention comes free.** The UI's "running NOW" and the
  forensic "what ran on 2026-03-19 at 14:22 UTC" use the same table,
  one with `DISTINCT ON (...) ORDER BY received_at DESC`, the other with
  a `received_at BETWEEN ...` filter.
- **Deterministic projections.** Dropping and recreating a view costs
  nothing. If you realise a join is wrong, fix the view, don't migrate
  data.
- **Fits existing spam.** No new worker infra; existing `jobs` table
  consumes the trigger-enqueued follow-ups.

### Gotcha: retention and partitioning

At real cluster scale, `ingest_events` grows. Plan partition rotation from
day one:

- **Partition** by `received_at` month.
- **Hot retention** on the primary: 90 days. Enough for any
  incident-response query.
- **Cold export** older partitions to S3/MinIO as parquet (one file per
  month, compressed) for long-term forensics, then `DROP PARTITION`.
- **Cluster-archive** also trims: once a cluster hits `archived` status
  (see `plan.md`), its events can be truncated at a shorter TTL since the
  cluster is gone.

## 3. Deduplication: join by UID, not by name

Every record carries a stable Kubernetes UID. Use it.

| Kind | Primary join key | Fallback for display |
|---|---|---|
| Container | `(cluster_id, pod_uid, container_kind, container_name)` | `(cluster_id, namespace, pod_name, container_kind, container_name)` |
| Service / Ingress / Gateway / *Route / IngressRoute* | `(cluster_id, uid)` | `(cluster_id, namespace, name)` |
| IngressClass / GatewayClass | `(cluster_id, uid)` | `(cluster_id, name)` |

With the JSONB model, these keys drive the `DISTINCT ON (...)` in every
view. Fallback keys (namespace+name) are display-only — never use them for
joins; `kubectl delete && kubectl apply` gives you a new UID and the old
row's data.

Why the UID is primary:

- StatefulSet pod `vaultwarden-0` gets recreated → same name, **new UID**.
- Agent restarts → re-emits every object as `INITIAL` with unchanged
  UIDs. The views' `DISTINCT ON (uid)` collapses re-emissions to the
  latest automatically.
- `kubectl delete ing foo && kubectl apply -f foo.yaml` produces a new
  UID — correctly treated as a new resource.

## 4. OCI image label resolution

The agent does **not** talk to registries. It only forwards what the
Kubernetes API reported: `registry`, `image`, `tag`, `digest`,
`image_spec`, `image_id`.

`spam` must:

1. Maintain a cache keyed by `digest` (sha256). OCI image configs are
   immutable per digest — one fetch per unique digest, ever.
2. Pull the image config blob from `{registry}/{image}@{digest}` using
   the OCI Distribution API, extract `config.Labels`, store in
   `image_digests.oci_labels`.
3. Central registry credentials — ideally one central store, not
   per-cluster. The agent has no creds and doesn't need any.
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

Join chain: `.source = https://git.torden.tech/jonasbg/spam` → match
against `ProviderInstance.BaseURL = git.torden.tech` → locate/create
`Repo{provider, org: "jonasbg", slug: "spam"}` → set
`ImageDigest.repo_binding_id = Repo.id`. Then `.revision` gives the commit
SHA for hooking to a `RepoCommit`.

### Implementation options

| Approach | Pros | Cons |
|---|---|---|
| Shell out to `skopeo inspect --no-tags docker://{registry}/{image}@{digest}` | Trivial to prototype | External binary dep; auth via `--creds` / `$REGISTRY_AUTH_FILE`; subprocess overhead |
| Go lib `github.com/google/go-containerregistry` (crane) | Pure Go, tiny, no cgo | Need to wire a `Keychain` for auth |
| Go lib `github.com/containers/image` | Most feature-complete (signatures, sigstore) | Heavier, many transitive deps |

Default: `go-containerregistry`. Sketch of the worker handler:

```go
ref, _ := name.NewDigest(fmt.Sprintf("%s/%s@%s", registry, image, digest))
img, _ := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
cfg, _ := img.ConfigFile()
labels := cfg.Config.Labels  // persist as-is to image_digests.oci_labels
```

Notes:

- **Cache by digest, not tag.** Digests are content-addressed → one fetch
  per unique digest, ever.
- **Multi-arch manifests.** A tag can point to a manifest list; the
  agent already emits the per-platform digest the kubelet resolved, so
  config-by-digest fetch gives you the right platform's labels.
- **Missing labels are normal.** Base images, distroless, scratch, some
  third-party tags. Leave `repo_binding_id` NULL and expose "unlinked"
  as a UI filter; admin can bind manually.
- **Stale `revision` label.** If the image was built from a dirty tree
  the label is whatever the build tool wrote. Trust it for grouping, not
  for provenance claims.

### Trigger-driven enqueue

Let the insert into `ingest_events` fire the resolver for unknown digests:

```sql
CREATE OR REPLACE FUNCTION enqueue_oci_label_resolve() RETURNS trigger AS $$
DECLARE
  new_digest_id bigint;
BEGIN
  IF NEW.kind = 'Container' AND COALESCE(NEW.record->>'digest','') <> '' THEN
    INSERT INTO image_digests (registry, repository, digest)
    VALUES (NEW.record->>'registry', NEW.record->>'image', NEW.record->>'digest')
    ON CONFLICT (registry, repository, digest) DO NOTHING
    RETURNING id INTO new_digest_id;

    IF new_digest_id IS NOT NULL THEN
      -- FOUND means ON CONFLICT didn't hit; the digest is new. Enqueue.
      INSERT INTO jobs (type, payload, status)
      VALUES ('OCI_LABEL_RESOLVE',
              jsonb_build_object('image_digest_id', new_digest_id),
              'QUEUED');
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ingest_event_ai
AFTER INSERT ON ingest_events
FOR EACH ROW EXECUTE FUNCTION enqueue_oci_label_resolve();
```

Fits spam's existing `jobs` table and worker pattern from
`internal/runner/`.

## 5. Exposure graph computation

Same joins as before, just expressed over views instead of tables.

```
current_containers ←─┐
                     │
  current_services ──┤   matches labels ⊇ selector
                     │
  current_ingresses ─┤   backend.service.name/namespace
                     │
  current_routes ────┘   backends[].name/namespace
                     │
  current_gateways ──┤   parentRefs → Gateway → addresses
```

Concretely:

1. **Pod ↔ Service:** `current_containers.pod_labels ⊇
   current_services.selector` and same `namespace`.
2. **Service ↔ Ingress:** `current_ingresses.rules[].paths[].backend.name
   = current_services.name` (same namespace).
3. **Service ↔ Gateway Route:** route's `backends[]` names the Service.
   Cross-namespace allowed; `backends[].namespace` defaults to the
   route's namespace.
4. **Service ↔ Traefik Route:** route's `backends[]` same.
5. **Route ↔ Gateway:** `parent_refs[]` on the Route matches
   `(namespace, name)` of a Gateway.
6. **Ingress ↔ Controller:** `ingress_class` on the Ingress matches
   `name` of an IngressClass.
7. **Entry IPs:**
   - Ingress → `lb_ips` / `lb_hostnames` on the Ingress itself.
   - Gateway Route → Gateway's `addresses[]`.
   - Traefik IngressRoute → Traefik's LoadBalancer Service (visible as
     a Service with `service_type: LoadBalancer` and the Traefik app
     labels).
   - LoadBalancer / NodePort Service exposed directly → Service's
     `lb_ips` + node IPs.

Cache the joined "exposure" view (see §9). Invalidate on each new
`ingest_events` row whose `kind` is in the set that affects exposure.

## 6. Local vs public IP classification

The agent ships raw IPs. `spam` classifies. Suggested CIDR rules:

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
  `spam` can talk to an external probe, confirm reachability.
- Hostnames need DNS resolution before classification. Resolve
  server-side, cache with TTL.
- Multiple Ingresses/Gateways can front the same Service, each with
  different IPs — union their classifications.

## 7. Git repo ↔ cluster binding

The headline feature.

1. A Container record comes in with digest `D`. The trigger enqueues
   `OCI_LABEL_RESOLVE` for unknown digests.
2. Worker resolves, writes `image_digests.oci_labels`.
3. Post-resolve hook parses `org.opencontainers.image.source`, normalises
   into an existing `Repo{provider, org, slug}`, sets
   `image_digests.repo_binding_id`.
4. Inverse query: per `Repo`, join
   `repos ← image_digests.repo_binding_id ← current_containers` → list of
   clusters + pod counts. That's the "this repo deploys to these
   clusters" view.

Gotchas:

- **No `source` label** → fall back to matching `registry` + first path
  segment against known `ProviderInstance.BaseURL`s
  (`git.torden.tech/<org>/<repo>` pattern). Admins can bind manually
  for the rest.
- **Multi-arch manifests**: the label is on the per-arch config, not the
  index. Fetch by per-platform digest (which is what the agent emits).
- **Stale labels after rebuild**: commit SHA is authoritative for the
  binary you're running, even if the repo has moved on.

## 8. SBOM / vulnerability lookups

Once you have `(digest → repo, commit)`:

- Trigger an SBOM generator per digest (trivy, syft, …) on first sight.
  Add a new `JobType IMAGE_SBOM_GENERATE`; execute as a K8s Job using
  spam's existing `K8sClient` runner.
- Persist results under the digest, not the image reference. Use the
  existing `SBOM` + `SBOMBinding{AssetType: AssetTypeImageDigest}`
  tables.
- Vulnerability feed lookups by `purl`s from the SBOM. Existing OSV
  worker already handles this for repo-side SBOMs.
- Secret scans: trivy `fs` inside the image. Store findings in existing
  `RunSecret` table against a synthetic image-scan `run_id`, so the
  existing `/app/secrets` dashboard "just works".

## 9. Performance: caching, indexes, materialised views

Because spam already has lots of data, the JSONB path needs to stay
cheap. Practical tactics in priority order:

### Indexes (table-level)

- **`(cluster_id, kind, uid, received_at DESC)` btree**. This is the
  index the `DISTINCT ON (...) ORDER BY received_at DESC` pattern walks.
  Make sure query plans show an index scan, not a sort over a sequence
  scan.
- **`USING GIN (record jsonb_path_ops)`**. Cheap containment queries
  (`WHERE record @> '{"kind":"Container","pod_phase":"Running"}'`) and
  arbitrary path lookups. `jsonb_path_ops` is smaller + faster to build
  than the default `jsonb_ops`.
- **Expression indexes on hot paths**. If a view heavily filters by
  `record->>'pod_phase'` and it's not already materialised in `event`,
  consider `CREATE INDEX ON ingest_events ((record->>'pod_phase'))`.
- **Partial indexes** for narrow hot queries. Example:
  `CREATE INDEX ON ingest_events (cluster_id, uid, received_at DESC)
  WHERE kind = 'Container' AND event <> 'DELETE'`.

### Materialised views for the hot paths

Start with plain views. Watch for two symptoms:

- Dashboard queries taking >100ms.
- `current_containers` scans touching >5% of `ingest_events` size.

Then promote to materialised views:

```sql
CREATE MATERIALIZED VIEW mv_running_now AS
SELECT * FROM running_now;
CREATE UNIQUE INDEX ON mv_running_now (cluster_id, pod_uid, container_kind, container_name);
```

Refresh strategies, from cheap to expensive:

1. **Periodic `REFRESH CONCURRENTLY`** on a cron (every 30s–2min).
   Simple, stale by refresh interval, runs while queries continue.
2. **LISTEN/NOTIFY driven**. Trigger on `ingest_events` insert issues
   `NOTIFY refresh_mv`. A background worker coalesces notifications and
   issues `REFRESH`. Lower latency, slightly more infra.
3. **Custom delta tables** (incremental materialisation). A proper
   pg_ivm / logical-replication setup keeps rows up to date as events
   arrive. Heaviest to implement; consider only when (1) and (2) don't
   cut it.

### Application-level caching

- **Redis / in-process LRU** on the read path. Cache `(cluster_id,
  query_key) → result_json` with a short TTL (30–60s). Fine-grained
  invalidation is hard with JSONB; time-based is enough.
- **Digest → OCI labels** is the most valuable cache. Labels are
  immutable per digest, so cache forever (until the row is evicted
  from `image_digests`).
- **Digest → repo binding** same story.
- **CVE / OSV lookups** already cached in `component_vulnerabilities`.

### Avoid N+1 in the UI

One query returns all rows; join-heavy views should either:

- be a single view joining everything, or
- be assembled server-side in one round-trip (batch fetch by IDs).

Never per-row follow-up queries from a list render. The JSONB model
makes this easy to get wrong because `SELECT * FROM view LIMIT 50` is so
cheap-looking.

### Vacuum and autovacuum

`ingest_events` is append-only, so vacuum cost is mostly index
maintenance, not dead tuples. Still, tune autovacuum on the partitions:

- `autovacuum_vacuum_scale_factor` low (e.g. `0.01`) on hot partitions
  so ANALYZE stats stay fresh — planner needs to know how big the hot
  partition is.
- Use `pg_partman` or equivalent to manage partition creation /
  rotation automatically.

## 10. Known gotchas

- **Late reference.** An Ingress created after its backend ClusterIP
  Service may not force a fresh Service emission — the agent covers
  this by emitting `EXPOSURE` on Ingress/Route add/update. `spam` treats
  `EXPOSURE` identically to `UPDATE`.
- **Eventual consistency on restart.** On agent restart every object is
  re-emitted as `INITIAL`. Views' `DISTINCT ON (uid) ORDER BY
  received_at DESC` naturally handle this — the re-emissions replace
  the last-known row.
- **Traefik legacy group.** Clusters with both `traefik.io` and
  `traefik.containo.us` installed emit on the newer group. `spam`
  doesn't need special handling — records carry `api_version`.
- **Empty digest on ADD.** New pods emit containers with empty
  `digest` / `image_id` until kubelet finishes pulling. A follow-up
  `UPDATE` fills them in. The `DISTINCT ON` view always shows the
  latest so the transient empty row isn't exposed for long.
- **No EndpointSlice.** The agent drops EndpointSlice watching — compute
  pod↔service backing from Service.selector + Pod labels. Headless
  services with manually-populated endpoints aren't covered; flag if
  that turns out to matter for your use case.
- **Terminal pod volume.** Clusters with many CronJobs produce lots of
  `Succeeded` records. Keep the `pod_phase IN ('Running','Pending')`
  filter in `running_now`. If it still pressures the view, add a
  per-cluster config to drop terminal phases at ingest time — the
  agent already has a `pod_phase` field ready for this knob.
