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

// PushLoop periodically flushes captured records to the SPAM callcenter endpoint.
func PushLoop(ctx context.Context, endpoint string, cap *LineCapture) {
	client := &http.Client{Timeout: 30 * time.Second}
	ticker := time.NewTicker(PushInterval)
	defer ticker.Stop()

	backoff := time.Duration(0)

	for {
		select {
		case <-ctx.Done():
			push(client, endpoint, cap.Flush())
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
			if !pushAll(client, endpoint, records) {
				cap.Rebuffer(records)
				backoff = NextBackoff(backoff)
				Log.Warn("push failed, backing off", "retry_in", backoff, "endpoint", endpoint)
			}
		}
	}
}

func pushAll(client *http.Client, endpoint string, records []json.RawMessage) bool {
	for len(records) > 0 {
		batch := records
		if len(batch) > pushBatchLimit {
			batch = records[:pushBatchLimit]
		}
		records = records[len(batch):]
		if !push(client, endpoint, batch) {
			return false
		}
	}
	return true
}

func push(client *http.Client, endpoint string, records []json.RawMessage) bool {
	if len(records) == 0 {
		return true
	}
	body, err := json.Marshal(records)
	if err != nil {
		Log.Error("push: marshal", "err", err)
		return false
	}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		Log.Error("push: post", "err", err)
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		Log.Error("push: unexpected status", "status", resp.StatusCode)
		return false
	}
	return true
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
