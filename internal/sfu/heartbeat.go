package sfu

import (
	"encoding/json"
	"time"
)

// Unreliable SCTP messages must fit one datagram: a lost diagnostic fragment
// must not hold decoder progress hostage. Full history is an explicit request.
func heartbeatPayload(message map[string]any, full bool) []byte {
	extra, _ := message["repair"].(map[string]any)
	if !full {
		if frames, ok := extra["fullLayerTimestamps"].([]uint32); ok && len(frames) > 4 {
			extra["fullLayerTimestamps"] = frames[len(frames)-4:]
		}
		if estimate, ok := extra["estimate"].(map[string]interface{}); ok {
			extra["estimate"] = map[string]any{"averageLoss": estimate["averageLoss"], "usage": estimate["usage"]}
		}
	}
	b, err := json.Marshal(message)
	if !full && len(b) > 900 {
		delete(extra, "feedbackRepairs")
		b, err = json.Marshal(message)
	}
	if !full && (err != nil || len(b) > 900) {
		for _, key := range []string{"sourceFrameTimes", "mediaRates", "layerDowns", "replay", "receivedWireRate", "provenWireRate", "paddingBytes", "baseWireDemand", "congestion", "targetBitrate"} {
			delete(extra, key)
		}
		b, err = json.Marshal(message)
	}
	if !full && (err != nil || len(b) > 900) {
		core := map[string]any{}
		for _, key := range []string{"wireBytes", "paddingBytes", "initialRTTMs", "decoderReported", "freshFrames", "freshAgeMs", "catchupMs", "keyframeBytes", "sourceKeyframeBytes", "fullLayerTimestamps", "pacerDrops", "wireBudget", "publisherBudget", "publisherQueueMs", "publisherReceivedWireRate"} {
			if value, ok := extra[key]; ok {
				core[key] = value
			}
		}
		message["repair"] = core
		b, _ = json.Marshal(message)
	}
	return b
}

// Five responses/s accommodates the 500ms sender and viewer cadence plus
// diagnostics while bounding expensive statistics and JSON work per peer.
func (p *Peer) acceptHeartbeat(now time.Time) bool {
	const period = int64(200 * time.Millisecond)
	for previous := p.heartbeatAcceptedAt.Load(); ; previous = p.heartbeatAcceptedAt.Load() {
		// Two-message burst covers a normal heartbeat plus requested diagnostics.
		if previous > 0 && now.UnixNano() < previous-period {
			return false
		}
		next := max(previous, now.UnixNano()) + period
		if p.heartbeatAcceptedAt.CompareAndSwap(previous, next) {
			return true
		}
	}
}

// One non-join native key per ten seconds per viewer: a one-second key can
// consume at most ten percent of its shared publisher's time. All non-join
// callers enter here, including upgrades and untrusted decoded-stall claims.
func (p *Peer) takeNativeKey(now time.Time) bool {
	at := &p.nativeKeyAt
	if p.nativeKeyOwner != nil {
		at = p.nativeKeyOwner
	}
	for previous := at.Load(); ; previous = at.Load() {
		if previous > 0 && now.UnixNano()-previous < int64(10*time.Second) {
			return false
		}
		if at.CompareAndSwap(previous, now.UnixNano()) {
			return true
		}
	}
}
