# Spam integration — plan & open decisions

A decision log of the spam-agent → `NorskHelsenett/spam` rollout, written
for the people doing the work. Pair with:

- [`records.md`](records.md) — the wire schema every record follows
- [`spam.md`](spam.md) — what `spam` must implement to consume the stream
- [`architecture.md`](architecture.md) — why the agent looks the way it does

This document captures the *journey* (what we tried, what we rejected, why)
and the *live questions* where we haven't fully landed yet.

## What's already landed in the agent

Working today, deployed to `talos-proxmox-cluster`, ~9 MiB RSS:

| Feature | File | Why it matters |
|---|---|---|
| Pod / Service / Ingress / IngressClass watchers | `collectors.go` | Baseline inventory |
| Gateway API watchers (dynamic + discovery probe) | `gatewayapi.go` | Optional, only if CRDs exist |
| Traefik IngressRoute/TCP/UDP watchers | `traefik.go` | Traefik-native routing with FQDNs in `match` strings |
| Exposure-chain filter | `collectors.go:serviceReferenced` | Drops noise: ClusterIP Services that nothing routes to |
| `byBackend` cache indexer | `collectors.go:423` + route files | O(1) "is this Service exposed?" lookups |
| `samePodImages` fast path | `collectors.go:samePodImages` | Zero-alloc on status-churn pod updates |
| Stable UID on every record | all emit funcs | Join key that survives name reuse |
| Raw `image_spec` + `image_id` alongside parsed values | `collectors.go:emitPod` | Ground truth if our parse is ever wrong |
| Per-container `pod_phase` | `collectors.go:emitPod` | Tells spam whether a pod is actually running, completed, failed |
| DELETE events on every informer | `helpers.go:onEvents` | Sub-minute accuracy for "what's running NOW" |

Pending in the agent:

- [ ] HTTP shipper replacing stdout (POST ndjson to spam).
- [ ] Heartbeat goroutine.
- [ ] Projected ServiceAccount token volume in the Deployment manifest.
- [ ] Two `NetworkPolicy` manifests — default-deny + egress-to-spam-only.

## Auth & registration: the decision

### The problem

- Clusters appear and disappear automatically — the agent ships inside an
  umbrella chart, not by humans.
- `spam` is the central ingest; it has to know *which* cluster is speaking.
- `spam` has to decide whether to trust the claim.
- We cannot ship a long-lived secret inside the agent's container image
  (public, reverse-engineerable, same value everywhere).
- A deployment-time secret mounted via GitOps isn't really a secret either —
  anyone who can read the umbrella repo or the cluster's Secrets can forge
  it.
- A full OIDC/OAuth flow with JWKS cache + client registration inside spam
  is correct-by-the-book but adds infrastructure we don't have yet.

### Options considered

| Option | Auth strength | Admin effort | Infra cost | Verdict |
|---|---|---|---|---|
| A. Pre-shared secret in a GitOps-templated Secret | Low (the secret is a capability anyone with read access to umbrella config can replay) | None ongoing | None | Rejected. The security story doesn't survive a single leaked config. |
| B. Projected SA JWT + spam verifies signature via OIDC discovery (JWKS fetch) | High, crypto-grade | None ongoing | spam needs *network* path to every cluster's API server | Rejected for v1. Most clusters' API servers aren't reachable from spam's network. |
| C. Projected SA JWT as self-asserted identity + admin approval gate + per-cluster token post-approval | Medium; the human is the root of trust | One approval per new cluster, which is fine because clusters are stable | None | **Chosen.** |
| D. Private PKI + client certs issued by spam to each cluster | High | Cert rotation to design, CA to operate | Full-blown PKI | Parked. Good upgrade path for later. |
| E. SPIFFE/SPIRE federation | Very high | None ongoing | SPIRE deployment + federation config | Overkill for the first iteration. |

### The chosen flow

```
┌───────────────────────────────────────────────────────────────────┐
│ inside a fresh cluster                                            │
│                                                                   │
│   umbrella Helm chart installs spam-agent                      │
│   │                                                               │
│   ServiceAccount "spam-agent"                                  │
│   │ projected token volume with audience: spam.nhn.no             │
│   │ (kubelet rotates every hour, no human touches it)             │
│   ▼                                                               │
│   agent POST → https://spam.nhn.no/api/ingest/agent/records │
│   Authorization: Bearer <projected SA JWT>                        │
└───────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌───────────────────────────────────────────────────────────────────┐
│ spam                                                              │
│                                                                   │
│   1. decode JWT claims (sub, iss, aud) — signature NOT verified   │
│   2. extract (iss, sub) as cluster identity hint                  │
│   3. look up clusters where (iss, sub) = approved                 │
│                                                                   │
│      ┌─── approved ────┐          ┌─── unknown ────┐              │
│      │                 │          │                │              │
│      │ accept records  │          │ create row     │              │
│      │ bump last_seen  │          │ status=pending │              │
│      │                 │          │ rate-limit hard│              │
│      │                 │          │ notify admin   │              │
│      └─────────────────┘          └────────────────┘              │
│                                                                   │
│   admin reviews pending ──► approves ──► issues per-cluster token │
│                                          for the cluster's Secret │
└───────────────────────────────────────────────────────────────────┘
                                │
                                ▼
                 agent picks up token on next restart,
                 prefers it over the SA JWT going forward
```

Post-approval, the per-cluster token is what the agent sends. It's
rotatable (spam can invalidate + re-issue), revocable (spam drops it and the
cluster goes dark), and scoped (bound to one `clusters.id`). If the token
leaks, blast radius is one cluster's audit log.

### The hijack threat — chance low but never zero

You asked directly: *what do we do about someone hijacking the endpoint?*

**Threat inventory.**

| # | Threat | Impact | Likelihood |
|---|---|---|---|
| T1 | Rogue cluster phones home, registers as new tenant, pollutes graph | Garbage data, admin time wasted | Low (attacker needs network path to spam + knowledge of URL) |
| T2 | Rogue cluster impersonates an **existing** approved cluster's `(iss, sub)` claims | Could overwrite truth of a real cluster | Very low — requires knowing the victim's token structure; rejected if token doesn't match registered one |
| T3 | Compromised umbrella Git repo plants an agent with malicious code | Legit cluster, malicious payload | Out of scope — supply-chain problem upstream of us |
| T4 | Leaked per-cluster token replayed from outside the legit cluster | Attacker can forge records for that one cluster | Low; revoke on detection |
| T5 | DoS via bulk fake registrations | Fill the pending queue, admin notification storm | Medium if unmitigated |
| T6 | Someone reads the umbrella's GitOps-templated Secret | Bearer-token access = registered-cluster access | Medium — scope defines blast radius |

**Mitigations layered.**

1. **Admin approval gate.** A human confirms "yes, cluster `foo-prod` is
   ours, that's the right ServiceAccount subject, approve". Admin reviews
   the captured claims + the first batch of records visible in the pending
   preview. Gate on suspicious claims — wrong `sub`, surprising `iss`
   domain, records referencing unfamiliar workloads.
2. **Aggressive rate limit on pending clusters.** Unapproved → at most N
   records/minute, dropped above that, no reads. DoS budget is bounded.
3. **Per-cluster token after approval.** Not environment-wide — scoped to
   one `clusters.id`. Revocable. Rotatable.
4. **Cluster-name uniqueness.** If cluster `foo-prod` already exists,
   registration as `foo-prod` requires matching `(iss, sub)` on file.
   Collision → rejected + alert.
5. **Mismatch sanity checks.** Auto-reject if `sub` is not
   `system:serviceaccount:spam-agent:spam-agent` (i.e., not our
   agent's SA), or `aud` is not the expected `spam.nhn.no`. A malicious
   actor deploying their own agent with their own SA gets auto-rejected
   at the claims level.
6. **Audit log.** Every registration attempt (approved, pending, rejected,
   rate-limited) captured with claim dump + source IP + timestamp. Admin
   can reconstruct a timeline.
7. **Optional upgrade path: JWKS verification.** When spam *can* reach a
   cluster's API server (e.g., same network), add signature verification.
   Turns T1/T4 from "admin-gated" to "cryptographically impossible". Leave
   the plumbing in place so this is a switch, not a rewrite.

**Honest about limits.**

You cannot make first-contact cryptographically impossible to forge without
one of: (a) spam reaching the cluster for JWKS, (b) a pre-shared secret, or
(c) a centralized PKI. All three have real costs. The admin-approval model
punts the trust decision to a human, which is the correct place for a
low-volume event (clusters arrive rarely). The layered mitigations keep the
cost of dealing with pending noise bounded.

If the approval volume starts mattering, the upgrade path is well-defined:
turn on JWKS verification for clusters whose API servers are network-
reachable, and fall back to admin approval for the ones that aren't.

## What spam implements, TL;DR

See [`spam.md`](spam.md) for the full schema contract. The new pieces
implied by this plan:

**Tables**

- `clusters` — one row per observed cluster. `status ∈ pending | approved |
  rejected | archived`. Identity claim (`iss`, `sub`), last seen, notes.
- `cluster_tokens` — per-cluster bearer token (hashed). Issued on approval,
  rotatable.
- `pods`, `pod_containers`, `cluster_ingresses`, `cluster_services`,
  `cluster_routes`, `cluster_gateways` — as described in `spam.md`.
- Extend `image_digests`: add `oci_labels` JSONB + `repo_binding_id` FK.

**Endpoints**

- `POST /api/ingest/agent/records` — ndjson body, bearer-token auth,
  rate-limited.
- `POST /api/ingest/agent/heartbeat` — keeps `clusters.last_seen_at`
  fresh.
- Admin: `GET/POST /api/admin/clusters` — list pending + approve/reject,
  issue/rotate token.

**Workers (jobs)**

- `OCI_LABEL_RESOLVE` — fires on every new digest insert. Pulls image
  config blob, extracts `org.opencontainers.image.source` →
  `repo_binding_id`. Cache by digest so it's one fetch per unique image.

**Background tasks**

- Cluster GC cron: `last_seen_at > 24h` → `status=offline`; `> 7d` →
  `status=archived`. Never hard-delete — audit preserved.
- Agent-restart reconcile: on ingest `reconcile_begin` marker, mark
  `last_seen_at` stale for that cluster; after replay completes, anything
  not re-asserted is soft-deleted. Belt for suspenders when DELETE events
  get lost.

**UI**

No new top-level nav bloat. Concretely:

- One new top-level section: `/app/clusters` (list + detail + approval).
- Extend `/app/providers/repo/{id}` with a "Deployed to" right-sidebar panel.
- New or extended `/app/images/{digest}` showing OCI labels, linked repo,
  and cluster occupancy.
- `/app/admin/clusters` for approve/reject + token rotation. Mirrors the
  `/app/admin/providers` pattern already in spam.

## Rollout order

Bite-sized, each step shippable on its own:

1. **Agent transport**: swap stdout for HTTP POST ndjson. Config:
   `SPAM_URL` env, `/var/run/secrets/tokens/spam` token path. Token
   precedence: per-cluster token file > projected SA JWT.
2. **Agent heartbeat**: goroutine, 60s tick, `{cluster, summary counts}`.
3. **Agent NetworkPolicy**: default-deny + egress to spam only + DNS +
   kube-apiserver.
4. **spam schema**: `clusters`, `cluster_tokens`, `pods`, `pod_containers`,
   `cluster_ingresses`. Extend `image_digests`.
5. **spam ingest**: `/api/ingest/agent/records` + heartbeat + pending
   flow (rate-limited, auto-create rows, admin notify).
6. **spam admin approval**: `/app/admin/clusters` page, approval action,
   token issuance.
7. **spam `OCI_LABEL_RESOLVE` worker**: extend `image_digests.oci_labels` +
   `repo_binding_id`.
8. **spam cluster detail page**: `/app/clusters/{name}` with deployments
   tree + exposure panel.
9. **spam "Deployed to" panel on repo detail**: the first user-visible
   payoff — "where does this repo run?".
10. **spam GC cron**: cluster offline/archive transitions.
11. **spam reconcile marker on agent restart** (belt for suspenders).

Later (not this rollout):

- Service / route / gateway ingest tables → exposure-graph view.
- `IMAGE_SBOM_GENERATE` / `IMAGE_SECRET_SCAN` job types.
- Public-vs-local IP classification rules.
- Cosign image signature verification on the agent side.
- JWKS verification when spam can network-reach the cluster API server.

## Open questions

- **Per-cluster token delivery.** Admin approves → spam generates token →
  … how does it reach the cluster? Options: (a) admin pastes it into a
  Secret manifest committed to the umbrella's cluster config, (b) spam
  exposes a one-shot retrieval URL that the agent can hit while still
  pending (and only for its own cluster claim), (c) write the token back
  via the umbrella's own Git flow. Decide before rollout step 6.
- **Heartbeat summary content.** How much does the heartbeat carry? At
  minimum cluster name + timestamp. Including per-kind counts makes it a
  cheap consistency check (detect drift between agent and spam), but
  bumps the payload. Default: counts included.
- **Rate limits.** What's the budget for pending clusters? Proposal: 30
  records/minute, 10 heartbeats/minute, drop with 429 above that. Tuneable
  in spam config.
- **Reconcile marker wire format.** Agent sends
  `{"msg":"RECONCILE_BEGIN","cluster":"..."}` at startup before the INITIAL
  batch? Or a special HTTP header on the first POST of a restart? Header
  is cleaner, but needs the transport layer wired first.
- **Terminal pod emits.** Current design keeps `pod_phase` visible on every
  record so spam can filter in queries. If the volume of `Succeeded`
  records from fast-churning cron-job clusters becomes ugly, add an
  agent flag `OMIT_TERMINAL_PHASES=Succeeded,Failed`. YAGNI until
  proven.
- **Image signature verification.** Cosign-verify the agent image at
  ingest time? It proves "that's my code speaking" but not "that's my
  cluster speaking". Deferred to later iterations once the simpler
  identity story is stable.

## Summary

- Agent is done and running. Stream is clean: ADD / UPDATE / DELETE,
  every record UID-keyed, pod phase tracked for "running NOW" queries,
  image ground-truth preserved in `image_spec` + `image_id`.
- Auth model: projected SA JWT as identity hint, admin approval gate,
  per-cluster token post-approval. Layered mitigations for endpoint hijack
  acknowledge that first-contact can't be cryptographically bulletproof
  without extra infrastructure.
- Rollout is sequenced so each step ships a user-visible bit of value.
- Major open question before shipping is how per-cluster tokens get back
  into the cluster post-approval — solvable, just needs a call.
