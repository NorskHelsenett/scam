# spam-operator

One-sentence: a cluster-wide Kubernetes watcher that emits structured JSON
records describing pods, images, and the ingress/gateway exposure graph, for
ingestion by [`NorskHelsenett/spam`](https://github.com/NorskHelsenett/spam).

## What it is

- **A dumb collector.** It watches Kubernetes resources, trims them for
  memory, and logs one JSON record per resource of interest.
- **Not a brain.** It does not join across resources, does not classify
  public-vs-private IPs, does not resolve OCI image labels, does not talk
  to registries. That all lives in `spam`.
- **Multi-cluster aware.** Every record is stamped with a `cluster` name
  (from the `CLUSTER_NAME` env var). One operator instance per cluster.

## Why it exists

`spam` wants to bind git repos to clusters and map the live exposure
surface (what FQDN reaches what pod running what image digest) so it can
pull in SBOMs / CVEs / secret-scan results per digest. The operator is
the metadata pump feeding that system.

## Docs

| File | For whom |
|---|---|
| [`architecture.md`](architecture.md) | Maintaining this operator — design choices, memory strategy, how to add a new watcher |
| [`records.md`](records.md) | Consumers of the log stream — schema and examples for every emitted record kind |
| [`spam.md`](spam.md) | Implementers on the `spam` side — what needs to be built to turn this stream into a graph |
| [`operations.md`](operations.md) | Deploying, configuring, observing, upgrading |
| [`plan.md`](plan.md) | Decision log: auth model, hijack threat mitigations, rollout order, open questions |

## Repo layout

```
main.go          bootstrap, flags, informer factories, sync + dump
collectors.go    pod/service/ingress/ingressclass watchers + emit funcs + filter logic
transforms.go    per-kind cache transforms (memory-shrinking field strippers)
gatewayapi.go    dynamic informers + parsers for gateway.networking.k8s.io/*
traefik.go       dynamic informers + parsers for traefik.io / traefik.containo.us
deploy/          ServiceAccount, ClusterRole/Binding, Deployment manifests
Dockerfile       scratch-based amd64 image, distroless-style
.devcontainer/   VS Code devcontainer (Go 1.26)
```

## Status

- Ship mode: logs to stdout. HTTP shipper to `spam` is not yet wired up
  (see `spam.md` for the API design discussion).
- Memory: ~7-8 MiB RSS watching a ~70-pod cluster with Gateway API +
  Traefik CRDs installed.
