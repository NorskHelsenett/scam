package collector

import (
	"testing"
	"time"
)

func TestNextBackoff(t *testing.T) {
	tests := []struct {
		current time.Duration
		want    time.Duration
	}{
		{0, PushInterval},
		{PushInterval, 2 * PushInterval},
		{2 * PushInterval, 4 * PushInterval},
		{3 * time.Minute, 5 * time.Minute}, // capped at maxBackoff
		{5 * time.Minute, 5 * time.Minute}, // already at cap
	}
	for _, tt := range tests {
		got := NextBackoff(tt.current)
		if got != tt.want {
			t.Errorf("NextBackoff(%v) = %v, want %v", tt.current, got, tt.want)
		}
	}
}

func TestLineCaptureBasic(t *testing.T) {
	lc := &LineCapture{Stdout: &discardWriter{}}

	// Write a JSON line.
	_, _ = lc.Write([]byte(`{"msg":"hello"}` + "\n"))
	// Write a non-JSON line (should be ignored).
	_, _ = lc.Write([]byte("plain text\n"))
	// Write another JSON line.
	_, _ = lc.Write([]byte(`{"msg":"world"}` + "\n"))

	records := lc.Flush()
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if string(records[0]) != `{"msg":"hello"}` {
		t.Errorf("record[0] = %s, want {\"msg\":\"hello\"}", records[0])
	}

	// After flush, should be empty.
	if more := lc.Flush(); len(more) != 0 {
		t.Errorf("expected 0 after flush, got %d", len(more))
	}
}

func TestLineCaptureOverflow(t *testing.T) {
	lc := &LineCapture{Stdout: &discardWriter{}}

	// Fill past maxBufferSize.
	for i := 0; i < maxBufferSize+100; i++ {
		_, _ = lc.Write([]byte(`{"i":1}` + "\n"))
	}

	records := lc.Flush()
	if len(records) != maxBufferSize {
		t.Errorf("expected %d records after overflow, got %d", maxBufferSize, len(records))
	}
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
