package ctrl

import (
	"testing"
	"time"
)

// A1 seed 16 exhausted bootstrap on the rendition rungs. Its 9 ms
// fast-key cluster fitted entirely in one 10 ms burst, measured nothing,
// and delayed native playback. An already admitted key is useful media
// that can fund a measurable experiment without any padding.
func TestAdmittedKeyFundsAMeasurableClusterWithoutPadding(t *testing.T) {
	p := &Discovery{bootstrap: BootstrapPadding}
	e := Estimate{Rate: 3_000_000, Proven: 3_000_000}
	c, ok := p.MaybeKey(epoch, 8_000_000, 250_000, e)
	if !ok || !c.MediaOnly || c.Rate > int(defaultConfig().ProbeMaxOverdrive*8_000_000) ||
		c.Until.Sub(epoch) > PacerBurst+time.Duration(ProbeMinPackets*BurstFloor*8)*time.Second/time.Duration(c.Rate)+time.Microsecond {
		t.Fatalf("key-funded cluster: %+v, started %v", c, ok)
	}
	sends := pacerSends(c, epoch, BurstFloor, 0)
	for i := range sends {
		sends[i].Kind, sends[i].Class = KindVideo, ClassKey
	}
	r, ok := deliver(p, sends, c.Until.Add(OveruseTime), func(i int) time.Time {
		return sends[i].At.Add(20 * time.Millisecond)
	})
	if !ok || r.Class == ProbeUndersized || r.Rate > c.Rate || r.Rate < c.Rate-2 || p.BootstrapUsed() != BootstrapPadding {
		t.Fatalf("key experiment: %+v, measured %v, bootstrap %d", r, ok, p.BootstrapUsed())
	}
	q := &Queue{}
	q.SetRate(epoch, e.Rate)
	q.SetCluster(&c)
	if pkt, ok := q.Pop(epoch.Add(time.Millisecond)); ok || !q.NextAt(epoch.Add(time.Millisecond)).IsZero() {
		t.Fatalf("a media-only experiment invented padding after its key ended: %+v %v", pkt, ok)
	}
}

func TestKeyExperimentRespectsDebtCapacityAndMinimumUsefulBytes(t *testing.T) {
	for _, e := range []Estimate{
		{Rate: 3_000_000, Proven: 3_000_000, Draining: true},
		{Rate: 3_000_000, Proven: 3_000_000, Overuse: true},
		{Rate: 3_000_000, Proven: 3_000_000, Ceiling: 3_000_000},
		{Rate: 3_000_000, Proven: 3_000_000, LostAt: epoch},
	} {
		if c, ok := (&Discovery{}).MaybeKey(epoch, 8_000_000, 250_000, e); ok {
			t.Fatalf("key overrides debt/congestion/capacity: %+v -> %+v", e, c)
		}
	}
	if c, ok := (&Discovery{}).MaybeKey(epoch, 8_000_000, BurstFloor, Estimate{Rate: 3_000_000, Proven: 3_000_000}); ok {
		t.Fatalf("one packet is not a sustainable key experiment: %+v", c)
	}
}

func TestQueuedKeyReportsActualSizeAndRemainingBytes(t *testing.T) {
	q := &Queue{}
	q.SetRate(epoch, 1_000_000)
	for i := range 10 {
		q.Push(epoch, Packet{ID: uint64(i), Size: 1000, FrameSize: 10_000, Packets: 10, Frame: 1,
			Key: true, Kind: KindVideo, Class: ClassKey, Rendition: Native, Last: i == 9})
	}
	if left, total := q.QueuedKey(Native); left != 10_000 || total != 10_000 {
		t.Fatalf("new key: %d/%d", left, total)
	}
	if _, ok := q.Pop(epoch); !ok {
		t.Fatal("key did not start")
	}
	if left, total := q.QueuedKey(Native); left != 9000 || total != 10_000 {
		t.Fatalf("partially sent key: %d/%d", left, total)
	}
}

func TestReplacementAcquiresLowBeforeRequestingNative(t *testing.T) {
	v := NewViewerFrom(1001, NewRoomKeys(), RateState{Rate: 5_000_000, Session: 1})
	v.Est, v.Probe = &FixedEstimator{Rate: 5_000_000}, NoProber{}
	c := Costs{Rate: [2][3]int{{1_000_000, 1_400_000, 2_000_000}, {50_000, 70_000, 100_000}},
		KeyBytes: [2]int{250_000, 9000}}
	if d := v.Step(epoch, c); d.Desired.Rendition != Low || !d.Requested {
		t.Fatalf("replacement did not acquire low: %+v", d)
	}
	v.OnApplied(epoch.Add(10*time.Millisecond), Option{Low, 2})
	if d := v.Step(epoch.Add(300*time.Millisecond), c); d.Desired.Rendition != Low {
		t.Fatalf("a native key interrupted recovery's first fresh frames: %+v", d)
	}
	if d := v.Step(epoch.Add(RecoveryQuiet), c); d.Desired.Rendition != Native {
		t.Fatalf("replacement failed to resume native discovery after recovery: %+v", d)
	}
}

func TestAnAdmittedKeySupersedesACheaperPendingExperiment(t *testing.T) {
	p := &Discovery{}
	e := Estimate{Rate: 2_400_000, Proven: 2_200_000}
	old, ok := p.Maybe(epoch, 2_600_000, 2_000_000, e, false)
	if !ok {
		t.Fatal("no ordinary experiment")
	}
	p.OnSent(Sent{Seq: 1, At: epoch, Size: BurstFloor, Kind: KindPadding, Cluster: old.ID})
	next, ok := p.MaybeKey(epoch.Add(time.Millisecond), 10_000_000, 400_000, e)
	if !ok || !next.MediaOnly || next.ID == old.ID {
		t.Fatalf("admitted key waited behind an obsolete lower experiment: %+v %v", next, ok)
	}
	p.OnFeedback(epoch.Add(2*time.Millisecond), Feedback{Arrivals: []Arrival{{Seq: 1, At: epoch.Add(20 * time.Millisecond)}}})
	if len(p.arrived) != 0 {
		t.Fatal("cancelled-cluster feedback contaminated the key experiment")
	}
}
