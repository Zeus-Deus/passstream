package sfu

import (
	"testing"
	"time"
)

func TestRefreshByteCapDoesNotChangeOrdinaryOrLowRenditionFlights(t *testing.T) {
	now := time.Now()
	for _, low := range []bool{false, true} {
		p := &Pacer{}
		p.boundFlight.Store(true)
		p.keyframeBytes.Store(9000)
		p.minimumFeedbackRTT.Store(int64(40 * time.Millisecond))
		p.rtt.Store(int64(40 * time.Millisecond))
		p.flightDemand.Store(2_000_000)
		p.acknowledgedBytes.Store(1)
		p.departedBytes.Store(70_001)
		p.activeKey.Store(true)
		p.startTrial(now.Add(time.Second).UnixNano())
		p.recordProbeRate(15_000_000, now)
		p.sustainedRate.Store(15_000_000)
		p.sustainedRateAt.Store(now.UnixNano())
		p.thumbnail.Store(low)
		if low {
			p.keyframeBytes.Store(250_000)
		}
		if p.flightLimited(15_000_000, false) {
			t.Fatal("native byte cap changed another rendition", low)
		}
	}
}
