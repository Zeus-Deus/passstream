package ctrl

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

var epoch = time.Unix(100, 0)

func probeTrace(t *testing.T, loss float64, lost map[int]bool) ProbeResult {
	t.Helper()
	p := &Discovery{}
	c, ok := p.Maybe(epoch, 3_000_000, 100_000, Estimate{Loss: loss}, false)
	if !ok {
		t.Fatal("cluster not started")
	}
	f := Feedback{}
	for i := 0; i < 30; i++ {
		at := epoch.Add(time.Duration(float64(i*BurstFloor*8) / float64(c.Rate) * float64(time.Second)))
		p.OnSent(Sent{Seq: uint64(i), At: at, Size: BurstFloor, Kind: KindPadding, Class: ClassPadding, Cluster: c.ID})
		a := Arrival{Seq: uint64(i), At: at.Add(20 * time.Millisecond)}
		if lost[i] {
			a.At = time.Time{}
		}
		f.Arrivals = append(f.Arrivals, a)
	}
	now := c.Until.Add(OveruseTime)
	r, ok := p.OnFeedback(now, f)
	if !ok {
		t.Fatal("no result")
	}
	return r
}
func TestLossCompensatedEqualSpacing(t *testing.T) {
	for _, n := range []int{0, 1, 6} {
		lost := map[int]bool{}
		for i := 0; i < n; i++ {
			lost[4+4*i] = true
		}
		r := probeTrace(t, float64(n)/29, lost)
		if !r.Success || !r.Clean || r.Rate < 3_000_000 || r.Class != ProbeDelivered {
			t.Fatalf("%d losses: %+v", n, r)
		}
	}
}
func TestBernoulliProbeUsesUniqueArrivals(t *testing.T) {
	for _, loss := range []float64{.05, .20} {
		for seed := uint64(1); seed <= 100; seed++ {
			rng := rand.New(rand.NewPCG(seed, 1))
			lost := map[int]bool{}
			for i := 1; i < 29; i++ {
				if rng.Float64() < loss {
					lost[i] = true
				}
			}
			r := probeTrace(t, loss, lost)
			offered := int(math.Ceil(3_000_000 * ProbeHeadroom / (1 - loss)))
			want := int(float64(offered) * float64(29-len(lost)) / 29)
			if r.Rate < want-2 || r.Rate > want+2 || r.Success != (r.Rate >= r.Target) || r.Class != ProbeDelivered {
				t.Fatalf("loss %v seed %d: %+v want %d", loss, seed, r, want)
			}
		}
	}
}
func TestProbePartialEvidenceAndFence(t *testing.T) {
	b := &Bandwidth{Config: defaultConfig()}
	b.OnProbe(epoch, ProbeResult{Rate: 1_500_000, Target: 3_000_000, Clean: true})
	if e := b.Estimate(epoch); e.Proven != 1_500_000 || e.Rate != 1_500_000 {
		t.Fatal(e)
	}
	b.OnGeneration(epoch, 1)
	b.OnProbe(epoch, ProbeResult{Rate: 50_000_000, Success: true, Clean: true})
	if e := b.Estimate(epoch); e.Proven != 0 {
		t.Fatal(e)
	}
}
func TestProbeCapAndDelayTail(t *testing.T) {
	p := &Discovery{}
	c, ok := p.Maybe(epoch, 1_000_000, 100_000, Estimate{Loss: .5}, false)
	if !ok || c.Rate != 1_500_000 || !p.capped {
		t.Fatal(c, p.capped)
	}
	p.Observe(epoch, Estimate{Overuse: true})
	r, ok := p.Tick(c.Until.Add(ProbeTimeout))
	if !ok || r.Clean || r.Success || !r.Capped {
		t.Fatal(r)
	}
}
func TestKeyBudgetAndDeliveryEvents(t *testing.T) {
	k := &RoomKeys{}
	if k.Request(epoch, 1, Low, CauseJoin) != KeyAccepted {
		t.Fatal("first request")
	}
	if len(k.Due(epoch)) != 1 {
		t.Fatal("no PLI")
	}
	k.OnKey(epoch.Add(time.Millisecond), Low, 100, epoch.Add(time.Millisecond))
	if k.Request(epoch.Add(time.Second), 2, Low, CauseJoin) != KeyPending || len(k.Due(epoch.Add(time.Second))) != 0 {
		t.Fatal("join bypassed budget")
	}
	k.OnDelivery(1, KeyEvent{Frame: 1, Phase: KeyAdmitted, At: epoch})
	if k.Request(epoch, 1, Native, CauseRecovery) != KeyIgnored {
		t.Fatal("own key stall")
	}
	k.OnDelivery(1, KeyEvent{Frame: 1, Phase: KeyAborted, At: epoch.Add(time.Second)})
	if k.Request(epoch.Add(time.Second), 1, Native, CauseBroken) != KeyRedirected {
		t.Fatal("broken key ignored")
	}
}
func TestFlexibleIntraAndUnsupported(t *testing.T) {
	d := &Dependency{}
	if !d.Allow(FrameInfo{Flexible: true, Key: true, Picture: 1}, 2) {
		t.Fatal("intra refused")
	}
	if d.Allow(FrameInfo{Flexible: true, Picture: 2, TID: 1}, 2) {
		t.Fatal("missing refs allowed")
	}
	if d.Allow(FrameInfo{Flexible: true, Key: true, Picture: 3}, 2) {
		t.Fatal("unsupported rendition restarted")
	}
}
func TestQueueEnvelopeAndBounds(t *testing.T) {
	p := &Queue{}
	released := 0
	p.SetRelease(func(Packet) { released++ })
	p.SetRate(epoch, 128_000)
	for i := 0; i < 80; i++ {
		p.Push(epoch, Packet{ID: uint64(i), Frame: uint64(i), Size: 1260, FrameSize: 1260, Packets: 1, Last: true, Kind: KindVideo, Class: ClassTL2, TID: 2})
	}
	if b, f := p.Queued(); b > QueueBytes || f > QueueFrames {
		t.Fatal(b, f)
	}
	total := 0
	for i := 0; i < 100; i++ {
		at := epoch.Add(time.Duration(i) * time.Millisecond)
		for {
			k, ok := p.Pop(at)
			if !ok {
				break
			}
			total += k.Size
		}
	}
	if total > 128_000/80+BurstFloor {
		t.Fatal("rate envelope", total)
	}
	if released == 0 {
		t.Fatal("bounds not exercised")
	}
}

func TestStartedDeltaSurvivesQueueAge(t *testing.T) {
	p := &Queue{}
	p.SetRate(epoch, 128_000)
	for i := 0; i < 4; i++ {
		p.Push(epoch, Packet{ID: uint64(i), Frame: 1, Size: 1260, FrameSize: 5040, Packets: 4, Last: i == 3, Kind: KindVideo, Class: ClassTL0, Deadline: epoch.Add(FrameDeadline)})
	}
	sent := 0
	for i := 0; i < 500; i++ {
		if _, ok := p.Pop(epoch.Add(time.Duration(i) * time.Millisecond)); ok {
			sent++
		}
	}
	if sent != 4 || len(p.Dropped()) != 0 {
		t.Fatal(sent, p.Dropped())
	}
}
func TestStartupKeyDoesNotSpendJoinBudget(t *testing.T) {
	k := &RoomKeys{}
	k.OnKey(epoch, Native, 251_000, epoch)
	if k.Request(epoch.Add(time.Second), 1, Native, CauseUpgrade) != KeyAccepted {
		t.Fatal("startup key consumed isolated join budget")
	}
}

func TestInitialLowBeforeNativeKey(t *testing.T) {
	a := &LayerAllocator{}
	in := AllocInput{Estimate: Estimate{Rate: 5_000_000, Proven: 5_000_000}, NativeKey: true, Applied: Option{Low, 2}}
	if o := a.Decide(epoch, in).Desired; o.Rendition != Low {
		t.Fatal(o)
	}
	a.OnApplied(epoch, Option{Low, 2})
	if o := a.Decide(epoch, in).Desired; o.Rendition != Native {
		t.Fatal(o)
	}
}
func TestSupersededLossCannotBreakNewKey(t *testing.T) {
	d := &Dependency{}
	d.Allow(FrameInfo{Key: true, Flexible: true, Picture: 100}, 2)
	d.Lost(FrameInfo{Picture: 99, TID: 0})
	if !d.Allow(FrameInfo{Picture: 101, Flexible: true, Refs: []uint64{100}}, 2) {
		t.Fatal("stale loss broke new key")
	}
}
func TestClockProbeContainsEnoughPackets(t *testing.T) {
	p := &Queue{}
	p.SetRate(epoch, 300_000)
	p.SetCluster(&Cluster{ID: 1, Start: epoch, Until: epoch.Add(ProbeLength), Rate: 200_000})
	n := 0
	for i := 0; i < 1000; i++ {
		if _, ok := p.Pop(epoch.Add(time.Duration(i) * time.Millisecond / 10)); ok {
			n++
		}
	}
	if n < ProbeMinPackets {
		t.Fatal(n)
	}
}
