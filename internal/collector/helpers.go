package collector

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

// nsNameLess orders by namespace, then name — the usual sort for namespaced
// Kubernetes objects.
func nsNameLess(ns1, n1, ns2, n2 string) bool {
	if ns1 != ns2 {
		return ns1 < ns2
	}
	return n1 < n2
}

// UnstructuredList converts the informer's typed-any cache snapshot into a
// concrete slice for sort + iterate in DumpSorted.
func UnstructuredList(inf interface{ GetIndexer() cache.Indexer }) []*unstructured.Unstructured {
	objs := inf.GetIndexer().List()
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok {
			out = append(out, u)
		}
	}
	return out
}

func LessUnstructured(a, b *unstructured.Unstructured) bool {
	return nsNameLess(a.GetNamespace(), a.GetName(), b.GetNamespace(), b.GetName())
}

// --- unstructured field accessors ---

func UStr(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

func USlice(obj map[string]any, path ...string) []any {
	s, _, _ := unstructured.NestedSlice(obj, path...)
	return s
}

func UStringSlice(obj map[string]any, path ...string) []string {
	s, _, _ := unstructured.NestedStringSlice(obj, path...)
	return s
}

func UInt32(obj map[string]any, path ...string) int32 {
	n, _, _ := unstructured.NestedInt64(obj, path...)
	return int32(n)
}

func Itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
