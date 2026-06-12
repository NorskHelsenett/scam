package collector

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestWithClusterAttrsSwap proves a SetClusterAttrs after logger
// construction is reflected in subsequent records — the property the
// ROR identity refresh loop depends on.
func TestWithClusterAttrsSwap(t *testing.T) {
	t.Cleanup(func() { SetClusterAttrs(nil) })

	var buf bytes.Buffer
	log := slog.New(WithClusterAttrs(slog.NewJSONHandler(&buf, nil)))

	SetClusterAttrs([]slog.Attr{slog.String("cluster", "old-slug")})
	log.Info("one")
	SetClusterAttrs([]slog.Attr{
		slog.String("cluster", "new-name"),
		slog.Group("ror_metadata", "cluster_id", "uuid-1"),
	})
	log.Info("two")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"cluster":"old-slug"`) {
		t.Errorf("first record missing initial attrs: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"cluster":"new-name"`) ||
		!strings.Contains(lines[1], `"ror_metadata":{"cluster_id":"uuid-1"}`) {
		t.Errorf("second record missing swapped attrs: %s", lines[1])
	}
	if strings.Contains(lines[1], "old-slug") {
		t.Errorf("second record still carries stale attrs: %s", lines[1])
	}
}
