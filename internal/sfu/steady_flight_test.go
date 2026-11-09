package sfu

import (
	"testing"
	"time"
)

func TestRefreshAndOrdinaryMediaKeepTheirFeedbackSlack(t *testing.T) {
	for _, kind := range []string{"key", "debt", "ordinary", "low"} {
		t.Run(kind, func(t *testing.T) {
			p := &Pacer{}
			p.boundFlight.Store(true)
			p.keyframeBytes.Store(250_000)
			p.minimumFeedbackRTT.Store(int64(10 * time.Millisecond))
			p.rtt.Store(int64(10 * time.Millisecond))
			p.flightDemand.Store(2_000_000)
			p.sustainedRate.Store(2_000_000)
			p.sustainedRateAt.Store(time.Now().UnixNano())
			p.acknowledgedBytes.Store(1)
			p.departedBytes.Store(20_001)
			switch kind {
			case "key":
				p.activeKey.Store(true)
			case "debt":
				p.graceBytes.Store(1)
			case "ordinary":
				p.keyframeBytes.Store(20_000)
			case "low":
				p.thumbnail.Store(true)
			}
			if p.flightLimited(10_000_000, false) {
				t.Fatal("steady-screen tightening starved a refresh or another media mode")
			}
		})
	}
}
