package main

import (
	"sort"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

// onAddUpdate registers Add + Update handlers on inf that gate on synced
// (dropping events during the initial sync window), type-assert to T, and
// dispatch through fn. fn receives the event name ("ADD" / "UPDATE") plus
// the new object and, on UPDATE, the previous one (nil-valued T on ADD).
//
// Delete is intentionally absent — the collector is a forward-only stream.
func onAddUpdate[T any](inf cache.SharedIndexInformer, synced *atomic.Bool, fn func(event string, newObj, oldObj T)) {
	var zero T
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if t, ok := obj.(T); ok {
				fn("ADD", t, zero)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if !synced.Load() {
				return
			}
			newT, ok := newObj.(T)
			if !ok {
				return
			}
			oldT, _ := oldObj.(T)
			fn("UPDATE", newT, oldT)
		},
	})
}

// dumpSorted emits the initial snapshot for a single kind: sort, walk, emit,
// log the counts. emit returns the number of records produced (e.g. one Pod
// emits one record per container, a filtered Service emits 0).
func dumpSorted[T any](kind string, items []T, less func(a, b T) bool, emit func(T) int) {
	sort.Slice(items, func(i, j int) bool { return less(items[i], items[j]) })
	emitted := 0
	for _, it := range items {
		emitted += emit(it)
	}
	log.Info("initial snapshot", "kind", kind, "cached", len(items), "emitted", emitted)
}

// nsNameLess orders by namespace, then name — the usual sort for namespaced
// Kubernetes objects.
func nsNameLess(ns1, n1, ns2, n2 string) bool {
	if ns1 != ns2 {
		return ns1 < ns2
	}
	return n1 < n2
}

// unstructuredList converts the informer's typed-any cache snapshot into a
// concrete slice for sort + iterate in dumpSorted.
func unstructuredList(inf cache.SharedIndexInformer) []*unstructured.Unstructured {
	objs := inf.GetIndexer().List()
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			out = append(out, u)
		}
	}
	return out
}

func lessUnstructured(a, b *unstructured.Unstructured) bool {
	return nsNameLess(a.GetNamespace(), a.GetName(), b.GetNamespace(), b.GetName())
}

// --- unstructured accessors -----------------------------------------------
//
// The stdlib shape `x, _, _ := unstructured.NestedString(...)` adds three
// tokens of ceremony to every field read. These drop the unused found/err
// returns so parsing code reads like regular struct access.

func uStr(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

func uSlice(obj map[string]any, path ...string) []any {
	s, _, _ := unstructured.NestedSlice(obj, path...)
	return s
}

func uStringSlice(obj map[string]any, path ...string) []string {
	s, _, _ := unstructured.NestedStringSlice(obj, path...)
	return s
}

func uInt32(obj map[string]any, path ...string) int32 {
	n, _, _ := unstructured.NestedInt64(obj, path...)
	return int32(n)
}
