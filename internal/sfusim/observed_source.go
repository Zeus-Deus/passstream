package sfusim

import (
	"fmt"
	"math"
	"time"
)

// ObservedSourceEvent is one measured boundary, not a generated packet train.
// Missing identities/timestamps/byte counts stay unknown. In particular a
// canvas draw is capture input, not an identified encoded frame's capture time.
// Encoded/Protected are transform byte counts; AssembledWire includes RTP and
// transport overhead. They must never substitute for each other.
type ObservedSourceEvent struct {
	AtUS           int64  `json:"at_us"`
	Boundary       string `json:"boundary"` // capture_input, encoded, arrival
	Clock          string `json:"clock"`
	Identity       string `json:"identity,omitempty"`
	Track          int    `json:"track"`
	Key            bool   `json:"key,omitempty"`
	EncodedBytes   int    `json:"encoded_bytes,omitempty"`
	ProtectedBytes int    `json:"protected_bytes,omitempty"`
	AssembledWire  int    `json:"assembled_wire_bytes,omitempty"`
}

// ObservedSource is a diagnostic trace on one explicitly declared relative
// time axis. Events need not have a cross-boundary frame identity: replay does
// not invent one or infer capture from network/decoder delay. Clock provenance
// remains attached when browser monotonic epoch and server wall time coexist.
// This deliberately does not drive packet-level Tier A from frame-only evidence.
type ObservedSource struct {
	Version int                   `json:"version"`
	Events  []ObservedSourceEvent `json:"events"`
}

// ReplayObservedSource schedules exact observed events on the simulator clock.
// It does not use the stochastic source, viewer feedback, a repayment formula,
// or synthetic packetization. The canonical generator/gates are untouched.
func ReplayObservedSource(s *Sim, trace ObservedSource, emit func(ObservedSourceEvent)) error {
	if trace.Version != 1 || emit == nil {
		return fmt.Errorf("observed source requires version 1 and an event sink")
	}
	// Validate atomically before scheduling anything. Sim.At otherwise clamps
	// past events to now, silently destroying the timing evidence.
	for i, e := range trace.Events {
		if e.AtUS < 0 || e.AtUS > math.MaxInt64/int64(time.Microsecond) || time.Duration(e.AtUS)*time.Microsecond < s.Now() || e.Clock == "" || e.Track < 0 {
			return fmt.Errorf("observed source event %d: invalid time, clock or track", i)
		}
		if e.EncodedBytes < 0 || e.ProtectedBytes < 0 || e.AssembledWire < 0 {
			return fmt.Errorf("observed source event %d: negative byte count", i)
		}
		switch e.Boundary {
		case "capture_input":
			if e.EncodedBytes != 0 || e.ProtectedBytes != 0 || e.AssembledWire != 0 || e.Key {
				return fmt.Errorf("observed source event %d: capture input has unobserved encoder data", i)
			}
		case "encoded":
			if e.EncodedBytes == 0 || e.AssembledWire != 0 {
				return fmt.Errorf("observed source event %d: encoded/wire units conflated", i)
			}
		case "arrival":
			if e.EncodedBytes != 0 || e.ProtectedBytes != 0 {
				return fmt.Errorf("observed source event %d: arrival/encoder units conflated", i)
			}
		default:
			return fmt.Errorf("observed source event %d: unknown boundary %q", i, e.Boundary)
		}
	}
	for _, e := range trace.Events {
		s.At(time.Duration(e.AtUS)*time.Microsecond, func() { emit(e) })
	}
	return nil
}
