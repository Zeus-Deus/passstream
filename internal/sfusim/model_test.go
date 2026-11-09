package sfusim

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Model self-tests: each pins one component to a measured fact.

func TestDeterministic(t *testing.T) {
	p := badWifi()
	a, b := Run(p.Build(true), 7, Default), Run(p.Build(true), 7, Default)
	c := Run(p.Build(true), 8, Default)
	same := func(x, y *Result) bool {
		tx, ty := x.Viewers[0].Timeline, y.Viewers[0].Timeline
		if len(tx) != len(ty) {
			return false
		}
		for i := range tx {
			if tx[i] != ty[i] {
				return false
			}
		}
		return true
	}
	if !same(a, b) {
		t.Fatal("same seed produced different timelines")
	}
	if same(a, c) {
		t.Fatal("different seeds produced identical timelines")
	}
}

// constantStream sends n packets at pps through l and returns arrival order.
func constantStream(s *Sim, l *Link, n int, gap time.Duration) *[]int {
	got := &[]int{}
	for i := range n {
		s.At(time.Duration(i)*gap, func() {
			l.Send(&Packet{Size: 200, payload: i, deliver: func(p *Packet) { *got = append(*got, p.payload.(int)) }})
		})
	}
	return got
}

// test/torture/results/netem-gemodel.json: netem "loss gemodel 1% 19% 100% 0%"
// lost 5.48% of 60,000 packets in 571 runs, mean run 5.76.
func TestGilbertElliottMatchesNetem(t *testing.T) {
	s := newSim(1)
	l := newLink(s, 1, LinkConfig{Loss: Loss{P: 0.01, R: 0.19, BadLoss: 1}})
	got := constantStream(s, l, 60_000, 500*time.Microsecond)
	s.Run(time.Minute)
	seen := map[int]bool{}
	for _, i := range *got {
		seen[i] = true
	}
	runs, cur, lost := 0, 0, 0
	for i := range 60_000 {
		if seen[i] {
			if cur > 0 {
				runs++
			}
			cur = 0
		} else {
			cur++
			lost++
		}
	}
	loss, mean := float64(lost)/60_000, float64(lost)/float64(max(1, runs))
	t.Logf("loss %.4f runs %d mean run %.2f (netem 0.0548, 571, 5.76)", loss, runs, mean)
	if loss < 0.045 || loss > 0.065 || mean < 4.6 || mean > 6.9 {
		t.Fatal("Gilbert-Elliott model outside netem measurement")
	}
}

// test/torture/results/netem-order.json: netem jitter without rate
// reordered 82% of 0.1 ms-spaced burst packets; with rate, 0%.
func TestJitterOrdering(t *testing.T) {
	for _, reorder := range []bool{false, true} {
		s := newSim(2)
		l := newLink(s, 1, LinkConfig{Delay: delay(40 * ms), Jitter: 10 * ms, Reorder: reorder})
		got := constantStream(s, l, 2000, 100*time.Microsecond)
		s.Run(time.Second)
		late, high := 0, -1
		for _, i := range *got {
			if i < high {
				late++
			}
			high = max(high, i)
		}
		frac := float64(late) / float64(len(*got))
		t.Logf("reorder=%v: %.3f reordered", reorder, frac)
		if reorder && frac < 0.6 || !reorder && late > 0 {
			t.Fatalf("reorder=%v reordered %.3f", reorder, frac)
		}
	}
}

func TestDropTailRateAndLimit(t *testing.T) {
	s := newSim(3)
	l := newLink(s, 1, LinkConfig{Rate: rate(1_000_000), Delay: delay(10 * ms), Limit: 100})
	got := constantStream(s, l, 2000, 100*time.Microsecond) // 200 B × 10k/s = 16 Mbit/s offered
	s.Run(10 * time.Second)
	if l.QueueDrops == 0 || len(*got) < 100 {
		t.Fatalf("expected tail drops: delivered %d drops %d", len(*got), l.QueueDrops)
	}
	// Delivered bytes cannot exceed rate × busy time (0.2 s offered).
	if bits := len(*got) * 200 * 8; bits > 1_000_000*1/2 {
		t.Fatalf("delivered %d bits beyond the link rate", bits)
	}
}

// A sparse flow beside bulk TCP sees little queueing under FQ-CoDel but the
// full standing queue in a drop-tail FIFO.
func TestFQCoDelIsolatesSparseFlow(t *testing.T) {
	probe := func(q int) time.Duration {
		s := newSim(4)
		l := newLink(s, 1, LinkConfig{Rate: rate(2_000_000), Queue: q, Limit: 1000})
		tcp := newTCP(s, l, newLink(s, 2, LinkConfig{Delay: delay(20 * ms)}), 1, 200_000)
		tcp.Start(0)
		var d []time.Duration
		s.Every(5*time.Second, 100*time.Millisecond, func() {
			at := s.Now()
			l.Send(&Packet{Size: 100, Flow: 99, deliver: func(*Packet) { d = append(d, s.Now()-at) }})
		})
		s.Run(20 * time.Second)
		return Pct(d, .95)
	}
	fq, fifo := probe(FQCoDel), probe(DropTail)
	t.Logf("sparse p95: FQ-CoDel %v, FIFO %v", fq, fifo)
	if fq > 20*ms || fifo < 200*ms {
		t.Fatal("queue disciplines do not separate")
	}
}

func TestSourceRatesAndKeyPause(t *testing.T) {
	s := newSim(5)
	var bytes [3]int
	var keys []*Frame
	var after []time.Duration
	src := newSource(s, 1, ctrl.Native, Detailed, func(f *Frame, i int) {
		if i != 0 {
			return
		}
		if f.Key {
			keys = append(keys, f)
		} else {
			bytes[f.TID] += f.Size
			if len(keys) > 0 && len(after) < len(keys) {
				after = append(after, f.Capture)
			}
		}
	})
	src.Start(0)
	s.Run(60 * time.Second)
	for tid, want := range Detailed.Layer {
		got := float64(bytes[tid]*8) / 60
		if got < 0.85*want || got > 1.1*want {
			t.Fatalf("TID %d rate %.0f, profile %.0f", tid, got, want)
		}
	}
	// The first key's encoder pause, from key ingress completion (a ≈7 ms
	// train at pubRate after pubDelay) to the next frame, scales from
	// 650–840 ms at 251 KB.
	k := keys[0]
	transfer := pubDelay + time.Duration(float64(k.Wire()*8)/pubRate*float64(time.Second))
	pause := after[0] - k.Capture - transfer
	scale := float64(k.Size) / 251_000
	t.Logf("key %d B pause %v", k.Size, pause)
	if pause < time.Duration(scale*600)*ms || pause > time.Duration(scale*900)*ms {
		t.Fatalf("post-key pause %v outside 650–840 ms × %.2f", pause, scale)
	}
}

// TestKeyBoostPause: the production sender's keyframe boost (A16 ruling,
// web/src/keyboost.ts) in the source model. Measured on the 2 Mbit/s screen
// source (test/torture/results/a1w-keyboost/keygap-{2,3}.json.gz), ~235 KB
// keys gave per-key max post-key ingress gaps of 132–155 ms with the boost
// and 373–833 ms with ?keyboost=off. The model must not be optimistic
// against the boosted measurement, must shorten the fixed-cap pause, and
// must keep a 560 KB key (cap ≈12.5 Mbit/s, under the 16 Mbit/s ceiling)
// within the same bound.
func TestKeyBoostPause(t *testing.T) {
	gap := func(seed uint64, size int, boost bool) time.Duration {
		s := newSim(seed)
		var key *Frame
		var next time.Duration
		p := Detailed
		p.KeyBytes = func(*rand.Rand) int { return size }
		src := newSource(s, 1, ctrl.Native, p, func(f *Frame, i int) {
			if i != 0 {
				return
			}
			if f.Key && key == nil {
				key = f
			} else if key != nil && next == 0 {
				next = f.Capture
			}
		})
		if boost {
			src.boost = &keyBoost{}
		}
		src.Start(0)
		s.Run(5 * time.Second)
		return next - key.Capture
	}
	for seed := uint64(1); seed <= 20; seed++ {
		on, off := gap(seed, 235_000, true), gap(seed, 235_000, false)
		big := gap(seed, 560_000, true)
		if on < 155*ms || on >= off || on > 400*ms {
			t.Fatalf("seed %d: 235 KB gap %v boosted, %v fixed (measured 132–155 / 373–833 ms)", seed, on, off)
		}
		if big > 400*ms {
			t.Fatalf("seed %d: 560 KB boosted gap %v", seed, big)
		}
		if seed == 1 {
			t.Logf("235 KB: boosted gap %v, fixed %v; 560 KB boosted %v", on, off, big)
		}
	}
	var b keyBoost
	if c := b.key(0, 560_000*8, 2_150_000); c > boostCeiling || c < 12_000_000 {
		t.Fatalf("560 KB cap %.0f", c)
	}
	// A second key inside the hold adds to the unrepaid debt.
	if c := b.key(100*ms, 560_000*8, 2_150_000); c != boostCeiling {
		t.Fatalf("stacked debt cap %.0f, want the %d ceiling", c, boostCeiling)
	}
	if c := b.key(5*time.Second, 235_000*8, 2_150_000); c > 5_300_000 {
		t.Fatalf("after the hold the debt restarts: cap %.0f", c)
	}
}

// docs/REPORT.md simulator calibration: the browser fifo_tcp_only_512kbit
// probe measured p50 193 ms, p95 247 ms; the model must stay within ±25% at p95.
func TestTCPOnlyQueueCalibrated(t *testing.T) {
	var probe []time.Duration
	for seed := uint64(1); seed <= 5; seed++ {
		probe = append(probe, ProbeDelays(Run(fifoScenario("fifo_tcp_only", 512_000, 1, false), seed, Default), score)...)
	}
	p50, p95 := Pct(probe, .5), Pct(probe, .95)
	t.Logf("probe p50 %v p95 %v (browser 193/247 ms)", p50, p95)
	if p95 < 185*ms || p95 > 309*ms {
		t.Fatal("TCP-only queue outside the calibration tolerance")
	}
}

// o* uses actual packet overhead (C2 review: "Use actual packet overhead
// rather than a universal 5% approximation near a boundary"). The review's
// arithmetic for the clock source: T2 payload ≈202 kbit/s, wire ≈216.
func TestReferenceOption(t *testing.T) {
	sum := 0.0
	for tid := range 3 {
		sum += expectedWire(Clock, tid)
	}
	t.Logf("clock T2 wire %.0f bps (payload %.0f), audio wire %d bps", sum, OfferedRate(&Scenario{Native: Clock}), audioWire*8*50)
	if sum < 210_000 || sum > 222_000 {
		t.Fatalf("clock T2 wire %.0f, review arithmetic ≈216 kbit/s", sum)
	}
	for _, p := range Properties() {
		if !strings.HasPrefix(p.ID, "A10 ") && !strings.HasPrefix(p.ID, "A11 ") {
			continue
		}
		sc := p.Build(false)
		bps := sc.Links[0].Rate[0].Bps
		o, rate, fps := reference(sc, float64(bps)/float64(len(sc.Viewers)+1))
		t.Logf("%-18s share %7.0f: o* %v/T%d rate %.0f fps %.2f", p.ID, float64(bps)/float64(len(sc.Viewers)+1), o.Rendition, o.TID, rate, fps)
	}
}
