package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	PushInterval             = 30 * time.Second
	defaultHeartbeatInterval = 5 * time.Minute
	maxHeartbeatBackoff      = 15 * time.Minute
	heartbeatTimeout         = 3 * time.Second
	heartbeatReminderAfter   = 1 * time.Hour
	maxBackoff               = 5 * time.Minute
	maxBufferSize            = 10_000
	pushBatchLimit           = 2_000
	// shutdownFlushTimeout bounds the final best-effort flush on
	// shutdown. Without it, an unreachable SPAM holds the process for
	// the full 30s client timeout per batch (up to 5 batches), eating
	// the pod's termination grace.
	shutdownFlushTimeout = 10 * time.Second
	// snapshotCooldown caps how often an ACK-mismatch triggers a
	// reconcile snapshot. Without it, a persistent SPAM-side gap
	// (e.g. broken persistence) would fire a snapshot every push tick.
	snapshotCooldown = 5 * time.Minute
)

// LineCapture is an io.Writer that always writes to stdout and additionally
// buffers each JSON line for periodic push to SPAM.
type LineCapture struct {
	Stdout io.Writer
	mu     sync.Mutex
	buf    []json.RawMessage
}

func (lc *LineCapture) Write(p []byte) (int, error) {
	n, err := lc.Stdout.Write(p)
	trimmed := bytes.TrimSpace(p)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		lc.mu.Lock()
		if len(lc.buf) >= maxBufferSize {
			drop := len(lc.buf) - maxBufferSize + 1
			lc.buf = lc.buf[drop:]
		}
		lc.buf = append(lc.buf, json.RawMessage(append([]byte(nil), trimmed...)))
		lc.mu.Unlock()
	}
	return n, err
}

func (lc *LineCapture) Flush() []json.RawMessage {
	lc.mu.Lock()
	lines := lc.buf
	lc.buf = nil
	lc.mu.Unlock()
	return lines
}

// Rebuffer puts unsent records back for retry on the next tick.
func (lc *LineCapture) Rebuffer(records []json.RawMessage) {
	lc.mu.Lock()
	lc.buf = append(records, lc.buf...)
	if len(lc.buf) > maxBufferSize {
		lc.buf = lc.buf[len(lc.buf)-maxBufferSize:]
	}
	lc.mu.Unlock()
}

// pushAck is the SPAM response body. last_seen_event_id is the highest
// event_id SPAM has persisted for this cluster; the push loop compares
// it against the local "highest pushed" and triggers a reconcile
// snapshot on mismatch. SPAM that hasn't implemented the contract
// returns nothing — last_seen_event_id is then 0 and we skip the
// comparison entirely until SPAM speaks the protocol.
type pushAck struct {
	LastSeenEventID uint64 `json:"last_seen_event_id"`
}

// PushLoop periodically flushes captured records to the SPAM callcenter
// endpoint, advances the local "last pushed" event_id after each
// success, and fires triggerSnapshot when SPAM's reported last_seen
// drifts from our last_pushed (with snapshotCooldown to avoid loops).
// triggerSnapshot may be nil; when it is, the ACK path is disabled.
func PushLoop(ctx context.Context, endpoint string, cap *LineCapture, triggerSnapshot func()) {
	client := &http.Client{Timeout: 30 * time.Second}
	ticker := time.NewTicker(PushInterval)
	defer ticker.Stop()

	var (
		backoff           time.Duration
		lastPushedEventID uint64
		lastSnapshotAt    time.Time
	)

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
			_, _, _ = pushAll(flushCtx, client, endpoint, cap.Flush())
			cancel()
			return
		case <-ticker.C:
			if backoff > 0 {
				backoff -= PushInterval
				if backoff > 0 {
					continue
				}
				backoff = 0
			}
			records := cap.Flush()
			if len(records) == 0 {
				continue
			}
			lastSeen, unsent, ok := pushAll(ctx, client, endpoint, records)
			if !ok {
				cap.Rebuffer(unsent)
				backoff = NextBackoff(backoff)
				// LogLocal: buffering this warning would queue the push
				// loop's own failure reports for redelivery to SPAM.
				LogLocal.Warn("push failed, backing off", "retry_in", backoff, "endpoint", endpoint)
				continue
			}
			if batchHigh := scanMaxEventID(records); batchHigh > lastPushedEventID {
				lastPushedEventID = batchHigh
			}
			if triggerSnapshot != nil && lastSeen != 0 &&
				lastSeen != lastPushedEventID &&
				time.Since(lastSnapshotAt) >= snapshotCooldown {
				Log.Info("snapshot: ACK mismatch, firing reconcile",
					"last_pushed", lastPushedEventID,
					"spam_last_seen", lastSeen)
				triggerSnapshot()
				lastSnapshotAt = time.Now()
			}
		}
	}
}

// scanMaxEventID returns the highest event_id field across records.
// Records without event_id (e.g. operational logs) contribute 0 and
// don't perturb the comparison; SPAM ignores them on its side too.
func scanMaxEventID(records []json.RawMessage) uint64 {
	type extract struct {
		EventID uint64 `json:"event_id"`
	}
	var maxID uint64
	for _, r := range records {
		var e extract
		if err := json.Unmarshal(r, &e); err != nil {
			continue
		}
		if e.EventID > maxID {
			maxID = e.EventID
		}
	}
	return maxID
}

// pushAll sends records in pushBatchLimit-sized batches. On failure it
// returns the undelivered tail (the failed batch and everything after)
// so the caller can rebuffer exactly that — batches already ACKed by
// SPAM must not be resent.
func pushAll(ctx context.Context, client *http.Client, endpoint string, records []json.RawMessage) (uint64, []json.RawMessage, bool) {
	var lastSeen uint64
	for len(records) > 0 {
		batch := records
		if len(batch) > pushBatchLimit {
			batch = records[:pushBatchLimit]
		}
		seen, ok := push(ctx, client, endpoint, batch)
		if !ok {
			return lastSeen, records, false
		}
		records = records[len(batch):]
		if seen > lastSeen {
			lastSeen = seen
		}
	}
	return lastSeen, nil, true
}

func push(ctx context.Context, client *http.Client, endpoint string, records []json.RawMessage) (uint64, bool) {
	if len(records) == 0 {
		return 0, true
	}
	body, err := json.Marshal(records)
	if err != nil {
		LogLocal.Error("push: marshal", "err", err)
		return 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		LogLocal.Error("push: build request", "err", err)
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		LogLocal.Error("push: post", "err", err)
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		LogLocal.Error("push: unexpected status", "status", resp.StatusCode)
		return 0, false
	}
	var ack pushAck
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	return ack.LastSeenEventID, true
}

// resolveHeartbeatInterval reads HEARTBEAT_INTERVAL (Go duration) and
// falls back to the default. Invalid values are logged and the default
// used so a typo doesn't silently disable heartbeats.
func resolveHeartbeatInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("HEARTBEAT_INTERVAL"))
	if raw == "" {
		return defaultHeartbeatInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		Log.Warn("heartbeat: invalid HEARTBEAT_INTERVAL, using default",
			"value", raw, "default", defaultHeartbeatInterval)
		return defaultHeartbeatInterval
	}
	return d
}

// HeartbeatLoop posts a liveness ping to SPAM periodically so the
// server's live-state filter keeps the cluster visible even when
// there's no resource churn to push.
//
// Design for large fleets:
//
//  1. Fire-and-forget with a short (heartbeatTimeout) request timeout —
//     a slow SPAM won't wedge the goroutine.
//  2. Exponential backoff on sustained failures (e.g. firewall closed)
//     caps at maxHeartbeatBackoff so we don't hammer the endpoint while
//     it's unreachable.
//  3. Logging is deliberately sparse: the first failure logs ERROR
//     (catches misconfiguration on first deploy), subsequent failures
//     in the same outage are silent, and every heartbeatReminderAfter
//     of continuous failure a fresh ERROR fires so alerting picks it
//     up. Recovery logs INFO exactly once.
//  4. Single Timer (not a Ticker) so variable intervals are clean;
//     body + http.Client reused across the loop.
//
// Failures never affect anything outside this goroutine — the cluster
// the agent is running in is unaffected by an unreachable SPAM.
func HeartbeatLoop(ctx context.Context, endpoint, clusterID string) {
	if clusterID == "" {
		Log.Warn("heartbeat: cluster_id empty, skipping loop")
		return
	}
	body, err := json.Marshal(map[string]string{"cluster_id": clusterID})
	if err != nil {
		Log.Error("heartbeat: marshal", "err", err)
		return
	}
	client := &http.Client{Timeout: heartbeatTimeout}
	interval := resolveHeartbeatInterval()

	var (
		consecutiveFailures int
		lastErrorLog        time.Time
		firstTick           = true
	)

	// First heartbeat fires immediately so liveness is established at
	// startup rather than after a full interval.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if err := sendHeartbeat(ctx, client, endpoint, body); err != nil {
			consecutiveFailures++
			// First failure always logs (catches firewall/DNS issues on
			// first deploy); subsequent failures log only every hour so
			// a long outage stays visible without flooding.
			if consecutiveFailures == 1 || time.Since(lastErrorLog) >= heartbeatReminderAfter {
				Log.Error("heartbeat: unable to contact callcenter",
					"endpoint", endpoint,
					"err", err,
					"consecutive_failures", consecutiveFailures)
				lastErrorLog = time.Now()
			}
			timer.Reset(nextHeartbeatBackoff(interval, consecutiveFailures))
			firstTick = false
			continue
		}

		if consecutiveFailures > 0 {
			Log.Info("heartbeat: restored",
				"endpoint", endpoint,
				"after_failures", consecutiveFailures)
			consecutiveFailures = 0
			lastErrorLog = time.Time{}
		} else if firstTick {
			// One INFO on first successful ping so operators can see
			// liveness got through on startup.
			Log.Info("heartbeat: ok", "endpoint", endpoint, "interval", interval)
			firstTick = false
		}
		timer.Reset(interval)
	}
}

// sendHeartbeat returns an error for transport failures or non-2xx
// responses. The request is cancelable via ctx so shutdown isn't
// blocked by the request timeout.
func sendHeartbeat(ctx context.Context, client *http.Client, endpoint string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// nextHeartbeatBackoff doubles the interval for each consecutive
// failure, capped at maxHeartbeatBackoff.
func nextHeartbeatBackoff(base time.Duration, consecutiveFailures int) time.Duration {
	d := base
	for i := 1; i < consecutiveFailures; i++ {
		d *= 2
		if d >= maxHeartbeatBackoff {
			return maxHeartbeatBackoff
		}
	}
	return d
}

func NextBackoff(current time.Duration) time.Duration {
	if current == 0 {
		return PushInterval
	}
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}
