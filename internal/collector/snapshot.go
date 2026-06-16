package collector

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	coreinformers "k8s.io/client-go/informers/core/v1"
	discoveryinformers "k8s.io/client-go/informers/discovery/v1"
	netinformers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/tools/cache"
)

const defaultSnapshotInterval = 6 * time.Hour

// SnapshotResources bundles every informer touched by a full reconcile
// snapshot. Populated once in main() after cache sync, then passed by
// value to EmitFullSnapshot and SnapshotLoop.
type SnapshotResources struct {
	Pods           coreinformers.PodInformer
	Services       coreinformers.ServiceInformer
	EndpointSlices discoveryinformers.EndpointSliceInformer
	Ingresses      netinformers.IngressInformer
	IngressClasses netinformers.IngressClassInformer
	GatewayAPI     []DynamicSnapshot
	Traefik        []DynamicSnapshot
}

// DynamicSnapshot pairs a GVR with its informer for the unstructured
// snapshot path (Gateway API and Traefik resources).
type DynamicSnapshot struct {
	GVR schema.GroupVersionResource
	Inf cache.SharedIndexInformer
}

// BuildDynamicSnapshots zips a slice of GVRs with the gvr-string keyed
// informer map main() already constructs, so per-GVR iteration order is
// stable across runs.
func BuildDynamicSnapshots(gvrs []schema.GroupVersionResource, infs map[string]cache.SharedIndexInformer) []DynamicSnapshot {
	out := make([]DynamicSnapshot, 0, len(gvrs))
	for _, gvr := range gvrs {
		if inf, ok := infs[gvr.String()]; ok {
			out = append(out, DynamicSnapshot{GVR: gvr, Inf: inf})
		}
	}
	return out
}

// newSnapshotID returns a 32-char hex string: 8 bytes big-endian unix
// milliseconds + 8 random bytes. Lex-sortable by emission time, no
// external deps.
func newSnapshotID() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli()))
	_, _ = cryptorand.Read(b[8:])
	return hex.EncodeToString(b[:])
}

// EmitFullSnapshot emits SNAPSHOT_BEGIN, one SNAPSHOT per tracked kind,
// then SNAPSHOT_END. snapshotType is "init" on startup and "reconcile"
// from SnapshotLoop (or ACK-mismatch trigger once that lands).
func EmitFullSnapshot(rs SnapshotResources, snapshotType string) {
	id := newSnapshotID()
	Log.Info("SNAPSHOT_BEGIN",
		"kind", "Snapshot",
		"event_id", NextEventID(),
		"snapshot_id", id,
		"snapshot_type", snapshotType,
	)
	if rs.Pods != nil {
		EmitContainerSnapshot(rs.Pods, id)
	}
	if rs.Services != nil {
		EmitServiceSnapshot(rs.Services, id)
	}
	if rs.EndpointSlices != nil {
		EmitEndpointSliceSnapshot(rs.EndpointSlices, id)
	}
	if rs.Ingresses != nil {
		EmitIngressSnapshot(rs.Ingresses, id)
	}
	if rs.IngressClasses != nil {
		EmitIngressClassSnapshot(rs.IngressClasses, id)
	}
	for _, ds := range rs.GatewayAPI {
		EmitGatewayAPISnapshot(ds.GVR, ds.Inf, id)
	}
	for _, ds := range rs.Traefik {
		EmitTraefikSnapshot(ds.GVR, ds.Inf, id)
	}
	Log.Info("SNAPSHOT_END",
		"kind", "Snapshot",
		"event_id", NextEventID(),
		"snapshot_id", id,
	)
}

// SnapshotLoop fires the periodic safety-net reconcile snapshot. Cadence
// is SNAPSHOT_INTERVAL (default 6h), randomly jittered on the first fire
// so a fleet of agents doesn't align. Per-cluster jitter holds across
// subsequent fires because each agent's first fire is at a random offset.
func SnapshotLoop(ctx context.Context, rs SnapshotResources) {
	interval := resolveSnapshotInterval()
	if interval <= 0 {
		Log.Info("snapshot: disabled")
		return
	}

	firstDelay := interval
	if interval > time.Minute {
		firstDelay = time.Duration(rand.Int64N(int64(interval)))
	}
	Log.Info("snapshot: scheduled", "interval", interval, "first_delay", firstDelay)

	timer := time.NewTimer(firstDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		EmitFullSnapshot(rs, "reconcile")
		timer.Reset(interval)
	}
}

func resolveSnapshotInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SNAPSHOT_INTERVAL"))
	if raw == "" {
		return defaultSnapshotInterval
	}
	if strings.EqualFold(raw, "off") || strings.EqualFold(raw, "disabled") || raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Minute {
		Log.Warn("snapshot: invalid SNAPSHOT_INTERVAL, using default",
			"value", raw, "default", defaultSnapshotInterval)
		return defaultSnapshotInterval
	}
	return d
}

// --- per-kind snapshot helpers ---

// uidKeys collects the resource_keys list for emitSnapshotKeys.
func uidKeys[T metav1.Object](list []T) []string {
	keys := make([]string, 0, len(list))
	for _, o := range list {
		keys = append(keys, string(o.GetUID()))
	}
	return keys
}

func EmitServiceSnapshot(inf coreinformers.ServiceInformer, snapshotID string) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		Log.Error("snapshot: list services", "err", err)
		return
	}
	DumpSorted("Service", list,
		func(a, b *corev1.Service) bool { return nsNameLess(a.Namespace, a.Name, b.Namespace, b.Name) },
		func(s *corev1.Service) int { EmitService("INITIAL", s); return 1 })
	emitSnapshotKeys(snapshotID, "Service", uidKeys(list))
}

func EmitEndpointSliceSnapshot(inf discoveryinformers.EndpointSliceInformer, snapshotID string) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		Log.Error("snapshot: list endpointslices", "err", err)
		return
	}
	DumpSorted("EndpointSlice", list,
		func(a, b *discoveryv1.EndpointSlice) bool {
			return nsNameLess(a.Namespace, a.Name, b.Namespace, b.Name)
		},
		func(es *discoveryv1.EndpointSlice) int { EmitEndpointSlice("INITIAL", es); return 1 })
	emitSnapshotKeys(snapshotID, "EndpointSlice", uidKeys(list))
}

func EmitIngressSnapshot(inf netinformers.IngressInformer, snapshotID string) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		Log.Error("snapshot: list ingresses", "err", err)
		return
	}
	DumpSorted("Ingress", list,
		func(a, b *networkingv1.Ingress) bool { return nsNameLess(a.Namespace, a.Name, b.Namespace, b.Name) },
		func(i *networkingv1.Ingress) int { EmitIngress("INITIAL", i); return 1 })
	emitSnapshotKeys(snapshotID, "Ingress", uidKeys(list))
}

func EmitIngressClassSnapshot(inf netinformers.IngressClassInformer, snapshotID string) {
	list, err := inf.Lister().List(labels.Everything())
	if err != nil {
		Log.Error("snapshot: list ingressclasses", "err", err)
		return
	}
	DumpSorted("IngressClass", list,
		func(a, b *networkingv1.IngressClass) bool { return a.Name < b.Name },
		func(ic *networkingv1.IngressClass) int { EmitIngressClass("INITIAL", ic); return 1 })
	emitSnapshotKeys(snapshotID, "IngressClass", uidKeys(list))
}

func EmitGatewayAPISnapshot(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer, snapshotID string) {
	list := UnstructuredList(inf)
	kind := kindFromResource(gvr.Resource)
	DumpSorted(kind, list, LessUnstructured,
		func(u *unstructured.Unstructured) int { EmitGatewayAPI("INITIAL", gvr, u); return 1 })
	emitSnapshotKeys(snapshotID, kind, uidKeys(list))
}

func EmitTraefikSnapshot(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer, snapshotID string) {
	list := UnstructuredList(inf)
	kind := traefikKindFromResource(gvr.Resource)
	DumpSorted(kind, list, LessUnstructured,
		func(u *unstructured.Unstructured) int { EmitTraefik("INITIAL", gvr, u); return 1 })
	emitSnapshotKeys(snapshotID, kind, uidKeys(list))
}

func emitSnapshotKeys(snapshotID, targetKind string, keys []string) {
	Log.Info("SNAPSHOT",
		"kind", "Snapshot",
		"event_id", NextEventID(),
		"snapshot_id", snapshotID,
		"target_kind", targetKind,
		"resource_keys", keys,
	)
}
