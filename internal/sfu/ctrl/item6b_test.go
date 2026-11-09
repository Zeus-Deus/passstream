package ctrl

import (
	"testing"
	"time"
)

// Regressions for C2 item 6b, "estimator, admission, repair"
// (docs/REPORT.md). Each names the Tier A row or review item that exposed it.

// l1t3 feeds a 15 fps L1T3 rendition (TID pattern 0,2,1,2) to m, one
// picture per 1/15 s, each picture's packets back to back. size gives a
// picture's bytes from its number and TID.
type l1t3 struct {
	m       *Ingress
	r       Rendition
	at      time.Time
	picture uint64
}

func (s *l1t3) frame(key bool, bytes int) {
	tid := [4]uint8{0, 2, 1, 2}[s.picture%4]
	if key {
		tid = 0
	}
	at := s.at
	for left := bytes; left > 0; left -= 1_200 {
		s.m.Video(at, s.r, tid, key, s.picture, min(left, 1_200))
		at = at.Add(50 * time.Microsecond)
	}
	s.picture++
	s.at = s.at.Add(time.Second / 15)
}

// detailedPicture is the detailed source's mean picture sizes per TID (1.13/0.35/0.66
// Mbit/s at 15 fps), with a deterministic ±25% variation standing in for the
// encoder's.
func detailedPicture(n uint64) int {
	mean := [3]int{37_667, 11_667, 11_000}[[4]uint8{0, 2, 1, 2}[n%4]]
	z := n*0x9e3779b97f4a7c15 + 0x632be59bd9b4e019 // splitmix64: independent per picture
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	z ^= z >> 31
	return mean + mean*int(z%51)/100 - mean/4
}

// The post-key encoder pause must not read as lower steady demand (A18
// native_1500k): nothing decays while the source is silent, and the cycle
// spanning the pause is not steady demand.
func TestIngressPauseIsNotLowerDemand(t *testing.T) {
	var m Ingress
	k := &RoomKeys{}
	s := &l1t3{m: &m, r: Native, at: epoch}
	for range 60 {
		s.frame(false, 10_000)
	}
	before := m.Costs(s.at, k).Rate[Native][2]
	s.frame(true, 250_000)
	s.at = s.at.Add(800 * time.Millisecond) // the encoder's post-key pause
	during := m.Costs(s.at, k).Rate[Native][2]
	for range 8 {
		s.frame(false, 10_000)
	}
	after := m.Costs(s.at, k).Rate[Native][2]
	if before < 1_150_000 || before > 1_250_000 || during != before || after < before*95/100 {
		t.Fatalf("rate %d before the pause, %d during it, %d after", before, during, after)
	}
}

// C2 §5 A12/A13 meter: the cost of T0 must not swing with the phase of the
// 3.75 base pictures/s against the meter's clock. With 100 ms buckets T0
// moved between 0.91 and 1.28 Mbit/s from one step to the next (A1
// good_join seed 49: four probes chased the same rung's moving target).
func TestIngressBaseCadenceIsNotCostNoise(t *testing.T) {
	var m Ingress
	k := &RoomKeys{}
	s := &l1t3{m: &m, r: Native, at: epoch}
	constant := func(n uint64) int { return [3]int{37_667, 11_667, 11_000}[[4]uint8{0, 2, 1, 2}[n%4]] }
	for range 30 {
		s.frame(false, constant(s.picture))
	}
	lo, hi := [3]int{1 << 62, 1 << 62, 1 << 62}, [3]int{}
	for step := s.at; s.at.Before(epoch.Add(30 * time.Second)); {
		s.frame(false, constant(s.picture))
		for ; step.Before(s.at); step = step.Add(10 * time.Millisecond) {
			c := m.Costs(step, k)
			for t := range 3 {
				lo[t], hi[t] = min(lo[t], c.Rate[Native][t]), max(hi[t], c.Rate[Native][t])
			}
		}
	}
	for tid := range 3 {
		if float64(hi[tid]) > 1.03*float64(lo[tid]) {
			t.Errorf("T%d cost moved between %d and %d bit/s with constant demand", tid, lo[tid], hi[tid])
		}
	}
}

// A held option is not revoked by cost noise: with R 5% above hold(native
// T2) on the meter's steady cost, 40 s of encoder-like size variation must
// not trigger the DownTime downswitch even once.
func TestHeldOptionSurvivesCostNoise(t *testing.T) {
	var m Ingress
	k := &RoomKeys{}
	s := &l1t3{m: &m, r: Native, at: epoch}
	for range 60 {
		s.frame(false, detailedPicture(s.picture))
	}
	steady := m.Costs(s.at, k)
	r := int(1.05 * float64(Hold(steady, Option{Native, 2}, 0, 0)))
	a := &LayerAllocator{}
	a.OnApplied(s.at, Option{Native, 2})
	for step := s.at; s.at.Before(epoch.Add(40 * time.Second)); {
		s.frame(false, detailedPicture(s.picture))
		for ; step.Before(s.at); step = step.Add(OveruseTime) {
			out := a.Decide(step, AllocInput{Estimate: Estimate{Rate: r, Proven: r}, Costs: m.Costs(step, k), Applied: Option{Native, 2}})
			if out.Desired != (Option{Native, 2}) {
				t.Fatalf("held native T2 revoked at %v: desired %+v", step.Sub(epoch), out.Desired)
			}
		}
	}
}

// One outlier base picture is not a demand change: the meter cannot tell
// them apart within its own TrendWindow, so a cost-driven excess must last
// that long before it revokes a held option. A cut in R is still answered
// within DownTime (A18 native_1500k seed 1: a 2.3× TL0 picture lifted T0's
// cost 27% for ≈0.6 s and dropped native T0 on 1.5 Mbit/s).
func TestOutlierPictureDoesNotRevoke(t *testing.T) {
	var m Ingress
	k := &RoomKeys{}
	s := &l1t3{m: &m, r: Native, at: epoch}
	constant := func(n uint64) int { return [3]int{37_667, 11_667, 11_000}[[4]uint8{0, 2, 1, 2}[n%4]] }
	for range 60 {
		s.frame(false, constant(s.picture))
	}
	o := Option{Native, 0}
	r := int(1.15 * float64(Hold(m.Costs(s.at, k), o, 0, 0)))
	a := &LayerAllocator{}
	a.OnApplied(s.at, o)
	decide := func(step time.Time, rate int) Option {
		return a.Decide(step, AllocInput{Estimate: Estimate{Rate: rate, Proven: rate}, Costs: m.Costs(step, k), Applied: o}).Desired
	}
	for s.picture%4 != 0 {
		s.frame(false, constant(s.picture))
	}
	s.frame(false, 87_000) // one outlier TL0
	for step := s.at; s.at.Before(epoch.Add(20 * time.Second)); {
		s.frame(false, constant(s.picture))
		for ; step.Before(s.at); step = step.Add(OveruseTime) {
			if d := decide(step, r); d != o {
				t.Fatalf("outlier picture revoked native T0 at %v: desired %+v", step.Sub(epoch), d)
			}
		}
	}
	cut := s.at
	for step := s.at; ; step = step.Add(OveruseTime) {
		s.frame(false, constant(s.picture))
		if decide(step, r/2) != o {
			if d := step.Sub(cut); d > DownTime+OveruseTime {
				t.Fatalf("a cut in R revoked native T0 only after %v", d)
			}
			return
		}
	}
}

// A temporal upgrade the evidence admits is not reset by cost noise: it
// needs admit(next) ≤ min(R, Proven) continuously for TemporalUpTime, and a
// base-cadence swing above the margin restarted that clock (A1 good_join
// seed 49 stayed on native T1 until a later probe).
func TestTemporalUpSurvivesCostNoise(t *testing.T) {
	var m Ingress
	k := &RoomKeys{}
	s := &l1t3{m: &m, r: Native, at: epoch}
	for range 60 {
		s.frame(false, detailedPicture(s.picture))
	}
	total, _ := Admit(m.Costs(s.at, k), Option{Native, 2}, 0, 0)
	r := int(1.03 * float64(total))
	a := &LayerAllocator{}
	a.OnApplied(s.at, Option{Native, 1})
	start := s.at
	for step := s.at; s.at.Before(start.Add(10 * time.Second)); {
		s.frame(false, detailedPicture(s.picture))
		for ; step.Before(s.at); step = step.Add(OveruseTime) {
			out := a.Decide(step, AllocInput{Estimate: Estimate{Rate: r, Proven: r}, Costs: m.Costs(step, k), Applied: Option{Native, 1}})
			if out.Desired == (Option{Native, 2}) {
				if d := step.Sub(start); d > TemporalUpTime+500*time.Millisecond {
					t.Fatalf("native T2 admitted only after %v", d)
				}
				return
			}
		}
	}
	t.Fatal("native T2 never admitted")
}

// fifoPath is a drop-free FIFO bottleneck with a fixed propagation delay
// and TWCC feedback every 50 ms, for estimator-level regressions.
type fifoPath struct {
	b        *Bandwidth
	rate     func(at time.Time) int // bottleneck bps
	free     time.Time              // when the serializer is next idle
	seq      uint64
	pending  []Arrival
	lastFeed time.Time
}

func (p *fifoPath) send(at time.Time, size int) {
	p.b.OnSent(Sent{Seq: p.seq, At: at, Size: size, Kind: KindVideo, Class: ClassTL0})
	start := later(p.free, at)
	p.free = start.Add(time.Duration(float64(size*8) / float64(p.rate(start)) * float64(time.Second)))
	p.pending = append(p.pending, Arrival{Seq: p.seq, At: p.free.Add(20 * time.Millisecond)})
	p.seq++
}

// feed delivers the arrivals due by now, every 50 ms.
func (p *fifoPath) feed(now time.Time) {
	if now.Sub(p.lastFeed) < 50*time.Millisecond {
		return
	}
	p.lastFeed = now
	var f Feedback
	keep := p.pending[:0]
	for _, a := range p.pending {
		if !a.At.After(now) {
			f.Arrivals = append(f.Arrivals, a)
		} else {
			keep = append(keep, a)
		}
	}
	p.pending = keep
	if len(f.Arrivals) > 0 {
		p.b.OnFeedback(now, f)
	}
}

// C2 §5 A5: a 5 Mbit/s → 256 kbit/s cliff starts a drain episode at the
// first feedback that shows the busy bottleneck's service rate, cuts R to
// Backoff × that rate at once, cancels the pre-cliff delivery proof and
// installs the service as a ceiling; once the sender stops adding to the
// debt, the episode ends after new work arrives and the excess settles.
func TestDrainEpisodeOnCapacityCliff(t *testing.T) {
	b := &Bandwidth{rtt: 50 * time.Millisecond}
	cliff := epoch.Add(10 * time.Second)
	p := &fifoPath{b: b, rate: func(at time.Time) int {
		if at.Before(cliff) {
			return 5_000_000
		}
		return 256_000
	}}
	var episode, cleared time.Time
	next := epoch
	for now := epoch; now.Before(epoch.Add(20 * time.Second)); now = now.Add(time.Millisecond) {
		e := b.Estimate(now)
		if e.Draining && episode.IsZero() {
			episode = now
			if now.Before(cliff) {
				t.Fatalf("drain episode at %v, before the cliff", now.Sub(epoch))
			}
			if e.Rate > int(Backoff*300_000) || e.Proven > 300_000 || e.Ceiling == 0 || e.Ceiling > 300_000 || e.Service == 0 || e.Debt == 0 {
				t.Fatalf("episode at %v: %+v", now.Sub(epoch), e)
			}
		}
		if !episode.IsZero() && !e.Draining && cleared.IsZero() {
			cleared = now
		}
		// 2.5 Mbit/s of media before the episode; afterwards only what a
		// drained path carries (64 kbit/s of audio).
		bps := 2_500_000
		if !episode.IsZero() {
			bps = 64_000
		}
		if !now.Before(next) {
			p.send(now, 1_200)
			next = now.Add(time.Duration(1_200 * 8 * int64(time.Second) / int64(bps)))
		}
		p.feed(now)
	}
	if episode.IsZero() || episode.Sub(cliff) > 250*time.Millisecond {
		t.Fatalf("drain episode at %v after the cliff", episode.Sub(cliff))
	}
	if cleared.IsZero() || cleared.Sub(cliff) > 6*time.Second {
		t.Fatalf("episode at %v never cleared (%v)", episode.Sub(epoch), cleared.Sub(epoch))
	}
}

// A picture serialising on a slow link is the ordinary decrease's
// business, not a drain episode (A11 fq_512k_2v: a 6.6 KB TL0 picture
// queues ≈200 ms at 256 kbit/s).
func TestPictureBurstIsNotADrainEpisode(t *testing.T) {
	b := &Bandwidth{rtt: 50 * time.Millisecond}
	p := &fifoPath{b: b, rate: func(time.Time) int { return 256_000 }}
	for now := epoch; now.Before(epoch.Add(30 * time.Second)); now = now.Add(time.Millisecond) {
		if b.Estimate(now).Draining {
			t.Fatalf("drain episode at %v", now.Sub(epoch))
		}
		// 7 KB TL0 pictures every 267 ms, sent back to back, plus 60 kbit/s.
		if ms := now.Sub(epoch).Milliseconds(); ms%267 == 0 {
			for range 6 {
				p.send(now, 1_200)
			}
		} else if ms%160 == 0 {
			p.send(now, 1_200)
		}
		p.feed(now)
	}
}

// C2 §5 A10: after congestion a native key rests on sustained delivery,
// not the best acked window: a compressed burst through a competitor's
// queue trough (one 500 ms window at 1.9 Mbit/s among 200 kbit/s ones)
// proves temporal upgrades (Proven) but not the native key (Sustained).
func TestCompressedBurstDoesNotProveANativeKey(t *testing.T) {
	now := epoch.Add(10 * time.Second)
	b := &Bandwidth{rate: 2_000_000, congestedAt: now.Add(-time.Second)}
	for i, r := range []int{200_000, 210_000, 1_900_000, 205_000} {
		b.rates = append(b.rates, ratePoint{now.Add(time.Duration(i-4) * 400 * time.Millisecond), r})
	}
	e := b.Estimate(now)
	if e.Proven != 1_900_000 || e.Sustained != 200_000 {
		t.Fatalf("proven %d sustained %d", e.Proven, e.Sustained)
	}
	c := Costs{Rate: [2][3]int{{1_000_000, 1_300_000, 1_600_000}, {60_000, 80_000, 100_000}}, AudioWire: 64_000}
	a := &LayerAllocator{}
	a.OnApplied(epoch, Option{Low, 2})
	out := a.Decide(now, AllocInput{Estimate: e, Costs: c, Applied: Option{Low, 2}, NativeKey: true})
	if out.Desired.Rendition != Low {
		t.Fatalf("a compressed burst admitted native: %+v", out.Desired)
	}
	// The same evidence on a clean path (a cold join) admits it at once.
	e.CongestedAt = time.Time{}
	a = &LayerAllocator{}
	a.OnApplied(epoch, Option{Low, 2})
	if out := a.Decide(now, AllocInput{Estimate: e, Costs: c, Applied: Option{Low, 2}, NativeKey: true}); out.Desired.Rendition != Native {
		t.Fatalf("clean evidence did not admit native: %+v", out.Desired)
	}
}

// C2 §5 A10: recent observed congestion imposes the stability requirement
// (no switch to native within RenditionDwell of a cut), and a requested
// switch is cancelled when its sustainable evidence is lost.
func TestCongestionHoldsAndCancelsTheNativeSwitch(t *testing.T) {
	now := epoch.Add(10 * time.Second)
	c := Costs{Rate: [2][3]int{{1_000_000, 1_300_000, 1_600_000}, {60_000, 80_000, 100_000}}, AudioWire: 64_000}
	e := Estimate{Rate: 5_000_000, Proven: 5_000_000, Sustained: 5_000_000, CongestedAt: now.Add(-time.Second)}
	a := &LayerAllocator{}
	a.OnApplied(epoch, Option{Low, 2})
	if out := a.Decide(now, AllocInput{Estimate: e, Costs: c, Applied: Option{Low, 2}, NativeKey: true}); out.Desired.Rendition != Low {
		t.Fatalf("native %v after a cut", now.Sub(e.CongestedAt))
	}
	if out := a.Decide(now.Add(RenditionDwell), AllocInput{Estimate: e, Costs: c, Applied: Option{Low, 2}, NativeKey: true}); out.Desired.Rendition != Native {
		t.Fatalf("no native RenditionDwell after the cut: %+v", out.Desired)
	}
	// A switch already requested: its evidence collapses under a new cut.
	e = Estimate{Rate: 2_000_000, Proven: 2_000_000, Sustained: 300_000, CongestedAt: now}
	if out := a.Decide(now, AllocInput{Estimate: e, Costs: c, Applied: Option{Low, 2}, NativeKey: true, Switching: true}); out.Desired.Rendition != Low {
		t.Fatalf("a switch without sustainable evidence persisted: %+v", out.Desired)
	}
}

// C2 §5 A10: after congestion a probe offer is capped by the current
// evidence (overdrive × max(Proven, R)); a clean cold join is not.
func TestProbeOfferCappedAfterCongestion(t *testing.T) {
	now := epoch.Add(10 * time.Second)
	p := &Discovery{MaxOverdrive: 1.5}
	c, ok := p.Maybe(now, 2_000_000, 100_000, Estimate{Rate: 300_000, Proven: 250_000, CongestedAt: now.Add(-time.Second)}, false)
	if !ok || c.Rate > 450_000 {
		t.Fatalf("offer %d after congestion (ok %v)", c.Rate, ok)
	}
	p = &Discovery{MaxOverdrive: 1.5}
	if c, ok := p.Maybe(now, 2_000_000, 100_000, Estimate{Rate: 300_000}, false); !ok || c.Rate < 2_000_000 {
		t.Fatalf("cold-join offer %d (ok %v)", c.Rate, ok)
	}
}

// C2 §5 A10: probing resumes after the debt was served even when a
// standing queue remains that is not draining (a competitor's): the
// episode must not become a stay-low-forever state; it settles once the
// path minimum re-bases onto the standing queue (PathWindow).
func TestDrainEndsBehindAStandingQueue(t *testing.T) {
	b := &Bandwidth{rtt: 50 * time.Millisecond}
	cliff := epoch.Add(10 * time.Second)
	p := &fifoPath{b: b, rate: func(at time.Time) int {
		if at.Before(cliff) {
			return 5_000_000
		}
		return 256_000
	}}
	var episode time.Time
	next := epoch
	for now := epoch; now.Before(epoch.Add(30 * time.Second)); now = now.Add(time.Millisecond) {
		e := b.Estimate(now)
		if e.Draining && episode.IsZero() {
			episode = now
		}
		if !episode.IsZero() && !e.Draining {
			// Behind a standing queue the path minimum re-bases within
			// PathWindow of the debt being served.
			if now.Sub(episode) > PathWindow+5*time.Second {
				t.Fatalf("drain lasted %v", now.Sub(episode))
			}
			return
		}
		// Before the episode 2.5 Mbit/s; then exactly the service rate, so
		// the queue the cliff left stands but no longer grows.
		bps := 2_500_000
		if !episode.IsZero() {
			bps = 256_000
		}
		if !now.Before(next) {
			p.send(now, 1_200)
			next = now.Add(time.Duration(1_200 * 8 * int64(time.Second) / int64(bps)))
		}
		p.feed(now)
	}
	t.Fatalf("episode at %v never ended", episode.Sub(epoch))
}

// A packet longer than the burst cap (parity is a few bytes longer than the
// longest packet it protects, so it can exceed BurstFloor) still leaves once
// the tokens reach the cap; it stalled the pacer for good below ≈1 Mbit/s.
func TestPacketLargerThanTheBurstLeaves(t *testing.T) {
	p := &Queue{}
	p.SetRate(epoch, 200_000)
	big := Packet{ID: 1, Size: BurstFloor + 15, FrameSize: BurstFloor + 15, Packets: 1, Last: true, Kind: KindVideo, Class: ClassFEC, ProtectedClass: ClassTL0, Frame: 1}
	p.Push(epoch, big)
	small := Packet{ID: 2, Size: 200, FrameSize: 200, Packets: 1, Last: true, Kind: KindAudio, Class: ClassAudio}
	sent := 0
	for at := epoch; at.Before(epoch.Add(time.Second)); at = at.Add(5 * time.Millisecond) {
		if at.Sub(epoch)%(20*time.Millisecond) == 0 {
			small.ID++
			p.Push(at, small)
		}
		for {
			k, ok := p.Pop(at)
			if !ok {
				break
			}
			if k.ID == 1 {
				sent++
			}
		}
	}
	if sent != 1 {
		t.Fatal("a packet longer than the burst cap never left")
	}
	if b, _ := p.Queued(); b > 2*small.Size {
		t.Fatalf("%d bytes still queued", b)
	}
}

// C2 §5 A12/A13: at loss where a one-per-four group fails more often than
// a bare packet is lost, critical packets get one parity per two.
func TestFECHalvesTheGroupAtHighLoss(t *testing.T) {
	r := DeadlineRepair{}
	// The doubled ratio interleaves by the mean loss run, as the table's
	// last row does: two buckets at a Bernoulli run of ≈1.25.
	for _, c := range []struct {
		loss       float64
		ratio      float64
		interleave int
	}{{0.005, 0, 1}, {0.05, FECRatio, 1}, {0.10, FECRatio, 1}, {0.20, 2 * FECRatio, 2}, {0.30, 2 * FECRatio, 2}} {
		if got, depth := r.FEC(Estimate{Loss: c.loss, LossRun: 1.2}, true); got != c.ratio || depth != c.interleave {
			t.Errorf("loss %.3f: ratio %.2f depth %d, want %.2f/%d", c.loss, got, depth, c.ratio, c.interleave)
		}
	}
	if g, _ := FECGroup(2*FECRatio, 1); g != 2 {
		t.Fatalf("group %d at ratio %.2f", g, 2*FECRatio)
	}
}

// C2 §5 A12/A13: a NACK and a TWCC loss report for the same packet are one
// repair: a second trigger within one RTT of the latest copy is a
// duplicate; a later one (the copy was lost too) is answered, up to
// NACKMaxSends.
func TestResendDeduplicatesAnInFlightCopy(t *testing.T) {
	r := DeadlineRepair{}
	q := RepairQuery{Current: true, RTT: 80 * time.Millisecond, PredictedArrival: epoch, Deadline: epoch.Add(time.Second)}
	if !r.Resend(q) {
		t.Fatal("first trigger refused")
	}
	q.Sends, q.Since = 1, 30*time.Millisecond
	if r.Resend(q) {
		t.Fatal("a duplicate trigger within one RTT was answered")
	}
	q.Since = 90 * time.Millisecond
	if !r.Resend(q) {
		t.Fatal("a trigger after the copy's RTT was refused")
	}
	q.Sends = NACKMaxSends
	if r.Resend(q) {
		t.Fatal("NACKMaxSends exceeded")
	}
}

// C2 §5 A12/A13: lost feedback is a measurement failure, not path
// evidence. A cluster whose first and last packets were reported is
// measured on its reported packets at once; one that was never covered
// retries at the current back-off without doubling it and cannot lower
// earlier probe evidence.
func TestFeedbackLossIsAMeasurementFailure(t *testing.T) {
	p := &Discovery{MaxOverdrive: 1.5}
	now := epoch
	c, ok := p.Maybe(now, 1_000_000, 0, Estimate{Rate: 300_000}, false)
	if !ok {
		t.Fatal("no cluster")
	}
	var sent []Sent
	for i := range 10 {
		s := Sent{Seq: uint64(i), At: now.Add(time.Duration(i) * 8 * time.Millisecond), Size: 1_200, Kind: KindPadding, Class: ClassPadding, Cluster: c.ID}
		p.OnSent(s)
		sent = append(sent, s)
	}
	// Every second report is lost on the reverse path, but the first and
	// last packets were reported.
	var f Feedback
	for i, s := range sent {
		if i%2 == 0 || i == len(sent)-1 {
			f.Arrivals = append(f.Arrivals, Arrival{Seq: s.Seq, At: s.At.Add(20 * time.Millisecond)})
		}
	}
	r, done := p.OnFeedback(c.Until.Add(OveruseTime), f)
	if !done || r.Class == ProbeMissing || r.Rate == 0 {
		t.Fatalf("spanned partial feedback: done %v %+v", done, r)
	}
	// A cluster whose last packet was never reported times out as missing:
	// the back-off does not double.
	p = &Discovery{MaxOverdrive: 1.5, backoff: 4 * time.Second}
	c, _ = p.Maybe(now, 1_000_000, 0, Estimate{Rate: 300_000}, false)
	for i := range 10 {
		p.OnSent(Sent{Seq: uint64(100 + i), At: now.Add(time.Duration(i) * 8 * time.Millisecond), Size: 1_200, Kind: KindPadding, Class: ClassPadding, Cluster: c.ID})
	}
	p.OnFeedback(now.Add(50*time.Millisecond), Feedback{Arrivals: []Arrival{{Seq: 100, At: now.Add(20 * time.Millisecond)}}})
	at := c.Until.Add(ProbeTimeout)
	r, done = p.Tick(at)
	if !done || r.Class != ProbeMissing || p.backoff != 4*time.Second || p.next != at.Add(4*time.Second) {
		t.Fatalf("missing feedback: %+v backoff %v next %v", r, p.backoff, p.next.Sub(at))
	}
	b := &Bandwidth{probeAt: now, probeRate: 2_000_000, rate: 2_000_000}
	b.OnProbe(now.Add(time.Second), ProbeResult{Class: ProbeSendLimited, Clean: true, Rate: 500_000})
	if e := b.Estimate(now.Add(time.Second)); e.Proven != 2_000_000 {
		t.Fatalf("an incomplete result lowered the evidence to %d", e.Proven)
	}
}

// Treat protection overhead consistently in admission and sending (C2 §5
// A12/A13 meter): the forwarder flushes each protected picture's partial
// parity group, so Need charges up to one parity packet per TL0 picture on
// top of the ratio. For one-packet clock TL0 pictures at ratio 0.25 that is
// a whole parity packet per picture, four times what the ratio alone
// charged.
func TestNeedChargesTheFlushedParity(t *testing.T) {
	var m Ingress
	k := &RoomKeys{}
	s := &l1t3{m: &m, r: Native, at: epoch}
	for range 80 {
		s.frame(false, [3]int{1_000, 1_500, 1_200}[[4]uint8{0, 2, 1, 2}[s.picture%4]])
	}
	c := m.Costs(s.at, k)
	o := Option{Native, 0}
	parity := Need(c, o, FECRatio, 0) - Need(c, o, 0, 0)
	// One 1,000-byte TL0 picture per 267 ms: the forwarder sends one
	// parity of 1,015 bytes for each.
	sent := 1_015 * 8 * 15 / 4
	if parity < sent*9/10 || parity > sent*11/10 {
		t.Fatalf("Need charges %d bit/s of parity, the forwarder sends %d", parity, sent)
	}
}

// lossyEstimator reports a fixed rate with non-congestive loss.
type lossyEstimator struct {
	FixedEstimator
	loss float64
}

func (e *lossyEstimator) Estimate(time.Time) Estimate {
	return Estimate{Rate: e.Rate, Proven: e.Rate, Loss: e.loss, RTT: 50 * time.Millisecond}
}

// C2 §5 A12/A13 wire budget: while NACK can repair within the deadline,
// protection that does not fit beside the applied option is reduced rather
// than pricing the option out (A11 fq_512k_2v: FQ-CoDel drops turned FEC on
// and native T0 off); with room for it, the protection stays.
func TestProtectionFitsTheWireBudget(t *testing.T) {
	c := Costs{Rate: [2][3]int{{120_000, 150_000, 200_000}, {50_000, 70_000, 100_000}}, Protected: [2][3]int{{120_000, 120_000, 120_000}, {50_000, 50_000, 50_000}},
		Flush: [2][3]int{{30_000, 30_000, 30_000}, {15_000, 15_000, 15_000}}, AudioWire: 65_000}
	for _, x := range []struct {
		rate int
		fec  float64
	}{{200_000, 0}, {5_000_000, FECRatio}} {
		v := NewViewer(1, NewRoomKeys())
		v.Est = &lossyEstimator{FixedEstimator{Rate: x.rate}, 0.03}
		v.Step(epoch, c)
		v.OnApplied(epoch, Option{Native, 0})
		if d := v.Step(epoch.Add(OveruseTime), c); d.FEC != x.fec || d.Desired != (Option{Native, 0}) && x.rate == 200_000 {
			t.Fatalf("R %d: FEC %.2f desired %+v, want FEC %.2f on native T0", x.rate, d.FEC, d.Desired, x.fec)
		}
	}
}

// 6a escalation A16: a native viewer at native T2 seeks the rate that
// delivers a refresh key within KeyFast, as a low viewer does before its
// switch, on a path that delivers without repair. R is clamped back to
// native T2's admit (pictures are not burst at the fast-key rate), and the
// evidence is kept for key delivery (the healthy co-viewer took ≈0.6 s for
// a 251 KB refresh key at 2.8 Mbit/s, a 585 ms freeze).
func TestNativeViewerSeeksTheFastKeyRate(t *testing.T) {
	c := Costs{Rate: [2][3]int{{1_200_000, 1_600_000, 2_300_000}, {50_000, 70_000, 100_000}}, KeyBytes: [2]int{251_000, 9_000}, KeySamples: [2]int{8, 8}, AudioWire: 65_000}
	e := Estimate{Rate: 3_000_000, Proven: 3_000_000}
	a := &LayerAllocator{}
	a.OnApplied(epoch, Option{Native, 2})
	out := a.Decide(epoch, AllocInput{Estimate: e, Costs: c, Applied: Option{Native, 2}, NativeKey: true})
	want := Hold(c, Option{Native, 2}, 0, 0) + KeyBits(251_000, 0)*int(time.Second)/int(KeyFast)
	if out.Candidate != (Option{Native, 2}) || out.Target != want {
		t.Fatalf("candidate %+v target %d, want native/2 at %d", out.Candidate, out.Target, want)
	}
	e.LostAt = epoch.Add(-time.Second)
	if out := a.Decide(epoch, AllocInput{Estimate: e, Costs: c, Applied: Option{Native, 2}, NativeKey: true}); out.Target != 0 {
		t.Fatalf("a path with recent loss probed for the fast-key rate: %d", out.Target)
	}
	e.LostAt = time.Time{}
	if out := a.Decide(epoch, AllocInput{Estimate: e, Costs: c, Applied: Option{Native, 1}, NativeKey: true}); out.Target == want {
		t.Fatal("native T1 probed for the fast-key rate before its temporal climb")
	}
	v := NewViewer(1, NewRoomKeys())
	v.Step(epoch, c)
	v.OnApplied(epoch, Option{Native, 2})
	v.Est.OnProbe(epoch, ProbeResult{Class: ProbeDelivered, Clean: true, Success: true, Rate: 8_000_000})
	v.switchRung, v.probing = true, false // the rung's cluster finished
	v.Step(epoch.Add(OveruseTime), c)
	top, _ := Admit(c, Option{Native, 2}, 0, 0)
	if x := v.Est.Estimate(epoch.Add(OveruseTime)); x.Rate > top || x.KeyRate != 8_000_000 {
		t.Fatalf("after the fast-key cluster R %d (admit %d) key rate %d", x.Rate, top, x.KeyRate)
	}
	// A congestion cut afterwards withdraws the fast-key evidence.
	b := &Bandwidth{keyRate: 8_000_000, keyAt: epoch, congestedAt: epoch.Add(time.Second), rate: 2_000_000}
	if x := b.Estimate(epoch.Add(2 * time.Second)); x.KeyRate != 0 {
		t.Fatalf("fast-key evidence survived a later cut: %d", x.KeyRate)
	}
}
