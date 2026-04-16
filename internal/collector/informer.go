package collector

import (
	"sort"
	"sync/atomic"

	"k8s.io/client-go/tools/cache"
)

// OnEvents registers Add, Update, and Delete handlers on inf that gate on
// synced (dropping events during the initial sync window), type-assert to T,
// and dispatch through the callbacks.
func OnEvents[T any](inf cache.SharedIndexInformer, synced *atomic.Bool,
	addUpdate func(event string, newObj, oldObj T),
	del func(T),
) {
	var zero T
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			if t, ok := obj.(T); ok {
				addUpdate("ADD", t, zero)
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
			addUpdate("UPDATE", newT, oldT)
		},
		DeleteFunc: func(obj any) {
			if !synced.Load() {
				return
			}
			t, ok := extractDeleted[T](obj)
			if !ok {
				return
			}
			del(t)
		},
	})
}

func extractDeleted[T any](obj any) (T, bool) {
	if t, ok := obj.(T); ok {
		return t, true
	}
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if t, ok := tomb.Obj.(T); ok {
			return t, true
		}
	}
	var zero T
	return zero, false
}

// DumpSorted emits the initial snapshot for a single kind: sort, walk, emit,
// log the counts.
func DumpSorted[T any](kind string, items []T, less func(a, b T) bool, emit func(T) int) {
	sort.Slice(items, func(i, j int) bool { return less(items[i], items[j]) })
	emitted := 0
	for _, it := range items {
		emitted += emit(it)
	}
	Log.Info("initial snapshot", "kind", kind, "cached", len(items), "emitted", emitted)
}
