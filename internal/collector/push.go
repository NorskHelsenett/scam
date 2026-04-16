package collector

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
	PushInterval   = 30 * time.Second
	maxBackoff     = 5 * time.Minute
	maxBufferSize  = 10_000
	pushBatchLimit = 2_000
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
