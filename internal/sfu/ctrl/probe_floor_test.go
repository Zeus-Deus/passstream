package ctrl

import (
	"testing"
	"time"
)

// A16 seed 85: a 64 kbit/s collapse left a 49 kbit/s ceiling. After
// capacity returned, ordinary media already proved 120 kbit/s, but probes
// still offered only 74 kbit/s, wholly inside one MTU burst.
func TestDiscoveryCannotOfferBelowDeliveryThatAlreadyContradictsItsCeiling(t *testing.T) {
	p := &Discovery{}
	e := Estimate{Rate: 128_000, Proven: 120_000, Ceiling: 49_502}
	c, ok := p.Maybe(epoch, 1_500_000, 100_000, e, false)
	if !ok || c.Rate < int(defaultConfig().ProbeMaxOverdrive*float64(e.Proven)) {
		t.Fatalf("fresh delivery was ignored: %+v, started %v", c, ok)
	}
}

func TestCompressedDeliveryAboveRDoesNotEscalateACeilingRatchet(t *testing.T) {
	p := &Discovery{}
	e := Estimate{Rate: 128_000, Proven: 1_500_000, Ceiling: 200_000}
	c, ok := p.Maybe(epoch, 2_000_000, 100_000, e, false)
	if !ok || c.Rate > int(defaultConfig().ProbeMaxOverdrive*float64(e.Ceiling)) {
		t.Fatalf("one compressed burst escaped the current paced rate/capacity bound: %+v %v", c, ok)
	}
}

// A16 seed 84: a healthy viewer could not renew its fast-key evidence,
// because the fixed 6.3 KB high-rate ratchet fitted inside one burst.
// Fund the measurable sample only from real available padding credit.
func TestHighRateRatchetEscapesItsBurstWhenTheBudgetAffordsIt(t *testing.T) {
	p := &Discovery{}
	c, ok := p.Maybe(epoch, 10_000_000, 0, Estimate{Rate: 2_800_000, Proven: 2_800_000, Ceiling: 6_000_000}, false)
	if !ok {
		t.Fatal("no ratchet")
	}
	sends := pacerSends(c, epoch, BurstFloor, int(2_800_000*PacerBurst.Seconds()/8))
	r, ok := deliver(p, sends, c.Until.Add(OveruseTime), func(i int) time.Time {
		return sends[i].At.Add(20 * time.Millisecond)
	})
	if !ok || r.Class == ProbeUndersized || r.Rate < c.Rate-2 || r.Rate > c.Rate {
		t.Fatalf("a funded high-rate ratchet remained unmeasurable: %+v, result %v", r, ok)
	}
	if p.BootstrapUsed() > BootstrapPadding {
		t.Fatal("invented padding credit")
	}
}
