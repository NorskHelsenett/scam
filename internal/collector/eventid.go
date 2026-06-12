package collector

import "sync/atomic"

// nextEventID is the per-process monotonic counter stamped on every
// emitted record. It resets to 0 every SCAM startup; SPAM tracks the
// highest stored value per cluster and returns it in push responses
// so the push loop can detect drift (mismatch -> reconcile snapshot).
var nextEventID atomic.Uint64

// NextEventID returns the next monotonic event_id for an emitted
// record. Callers attach it as a "event_id" attribute on the slog
// record so SPAM can advance its last_seen pointer in lockstep.
func NextEventID() uint64 {
	return nextEventID.Add(1)
}
