// Package collector watches Kubernetes cluster resources and emits
// per-object JSON records on stdout for ingest by SPAM.
package collector

import (
	"log/slog"
	"os"

	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	netinformers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/tools/cache"
)

// Log is the package-level structured logger. Set it from main before
// starting informers so all emitted records carry cluster identity attrs.
var Log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Refs holds informer references used for cross-resource lookups
// (e.g., "is this Service referenced by any Ingress/Route?"). Populated
// from main() after all informers are created.
var Refs struct {
	Services    coreinformers.ServiceInformer
	Ingresses   netinformers.IngressInformer
	ReplicaSets appsinformers.ReplicaSetInformer
	GWInformers map[string]cache.SharedIndexInformer // Gateway API, keyed by gvr.String()
	TRInformers map[string]cache.SharedIndexInformer // Traefik, keyed by gvr.String()
}

// BackendIndexName is the name of the cache.Indexer attached to each
// Ingress/route informer that maps "ns/svcName" keys → referring objects.
const BackendIndexName = "byBackend"

// BackendTarget identifies a Service a given Ingress/Route points at.
type BackendTarget struct{ Namespace, Name string }
