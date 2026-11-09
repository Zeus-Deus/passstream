package ctrl

import (
	"testing"
	"time"
)

// C2-2 probe evidence regressions: D is bounded by the sustained offer on
// both sides of the same acked packets, and only a saturated complete
// cluster installs a capacity ceiling.

// pacerSends is what a token-bucket pacer at c.Rate emits for the cluster
// starting at start with tokens bytes of credit: size-byte packets, the
// credited ones back-to-back, then one each size×8/rate, while before Until.
func pacerSends(c Cluster, start time.Time, size, tokens int) []Sent {
	var out []Sent
	at, credit := start, float64(tokens)
	for at.Before(c.Until) {
		if credit < float64(size) {
			at = at.Add(time.Duration((float64(size) - credit) * 8 / float64(c.Rate) * float64(time.Second)))
			credit = float64(size)
			continue
		}
		credit -= float64(size)
		out = append(out, Sent{Seq: uint64(len(out)), At: at, Size: size, Kind: KindPadding, Class: ClassPadding, Cluster: c.ID})
	}
	return out
}

// deliver reports every packet in one feedback at now, arriving at arrive(i)
// (zero: lost).
func deliver(p *Discovery, sends []Sent, now time.Time, arrive func(i int) time.Time) (ProbeResult, bool) {
	for _, s := range sends {
		p.OnSent(s)
	}
	f := Feedback{}
	for i, s := range sends {
		f.Arrivals = append(f.Arrivals, Arrival{Seq: s.Seq, At: arrive(i)})
	}
	return p.OnFeedback(now, f)
}

// A full pacer burst at the start and a delayed, compressed delivery tail:
// before C2-2 the tail's dispersion was credited as the delivered rate.
func TestProbeCompressionCannotExceedSustainedOffer(t *testing.T) {
	p := &Discovery{}
	c, ok := p.Maybe(epoch, 5_000_000, 0, Estimate{}, false)
	if !ok {
		t.Fatal("no probe")
	}
	burst := int(float64(c.Rate) * PacerBurst.Seconds() / 8)
	sends := pacerSends(c, epoch, BurstFloor, burst)
	half := len(sends) / 2
	r, ok := deliver(p, sends, c.Until.Add(time.Second/2), func(i int) time.Time {
		if i < half {
			return sends[i].At.Add(20 * time.Millisecond)
		}
		// The second half is compressed onto the first half's last arrival.
		return sends[half-1].At.Add(20*time.Millisecond + time.Duration(i)*10*time.Microsecond)
	})
	if !ok {
		t.Fatal("no result")
	}
	t.Logf("offer %d: %+v", c.Rate, r)
	if r.Rate > c.Rate || r.Rate < c.Rate*9/10 || r.Class != ProbeDelivered || !r.Success {
		t.Fatal("a compressed tail is credited beyond the sustained offer, or the offer is not credited", r)
	}
}

// Feedback for the cluster arrives in several late reports; the arrival
// times reflect the path, so the reporting delay changes nothing, and the
// result waits for the last report rather than finishing on a partial one.
func TestProbeStretchedFeedbackMeasuresThePath(t *testing.T) {
	p := &Discovery{}
	c, _ := p.Maybe(epoch, 2_000_000, 0, Estimate{}, false)
	sends := pacerSends(c, epoch, BurstFloor, 0)
	for _, s := range sends {
		p.OnSent(s)
	}
	third := len(sends) / 3
	reports := []struct {
		at       time.Time
		from, to int
	}{{c.Until.Add(50 * time.Millisecond), 0, third}, {c.Until.Add(400 * time.Millisecond), third, 2 * third}, {c.Until.Add(900 * time.Millisecond), 2 * third, len(sends)}}
	var r ProbeResult
	for i, rep := range reports {
		f := Feedback{}
		for _, s := range sends[rep.from:rep.to] {
			f.Arrivals = append(f.Arrivals, Arrival{Seq: s.Seq, At: s.At.Add(30 * time.Millisecond)})
		}
		var ok bool
		r, ok = p.OnFeedback(rep.at, f)
		if ok != (i == len(reports)-1) {
			t.Fatalf("report %d finished=%v", i, ok)
		}
		if _, ok := p.Tick(rep.at.Add(time.Millisecond)); ok && i < len(reports)-1 {
			t.Fatal("Tick finished on partial feedback before ProbeTimeout")
		}
	}
	if r.Class != ProbeDelivered || !r.Success || r.Rate < c.Rate-2 || r.Rate > c.Rate {
		t.Fatal("reporting delay changed the measurement", c.Rate, r)
	}
}

// Sparse media: the cluster is mostly media pictures, each a back-to-back
// burst, with few padding packets because the padding budget ran out. The
// sender did not sustain the offer, so the cluster proves nothing above what
// it actually sent, and is no bottleneck bound however slowly it arrived.
func TestProbeSparseMediaIsSendLimited(t *testing.T) {
	p := &Discovery{}
	c, _ := p.Maybe(epoch, 1_000_000, 300_000, Estimate{}, false)
	var sends []Sent
	for frame := 0; frame < 4; frame++ {
		at := epoch.Add(time.Duration(frame) * 33 * time.Millisecond)
		for k := 0; k < 2; k++ {
			sends = append(sends, Sent{Seq: uint64(len(sends)), At: at, Size: 1200, Kind: KindVideo, Class: ClassTL0, Cluster: c.ID})
		}
	}
	for k := 0; k < 2; k++ {
		sends = append(sends, Sent{Seq: uint64(len(sends)), At: epoch.Add(time.Duration(10+k*35) * time.Millisecond), Size: BurstFloor, Kind: KindPadding, Class: ClassPadding, Cluster: c.ID})
	}
	slow := 0
	r, ok := deliver(p, sends, c.Until.Add(OveruseTime), func(i int) time.Time {
		slow++
		return epoch.Add(20*time.Millisecond + time.Duration(slow)*12*time.Millisecond)
	})
	if !ok {
		t.Fatal("no result")
	}
	var bytes int
	for _, s := range sends[1:] {
		bytes += s.Size
	}
	// What was actually offered over the planned cluster.
	offered := int(float64(bytes*8) / c.Until.Sub(epoch).Seconds())
	t.Logf("planned %d, offered %d over the cluster: %+v", c.Rate, offered, r)
	if r.Class != ProbeSendLimited || r.Success || r.Rate > offered {
		t.Fatal("sparse media classified as a complete experiment", r)
	}
	b := &Bandwidth{Config: defaultConfig()}
	b.OnProbe(epoch, r)
	if e := b.Estimate(epoch); e.Ceiling != 0 || e.Proven != r.Rate {
		t.Fatal("a send-limited cluster set a ceiling, or lost its delivery evidence", e)
	}
}

// No feedback: the cluster times out with no evidence. Feedback after the
// timeout is ignored. Partial feedback is missing-feedback: its delivery is
// evidence but proves no target and bounds nothing.
func TestProbeTimeoutAndMissingFeedback(t *testing.T) {
	p := &Discovery{}
	c, _ := p.Maybe(epoch, 2_000_000, 0, Estimate{}, false)
	sends := pacerSends(c, epoch, BurstFloor, 0)
	for _, s := range sends {
		p.OnSent(s)
	}
	if _, ok := p.Tick(c.Until.Add(ProbeTimeout - time.Millisecond)); ok {
		t.Fatal("finished before ProbeTimeout")
	}
	r, ok := p.Tick(c.Until.Add(ProbeTimeout))
	if !ok || r.Class != ProbeTimedOut || r.Rate != 0 || r.Success {
		t.Fatal("no-feedback cluster", ok, r)
	}
	late := Feedback{}
	for _, s := range sends {
		late.Arrivals = append(late.Arrivals, Arrival{Seq: s.Seq, At: s.At.Add(20 * time.Millisecond)})
	}
	if _, ok := p.OnFeedback(c.Until.Add(ProbeTimeout+time.Millisecond), late); ok {
		t.Fatal("late feedback produced a second result")
	}

	p = &Discovery{}
	c, _ = p.Maybe(epoch, 2_000_000, 0, Estimate{}, false)
	sends = pacerSends(c, epoch, BurstFloor, 0)
	for _, s := range sends {
		p.OnSent(s)
	}
	part := Feedback{}
	for _, s := range sends[:len(sends)-3] {
		part.Arrivals = append(part.Arrivals, Arrival{Seq: s.Seq, At: s.At.Add(20 * time.Millisecond)})
	}
	if _, ok := p.OnFeedback(c.Until.Add(OveruseTime), part); ok {
		t.Fatal("finished on partial feedback")
	}
	r, ok = p.Tick(c.Until.Add(ProbeTimeout))
	if !ok || r.Class != ProbeMissing || r.Success || r.Rate <= 0 || r.Rate > c.Rate {
		t.Fatal("missing-feedback cluster", ok, r)
	}
}

// Every pacer cluster below ~100 kbit/s fits in one BurstFloor burst: it
// could have left back-to-back and shows no sustained rate.
func TestProbeUndersizedBurstOnlyCluster(t *testing.T) {
	p := &Discovery{}
	c, _ := p.Maybe(epoch, 60_000, 20_000, Estimate{}, false)
	size := max(1, int(float64(c.Rate)*ProbeLength.Seconds()/8/ProbeMinPackets))
	sends := pacerSends(c, epoch, size, 0)
	r, ok := deliver(p, sends, c.Until.Add(OveruseTime), func(i int) time.Time { return sends[i].At.Add(20 * time.Millisecond) })
	if !ok || r.Class != ProbeUndersized || r.Rate != 0 || r.Success {
		t.Fatal("burst-sized cluster", ok, r)
	}
}

// Only a saturated complete cluster installs D/(1−L̂); every other class
// leaves no bound behind, and a non-saturated delivery above a live bound
// only raises it.
func TestProbeOnlySaturatedSetsCeiling(t *testing.T) {
	p := &Discovery{}
	c, _ := p.Maybe(epoch, 2_000_000, 0, Estimate{}, false)
	sends := pacerSends(c, epoch, BurstFloor, int(float64(c.Rate)*PacerBurst.Seconds()/8))
	// A 1.2 Mbit/s bottleneck: arrivals spaced at its serialisation time.
	bottleneck := 1_200_000
	gap := time.Duration(float64(BurstFloor*8) / float64(bottleneck) * float64(time.Second))
	r, ok := deliver(p, sends, c.Until.Add(time.Second/2), func(i int) time.Time { return epoch.Add(20*time.Millisecond + time.Duration(i)*gap) })
	if !ok || r.Class != ProbeSaturated || r.Success || r.Rate < bottleneck-2 || r.Rate > bottleneck+2 {
		t.Fatal("saturated cluster", ok, r)
	}
	b := &Bandwidth{Config: defaultConfig()}
	b.OnProbe(epoch, r)
	if e := b.Estimate(epoch); e.Ceiling != r.Rate {
		t.Fatal("saturated cluster did not set the ceiling", e)
	}

	// The same delivery shape under any other class bounds nothing.
	for _, class := range []ProbeClass{ProbeDelivered, ProbeSendLimited, ProbeUndersized, ProbeMissing, ProbeTimedOut, ProbeDirty} {
		b := &Bandwidth{Config: defaultConfig()}
		b.OnProbe(epoch, ProbeResult{Class: class, Rate: 1_200_000, Target: 2_000_000, Clean: class != ProbeDirty})
		if e := b.Estimate(epoch); e.Ceiling != 0 {
			t.Fatalf("%v set a ceiling: %+v", class, e)
		}
	}
	// A dirty saturated cluster still measured the bottleneck (its own
	// overshoot raised delay) but is not admission evidence.
	b = &Bandwidth{Config: defaultConfig()}
	b.OnProbe(epoch, ProbeResult{Class: ProbeSaturated, Rate: 1_200_000, Target: 2_000_000})
	if e := b.Estimate(epoch); e.Ceiling != 1_200_000 || e.Proven != 0 {
		t.Fatal("dirty saturated cluster", e)
	}
	// A live ceiling: a lower non-saturated delivery leaves it, a higher one
	// raises it.
	b.OnProbe(epoch, ProbeResult{Class: ProbeSendLimited, Rate: 800_000, Target: 2_000_000, Clean: true})
	if e := b.Estimate(epoch); e.Ceiling != 1_200_000 {
		t.Fatal("send-limited delivery lowered the ceiling", e)
	}
	b.OnProbe(epoch, ProbeResult{Class: ProbeDelivered, Rate: 1_500_000, Target: 2_000_000, Clean: true})
	if e := b.Estimate(epoch); e.Ceiling != 1_500_000 {
		t.Fatal("a delivery above the ceiling did not raise it", e)
	}
}

// Overuse during a cluster whose delivery kept pace is dirty, not a bound.
func TestProbeDirtyWithoutSaturation(t *testing.T) {
	p := &Discovery{}
	c, _ := p.Maybe(epoch, 2_000_000, 0, Estimate{}, false)
	sends := pacerSends(c, epoch, BurstFloor, 0)
	p.Observe(epoch, Estimate{Overuse: true})
	r, ok := deliver(p, sends, c.Until.Add(OveruseTime), func(i int) time.Time { return sends[i].At.Add(20 * time.Millisecond) })
	if !ok || r.Class != ProbeDirty || r.Clean || r.Success {
		t.Fatal("dirty cluster", ok, r)
	}
}
