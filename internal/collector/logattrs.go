package collector

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// clusterAttrs is the cluster-identity attribute set appended to every
// emitted record (cluster, cluster_id, environment, ror_metadata, ...).
// It is read at Handle time rather than baked in with Logger.With so a
// late or changed ROR identity (binding created after boot, slug → UUID
// migration) reaches subsequent records without a process restart.
var clusterAttrs atomic.Pointer[[]slog.Attr]

func init() {
	empty := []slog.Attr{}
	clusterAttrs.Store(&empty)
}

// SetClusterAttrs atomically replaces the identity attrs stamped on
// every record. Called from main at startup and from the ROR identity
// refresh loop when the binding changes.
func SetClusterAttrs(attrs []slog.Attr) {
	clusterAttrs.Store(&attrs)
}

// WithClusterAttrs wraps h so each record carries the current cluster
// identity attrs.
func WithClusterAttrs(h slog.Handler) slog.Handler {
	return clusterAttrHandler{inner: h}
}

type clusterAttrHandler struct {
	inner slog.Handler
}

func (h clusterAttrHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h clusterAttrHandler) Handle(ctx context.Context, r slog.Record) error {
	r2 := r.Clone()
	r2.AddAttrs(*clusterAttrs.Load()...)
	return h.inner.Handle(ctx, r2)
}

func (h clusterAttrHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return clusterAttrHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup note: identity attrs are appended at Handle time, so they
// would land inside an open group. Nothing in this codebase uses
// Logger.WithGroup; keep it that way or restructure this handler.
func (h clusterAttrHandler) WithGroup(name string) slog.Handler {
	return clusterAttrHandler{inner: h.inner.WithGroup(name)}
}
