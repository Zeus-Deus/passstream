package sfu

import (
	"encoding/json"
	"testing"
)

func TestHeartbeatHistoryDoesNotFragmentDecoderFeedback(t *testing.T) {
	frames := make([]int64, 256)
	for i := range frames {
		frames[i] = 1_793_800_000_000 + int64(i*66)
	}
	message := map[string]any{"type": "heartbeat", "ingressFrames": uint64(1000000), "repair": map[string]any{"sourceFrameTimes": frames, "decoderReported": true, "freshFrames": 1000, "wireBudget": 4_000_000, "initialRTTMs": 80}}
	full := heartbeatPayload(message, true)
	if len(full) <= 900 {
		t.Fatal("test history was not larger than one datagram")
	}
	compact := heartbeatPayload(message, false)
	if len(compact) > 900 {
		t.Fatal("decoder heartbeat can fragment", len(compact))
	}
	var decoded struct {
		Repair struct {
			DecoderReported bool
			FreshFrames     int
		}
	}
	if json.Unmarshal(compact, &decoded) != nil || !decoded.Repair.DecoderReported || decoded.Repair.FreshFrames != 1000 {
		t.Fatal("compacting diagnostics dropped decoder progress")
	}
}
