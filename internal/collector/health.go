package collector

import (
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// processStart is captured at package load so uptime is measured from as
// close to process start as we can get without threading a timestamp
// through main.
var processStart = time.Now()

// clockTicksPerSec is the assumed _SC_CLK_TCK. It is effectively always
// 100 on Linux; the stdlib can't read sysconf, and a wrong value only
// scales cpu_seconds_total, which the fleet view uses for relative
// comparison (and rate-from-deltas) rather than absolute accounting.
const clockTicksPerSec = 100.0

// healthReport is the heartbeat body. It carries the identity SPAM keys
// the session on (the kube-system cluster_id), the agent build identity,
// and a snapshot of self-metrics for the fleet view. It is a strict
// superset of the previous {"cluster_id": ...} body, so an older SPAM
// simply ignores the extra fields. Metric fields are best-effort:
// consumers must tolerate zeros (e.g. /proc is absent off Linux).
type healthReport struct {
	ClusterID string `json:"cluster_id"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	GoVersion string `json:"go_version,omitempty"`

	UptimeSeconds   int64   `json:"uptime_seconds"`
	Goroutines      int     `json:"goroutines"`
	HeapAllocBytes  uint64  `json:"heap_alloc_bytes"`
	SysBytes        uint64  `json:"sys_bytes"`
	RSSBytes        uint64  `json:"rss_bytes,omitempty"`
	CPUSecondsTotal float64 `json:"cpu_seconds_total,omitempty"`
	NumGC           uint32  `json:"num_gc"`
	GCPauseMsTotal  float64 `json:"gc_pause_ms_total"`
}

// buildHeartbeatBody collects current self-metrics and marshals the
// heartbeat/health body. Called once per heartbeat tick so the snapshot
// is fresh. Cumulative counters (cpu_seconds_total) are reported as-is
// so SPAM can derive rates from deltas and spot restarts (the counter
// drops to ~0 on a fresh process).
func buildHeartbeatBody(clusterID, version, commit string) ([]byte, error) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	r := healthReport{
		ClusterID:      clusterID,
		Version:        version,
		Commit:         commit,
		GoVersion:      runtime.Version(),
		UptimeSeconds:  int64(time.Since(processStart).Seconds()),
		Goroutines:     runtime.NumGoroutine(),
		HeapAllocBytes: ms.HeapAlloc,
		SysBytes:       ms.Sys,
		NumGC:          ms.NumGC,
		GCPauseMsTotal: float64(ms.PauseTotalNs) / 1e6,
	}
	r.RSSBytes, r.CPUSecondsTotal = procSelfStats()
	return json.Marshal(r)
}

// procSelfStats reads resident set size and cumulative CPU seconds from
// /proc/self/stat. procfs is mounted in the pod regardless of the
// scratch runtime image, so this works in-cluster; it returns zeros when
// /proc is unavailable (e.g. local dev on macOS) and the fields are then
// omitted from the body.
func procSelfStats() (rssBytes uint64, cpuSeconds float64) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, 0
	}
	// The comm field (2nd) is parenthesised and may itself contain
	// spaces or ')', so the stable tail is everything after the LAST
	// ')': it begins at field 3 (state), space-delimited.
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 >= len(s) {
		return 0, 0
	}
	fields := strings.Fields(s[rparen+2:])
	// Tail index = (stat field number - 3): utime=14, stime=15, rss=24.
	const utimeIdx, stimeIdx, rssIdx = 11, 12, 21
	if len(fields) <= rssIdx {
		return 0, 0
	}
	utime, _ := strconv.ParseFloat(fields[utimeIdx], 64)
	stime, _ := strconv.ParseFloat(fields[stimeIdx], 64)
	cpuSeconds = (utime + stime) / clockTicksPerSec
	if pages, err := strconv.ParseUint(fields[rssIdx], 10, 64); err == nil {
		rssBytes = pages * uint64(os.Getpagesize())
	}
	return rssBytes, cpuSeconds
}
