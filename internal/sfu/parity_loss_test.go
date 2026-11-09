package sfu

import (
	"testing"
	"time"
)

func TestExtraParityRetainsRecentLossAcrossCleanFeedback(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.effective.Store(700_000)
	p.flightDemand.Store(200_000)
	p.rtt.Store(int64(80 * time.Millisecond))
	p.keyframeBytes.Store(9_000)
	p.mediaLossAt.Store(now.UnixNano())
	p.mediaLossPPM.Store(50_000)
	if !p.extraParity(now) {
		t.Fatal("loss did not enable spare-capacity repair")
	}
	p.mediaLossPPM.Store(0)
	p.mediaLossAt.Store(now.Add(50 * time.Millisecond).UnixNano())
	if !p.extraParity(now.Add(120 * time.Millisecond)) {
		t.Fatal("clean feedback canceled scheduled repair")
	}
	p.retainParityLoss(now.Add(-time.Second).UnixNano())
	if !p.extraParity(now.Add(1999 * time.Millisecond)) {
		t.Fatal("loss episode ended before its bounded window")
	}
	if p.extraParity(now.Add(2 * time.Second)) {
		t.Fatal("clean feedback or old evidence extended the loss episode")
	}
}

func TestRetainedParityLossStillRequiresSafePath(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.effective.Store(700_000)
	p.flightDemand.Store(200_000)
	p.rtt.Store(int64(80 * time.Millisecond))
	p.keyframeBytes.Store(9_000)
	p.retainParityLoss(now.UnixNano())
	for _, rtt := range []time.Duration{0, 49 * time.Millisecond, 150 * time.Millisecond} {
		p.rtt.Store(int64(rtt))
		if p.extraParity(now) {
			t.Fatal("retained loss bypassed RTT guard", rtt)
		}
	}
	p.rtt.Store(int64(80 * time.Millisecond))
	p.effective.Store(659_999)
	if p.extraParity(now) {
		t.Fatal("retained loss bypassed capacity guard")
	}
	p.effective.Store(700_000)
	p.queue = []pacedPacket{{at: now.Add(-20 * time.Millisecond)}}
	if p.extraParity(now) {
		t.Fatal("retained loss bypassed backlog guard")
	}
	p.queue = nil
	if !p.extraParity(now) {
		t.Fatal("safe path lost retained repair")
	}
}
