package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	pushInterval   = 30 * time.Second
	maxBackoff     = 5 * time.Minute
	maxBufferSize  = 10_000 // drop oldest records beyond this
	pushBatchLimit = 2_000  // max records per POST
)

// lineCapture is an io.Writer that always writes to stdout and additionally
// buffers each JSON line for periodic push to SPAM.
type lineCapture struct {
	stdout io.Writer
	mu     sync.Mutex
	buf    []json.RawMessage
}

func (lc *lineCapture) Write(p []byte) (int, error) {
	// Always write to stdout first.
	n, err := lc.stdout.Write(p)

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

func (lc *lineCapture) flush() []json.RawMessage {
	lc.mu.Lock()
	lines := lc.buf
	lc.buf = nil
	lc.mu.Unlock()
	return lines
}

// pushLoop periodically flushes captured records to the SPAM callcenter endpoint.
// Uses exponential backoff on failure so a downed SPAM doesn't get hammered.
func pushLoop(ctx context.Context, endpoint string, cap *lineCapture) {
	client := &http.Client{Timeout: 30 * time.Second}
	ticker := time.NewTicker(pushInterval)
	defer ticker.Stop()

	backoff := time.Duration(0)

	for {
		select {
		case <-ctx.Done():
			push(client, endpoint, cap.flush())
			return
		case <-ticker.C:
			if backoff > 0 {
				backoff -= pushInterval
				if backoff > 0 {
					continue // still waiting out backoff
				}
				backoff = 0
			}
			records := cap.flush()
			if len(records) == 0 {
				continue
			}
			if !pushAll(client, endpoint, records) {
				// Re-buffer unsent records (they'll be retried next tick).
				cap.mu.Lock()
				cap.buf = append(records, cap.buf...)
				if len(cap.buf) > maxBufferSize {
					cap.buf = cap.buf[len(cap.buf)-maxBufferSize:]
				}
				cap.mu.Unlock()
				// Exponential backoff: 30s → 60s → 120s → ... → 5min cap.
				backoff = nextBackoff(backoff)
				log.Warn("push failed, backing off", "retry_in", backoff, "endpoint", endpoint)
			}
		}
	}
}

// pushAll sends records in batches, returns true if all succeeded.
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
		log.Error("push: marshal", "err", err)
		return false
	}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Error("push: post", "err", err)
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Error("push: unexpected status", "status", resp.StatusCode)
		return false
	}
	return true
}

func nextBackoff(current time.Duration) time.Duration {
	if current == 0 {
		return pushInterval
	}
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}
