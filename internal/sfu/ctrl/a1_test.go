package ctrl

import (
	"testing"
	"time"
)

// Regressions for the rules A1 added or made concrete (docs/REPORT.md A1
// ledger). Each names the Tier A row that exposed it.

// One oversized key must not pin admission; the p75 of recent keys governs
// once there are KeySizeSamples of them, and old keys age out.
func TestKeySizeEstimate(t *testing.T) {
	k := &RoomKeys{}
	k.OnKey(epoch, Native, 588_020, epoch)
	if b, n := k.Size(epoch, Native); b != 588_020 || n != 1 {
		t.Fatal(b, n)
	}
	for i := range 7 {
		k.OnKey(epoch.Add(time.Duration(i+1)*time.Second), Native, 263_600, epoch)
	}
	if b, n := k.Size(epoch.Add(8*time.Second), Native); b != 263_600 || n != KeySizeSamples {
		t.Fatal(b, n)
	}
	if _, n := k.Size(epoch.Add(KeySizeAge+8*time.Second), Native); n != 0 {
		t.Fatal("keys did not age out", n)
	}
}

// The orchestrator's counterexample (C0 r3): 1.5 Mbit/s path, native T0
// holds at ≈1.24 Mbit/s, a typical key admits at ≈1.42 Mbit/s, but the only
// known key is oversized. Discovery first seeks the full admit; once a probe
// measured the path below it, native T0 is admitted on its steady need, and
// the actual assembled key is rechecked by KeyFits.
func TestOversizeKeyCounterexampleRecovers(t *testing.T) {
	c := Costs{Rate: [2][3]int{{1_190_869, 1_563_283, 2_257_015}, {52_500, 73_500, 105_000}}, KeyBytes: [2]int{588_020, 10_000}, KeySamples: [2]int{1, 1}, AudioWire: 48_000}
	low := Option{Low, 2}
	decide := func(ceiling int) Option {
		a := &LayerAllocator{}
		a.OnApplied(epoch, low)
		in := AllocInput{Estimate: Estimate{Rate: 1_500_000, Proven: 1_500_000, Ceiling: ceiling}, Costs: c, Applied: low, NativeKey: true}
		return a.Decide(epoch, in).Desired
	}
	if o := decide(0); o.Rendition != Low {
		t.Fatal("admitted before discovery measured the path", o)
	}
	if o := decide(1_500_000); o != (Option{Native, 0}) {
		t.Fatal("still blocked by one oversized key", o)
	}
	c.KeySamples[Native] = KeySizeSamples // a robust estimate this large does block
	a := &LayerAllocator{}
	a.OnApplied(epoch, low)
	if o := a.Decide(epoch, AllocInput{Estimate: Estimate{Rate: 1_500_000, Proven: 1_500_000, Ceiling: 1_500_000}, Costs: c, Applied: low, NativeKey: true}).Desired; o.Rendition != Low {
		t.Fatal(o)
	}
}

// A refused assembled key backs the viewer's upgrade off 3 s → 30 s.
func TestKeyRefusalBackoffBounded(t *testing.T) {
	v := NewViewer(1, NewRoomKeys())
	v.Est = &FixedEstimator{Rate: 1_500_000}
	now := epoch
	want := []time.Duration{3, 6, 12, 24, 30, 30}
	for _, w := range want {
		if v.KeyFits(now, Native, 588_020, Costs{AudioWire: 48_000}) {
			t.Fatal("oversized key fitted")
		}
		if got := v.refusedUntil.Sub(now); got != w*time.Second {
			t.Fatalf("back-off %v, want %v", got, w*time.Second)
		}
		now = v.refusedUntil
	}
	if !v.KeyFits(now, Native, 251_000, Costs{AudioWire: 48_000}) || v.refusedFor != 0 {
		t.Fatal("a fitting key did not reset the back-off")
	}
}

// FrameAge sheds disposable layers; a base picture waits for its deadline.
func TestFrameAgeShedsOnlyDisposable(t *testing.T) {
	p := &Queue{}
	var released []Packet
	p.SetRelease(func(k Packet) { released = append(released, k) })
	p.SetRate(epoch, 1) // nothing departs
	p.tokens = 0        // not even one packet larger than the burst cap
	deadline := epoch.Add(FrameDeadline)
	for i, tid := range []uint8{0, 2} {
		p.Push(epoch, Packet{ID: uint64(i), Size: 2 * BurstFloor, Kind: KindVideo, Class: ClassTL0 + Class(tid), Frame: uint64(i), TID: tid, Last: true, Packets: 1, FrameSize: 2 * BurstFloor, Deadline: deadline})
	}
	p.Pop(epoch.Add(2 * FrameAge))
	if len(released) != 1 || released[0].TID != 2 || len(p.Dropped()) != 0 {
		t.Fatal("age drop", released)
	}
	p.Pop(deadline)
	if d := p.Dropped(); len(released) != 2 || released[1].TID != 0 || len(d) != 1 || d[0].TID != 0 {
		t.Fatal("deadline drop", released, d)
	}
}

// Offers carry headroom above the target; once a saturated probe measured
// the path, offers stay within the overdrive cap of it and the cluster is
// the smallest measurable one (A6/A15/A18 return).
func TestProbeOfferHeadroomAndCeiling(t *testing.T) {
	p := &Discovery{}
	c, ok := p.Maybe(epoch, 1_000_000, 0, Estimate{}, false)
	if !ok || c.Rate != 1_100_000 {
		t.Fatal(c)
	}
	p = &Discovery{}
	c, ok = p.Maybe(epoch, 1_400_000, 100_000, Estimate{Rate: 256_000, Proven: 256_000, Ceiling: 256_000}, false)
	if !ok || c.Rate != 384_000 {
		t.Fatal(c)
	}
	if bytes := float64(c.Rate-100_000) * c.Until.Sub(epoch).Seconds() / 8; bytes > ProbeMinPackets*BurstFloor+1 {
		t.Fatal("ceiling-limited cluster padding", bytes)
	}
}

// A probe's own overshoot does not cut R, a collapse under a probe does.
func TestProbeOvershootExemptionBounded(t *testing.T) {
	b := &Bandwidth{Config: defaultConfig(), rate: 2_000_000, acked: 1_000_000, over: true, clusterAt: epoch, last: epoch}
	b.excess = ProbeLength / 2
	b.update(epoch.Add(50 * time.Millisecond))
	if b.rate != 2_000_000 {
		t.Fatal("probe overshoot cut R", b.rate)
	}
	b.excess = 2 * ProbeLength
	b.update(epoch.Add(60 * time.Millisecond))
	if b.rate >= 2_000_000 {
		t.Fatal("collapse under a probe was not cut")
	}
}

// A stalled viewer repeating its heartbeat is served once per RecoveryQuiet;
// ignored requests do not extend the debounce (A8 outage, A9 spam).
func TestRecoveryDebounceUsesAcceptedRequests(t *testing.T) {
	k := &RoomKeys{}
	if k.Request(epoch, 1, Native, CauseRecovery) != KeyRedirected {
		t.Fatal("first recovery")
	}
	if k.Request(epoch.Add(1500*time.Millisecond), 1, Native, CauseRecovery) != KeyIgnored {
		t.Fatal("debounce")
	}
	if k.Request(epoch.Add(RecoveryQuiet), 1, Native, CauseRecovery) != KeyRedirected {
		t.Fatal("ignored request extended the debounce")
	}
}

// C2-5, beside A9 spam: a recovery request ignored in any suppression phase
// (key admitted, key started, after completion within KeyEnvelope, or the
// RecoveryQuiet debounce) is still evidence that the viewer is asking, so it
// blocks a native upgrade for RecoveryQuiet, while the debounce keeps running
// from the last accepted recovery and broken events bypass key suppression.
func TestSuppressedRecoveryStillBlocksNativeUpgrade(t *testing.T) {
	for _, phase := range []string{"admitted", "started", "completed", "debounced"} {
		k := &RoomKeys{}
		if k.Request(epoch, 1, Native, CauseRecovery) != KeyRedirected {
			t.Fatal("first recovery")
		}
		at := epoch.Add(2100 * time.Millisecond) // past the debounce of the accepted request
		switch phase {
		case "admitted", "started":
			k.OnDelivery(1, KeyEvent{Frame: 1, Rendition: Low, Phase: KeyAdmitted, At: epoch.Add(time.Second)})
			if phase == "started" {
				k.OnDelivery(1, KeyEvent{Frame: 1, Rendition: Low, Phase: KeyStarted, At: epoch.Add(time.Second)})
			}
		case "completed":
			k.OnDelivery(1, KeyEvent{Frame: 1, Rendition: Low, Phase: KeyAdmitted, At: epoch.Add(400 * time.Millisecond)})
			k.OnDelivery(1, KeyEvent{Frame: 1, Rendition: Low, Phase: KeyCompleted, At: epoch.Add(500 * time.Millisecond)})
		case "debounced":
			at = epoch.Add(1900 * time.Millisecond)
		}
		if s := k.Request(at, 1, Native, CauseRecovery); s != KeyIgnored {
			t.Fatalf("%s: repeated recovery %v, want ignored", phase, s)
		}
		for _, later := range []time.Duration{0, RecoveryQuiet - time.Millisecond} {
			if k.Allowed(at.Add(later), 1, Native, CauseUpgrade) {
				t.Fatalf("%s: native upgrade allowed %v after a suppressed recovery request", phase, later)
			}
		}
		if !k.Allowed(at.Add(RecoveryQuiet), 1, Native, CauseUpgrade) {
			t.Fatalf("%s: upgrade still blocked RecoveryQuiet after the last request", phase)
		}
		if phase != "debounced" && k.Request(at, 1, Low, CauseBroken) != KeyRedirected {
			t.Fatalf("%s: a broken picture did not bypass key-delivery suppression", phase)
		}
	}
}

// Pictures of one rendition leave in order even when a newer one has a
// higher class (e2ee.mjs SFrame counters).
func TestPacerKeepsRenditionOrder(t *testing.T) {
	p := &Queue{}
	p.SetRate(epoch, 100_000_000)
	for i, tid := range []uint8{2, 0} {
		p.Push(epoch, Packet{ID: uint64(i), Size: 2 * BurstFloor, Kind: KindVideo, Class: ClassTL0 + Class(tid), Frame: uint64(i), TID: tid, Last: true, Packets: 1, FrameSize: 2 * BurstFloor, Deadline: epoch.Add(time.Second)})
	}
	var order []uint64
	for i := range 100 {
		if k, ok := p.Pop(epoch.Add(time.Duration(i) * time.Millisecond)); ok {
			order = append(order, k.Frame)
		}
	}
	if len(order) != 2 || order[0] != 0 {
		t.Fatal(order)
	}
}
