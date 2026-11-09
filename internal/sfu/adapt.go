package sfu

import "time"

type rttSample struct {
	at  time.Time
	rtt time.Duration
}

const (
	rttRise    = 120 * time.Millisecond
	rttConfirm = 40 * time.Millisecond
	rttWindow  = 30 * time.Second
	minBackoff = 2 * time.Second
	// Revisit spare capacity within the 20s native-admission objective even
	// after repeated failures; byte and flight caps bound each experiment.
	maxBackoff   = 15 * time.Second
	stableWindow = 15 * time.Second
)

// ICE precedes media queues. Its handshake is a conservative fallback when
// the selected pair has not published a round-trip sample yet.
func initialRoundTrip(iceRTT, handshake time.Duration) time.Duration {
	if iceRTT > 0 {
		return iceRTT
	}
	return max(0, handshake)
}
