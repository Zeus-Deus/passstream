package sfusim

// Tier A properties from docs/GATES.md (revision 2). The reviewer and
// orchestrator own this file; implementers must not edit predicates,
// thresholds or pooling.

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Clock is the ordinary 960×540 clock source (archived r5 medians,
// non-flexible as measured in test/torture/results/vp9-structure.json).
//
// Its low rendition's keys are 2.7–4.4 KB: the 16 low keys the SFU
// assembled in viewer_flap (test/torture/results/flap-diag-{1..4}, server
// trace "keys onkey rend=low") were 2,710–4,365 wire bytes.
var Clock = Profile{FPS: 15, Layer: [3]float64{42_002, 60_601, 99_087}, EncoderBitrate: 4_000_000, KeyBytes: func(r *rand.Rand) int { return 8_500 + r.IntN(1_000) }, Noise: 0.25, Endpoint: 115 * time.Millisecond,
	LowKeyBytes: func(r *rand.Rand) int { return 2_700 + r.IntN(1_700) }}

const ms = time.Millisecond

func delay(d time.Duration) []DelayPoint { return []DelayPoint{{0, d}} }
func rate(bps int) []RatePoint           { return []RatePoint{{0, bps}} }

// fifo is a netem "delay D rate R limit 1000" qdisc.
func fifo(bps int, d time.Duration) LinkConfig {
	return LinkConfig{Rate: rate(bps), Delay: delay(d), Limit: 1000}
}

// Verdict is one seed's outcome.
type Verdict struct {
	Pass   bool
	Detail string
}

// Property is one Tier A row.
type Property struct {
	ID    string
	Build func(short bool) *Scenario
	// Control, when set, runs with the same seed; its result is passed to Check.
	Control func(short bool) *Scenario
	Check   func(r, control *Result) Verdict
	// Pooled, when set, replaces the per-seed check with a pooled predicate.
	// Otherwise every seed must pass (GATES.md Tier A "Setup": per-seed
	// properties pass all 100 seeds; Evaluate's Need is the seed count).
	Pooled func(rs []*Result) Verdict
	// Capacity is the video rate an omniscient estimator would use for
	// viewer v at time t (internal oracle; not part of the gate).
	Capacity func(v int, t time.Duration) int
}

func failf(format string, a ...any) Verdict { return Verdict{false, fmt.Sprintf(format, a...)} }
func passf(format string, a ...any) Verdict { return Verdict{true, fmt.Sprintf(format, a...)} }

func flat(bps int) func(int, time.Duration) int { return func(int, time.Duration) int { return bps } }

// Properties is the Tier A suite in docs/GATES.md order.
func Properties() []Property {
	ps := []Property{goodJoin(), badWifi(), isolation()}
	for _, rt := range []int{3_000_000, 5_000_000, 10_000_000} {
		for _, pre := range []bool{true, false} {
			ps = append(ps, b1(rt, pre, 0))
		}
	}
	ps = append(ps, b1(3_000_000, true, 560_000), b1(3_000_000, false, 560_000))
	ps = append(ps, b1Flap(), viewer256k(), rttStep(), outage(), spam())
	for _, rt := range []int{512_000, 2_000_000} {
		for _, n := range []int{1, 2} {
			ps = append(ps, fifoCompetition(rt, n))
		}
	}
	for _, rt := range []int{512_000, 2_000_000} {
		for _, n := range []int{1, 2} {
			ps = append(ps, fqCompetition(rt, n))
		}
	}
	ps = append(ps, lossRow(0.05), lossRow(0.20), lossPrejoin(0.05), lossPrejoin(0.20))
	ps = append(ps, joinStorm(), pairChange(), collapse(), tl1Loss(), native1500k(true), native1500k(false))
	return append(ps, pcReplacement(""), pcReplacement("repeat"), pcReplacement("constrained"))
}

// A1
func goodJoin() Property {
	return Property{ID: "A1 good_join", Capacity: flat(50_000_000),
		Build: func(bool) *Scenario {
			return &Scenario{Name: "good_join", Duration: 12 * time.Second, Native: Detailed, Audio: true,
				Links:   []LinkConfig{fifo(50_000_000, 20*ms), {Delay: delay(20 * ms)}},
				Viewers: []ViewerConfig{{Join: 2 * time.Second, Fwd: 0, Rev: 1, NominalRTT: 40 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			full := FirstNative(v, Window{v.Join, r.Scenario.Duration}, true)
			if v.TTFF < 0 || v.TTFF > time.Second || full < 0 || full > 2*time.Second {
				return failf("ttff %v full native %v", v.TTFF, full)
			}
			return passf("ttff %v full native %v", v.TTFF, full)
		}}
}

// audioGaps pools audio continuity (reported, not gated): gaps >200 ms and
// frames that never played.
func audioGaps(rs []*Result) (gaps, missing int) {
	for _, r := range rs {
		for _, v := range r.Viewers {
			gaps += len(v.AudioGaps)
			missing += v.AudioMissing
		}
	}
	return gaps, missing
}

// A2
func badWifi() Property {
	return Property{ID: "A2 bad_wifi", Capacity: flat(1_000_000_000),
		Build: func(short bool) *Scenario {
			d := 301 * time.Second
			if short {
				d = 61 * time.Second
			}
			wifi := LinkConfig{Delay: delay(40 * ms), Jitter: 10 * ms, Loss: Loss{P: 0.01, R: 0.19, BadLoss: 1}}
			return &Scenario{Name: "bad_wifi", Duration: d, Native: Detailed, Audio: true, Links: []LinkConfig{wifi},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 0, NominalRTT: 80 * ms}}}
		},
		Pooled: func(rs []*Result) Verdict {
			freezes, seconds, long := 0, 0.0, 0
			var g2g []time.Duration
			native, samples := 0.0, 0
			for _, r := range rs {
				v := r.Viewers[0]
				w := Window{6 * time.Second, r.Scenario.Duration}
				f := Frames(v.Timeline, w)
				freezes += len(Gaps(f, w))
				seconds += (w.To - w.From).Seconds()
				if MaxGap(f, w) > 2*time.Second {
					long++
				}
				g2g = append(g2g, G2Gs(f)...)
				n := len(Samples(v, w))
				native += NativeShare(v, w, true) * float64(n)
				samples += n
			}
			rate := float64(freezes) / (seconds / 300)
			p95 := Pct(g2g, .95)
			share := native / float64(max(1, samples))
			maxLong := len(rs) * 5 / 100
			gaps, missing := audioGaps(rs)
			d := fmt.Sprintf("freezes %.2f/300s, seeds with >2s gap %d (≤%d), G2G p95 %v, full native %.3f, audio gaps >200ms %d, audio frames missing %d", rate, long, maxLong, p95, share, gaps, missing)
			return Verdict{rate <= 1 && long <= maxLong && p95 >= 0 && p95 <= 400*ms && share >= 0.9, d}
		}}
}

// A3
func isolation() Property {
	build := func(slow bool) *Scenario {
		sc := &Scenario{Name: "isolation", Duration: 60 * time.Second, Native: Detailed,
			Links:   []LinkConfig{fifo(50_000_000, 20*ms), fifo(50_000_000, 20*ms), fifo(256_000, 40*ms), {Delay: delay(20 * ms)}, {Delay: delay(20 * ms)}},
			Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 3, NominalRTT: 40 * ms}, {Join: time.Second, Fwd: 1, Rev: 4, NominalRTT: 40 * ms}}}
		if slow {
			sc.Viewers = append(sc.Viewers, ViewerConfig{Join: time.Second, Fwd: 2, Rev: Ideal, NominalRTT: 40 * ms})
		}
		return sc
	}
	return Property{ID: "A3 isolation",
		Capacity: func(v int, _ time.Duration) int { return [3]int{50_000_000, 50_000_000, 256_000}[v] },
		Build:    func(bool) *Scenario { return build(true) }, Control: func(bool) *Scenario { return build(false) },
		Check: func(r, c *Result) Verdict {
			w := Window{10 * time.Second, 60 * time.Second}
			for i := range 2 {
				v, cv := r.Viewers[i], c.Viewers[i]
				f := Frames(v.Timeline, w)
				p, cp := Pct(G2Gs(f), .95), Pct(G2Gs(Frames(cv.Timeline, w)), .95)
				if g, s := len(Gaps(f, w)), NativeShare(v, w, true); g > 0 || s < 1 || p > cp+50*ms {
					return failf("viewer %d: freezes %d full native %.3f p95 %v control %v", i, g, s, p, cp)
				}
			}
			return passf("healthy viewers unaffected")
		}}
}

// keyOf returns a profile whose native keys are all `bytes` (0 = archived).
func keyOf(p Profile, bytes int) Profile {
	if bytes > 0 {
		p.KeyBytes = func(*rand.Rand) int { return bytes }
	}
	return p
}

// A4, A4k
func b1(bps int, prejoin bool, key int) Property {
	history := map[bool]string{true: "prejoin", false: "established"}[prejoin]
	name, id := fmt.Sprintf("b1_%dm_%s", bps/1_000_000, history), "A4 "
	if key > 0 {
		name, id = fmt.Sprintf("b1_%dm_%dk_%s", bps/1_000_000, key/1000, history), "A4k "
	}
	t0 := 10 * time.Second
	if prejoin {
		t0 = time.Second
	}
	return Property{ID: id + name,
		Capacity: func(_ int, t time.Duration) int {
			if t < t0 {
				return 50_000_000
			}
			return bps
		},
		Build: func(bool) *Scenario {
			l := LinkConfig{Rate: []RatePoint{{0, 50_000_000}, {t0, bps}}, Delay: delay(40 * ms), Limit: 1000}
			return &Scenario{Name: name, Duration: t0 + 51*time.Second, Native: keyOf(Detailed, key), Audio: true, Links: []LinkConfig{l},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
		},
		Check: func(r, _ *Result) Verdict { return b1Check(r, r.Viewers[0], t0) }}
}

func b1Check(r *Result, v *ViewerResult, t0 time.Duration) Verdict {
	first := FirstNative(v, Window{t0, t0 + 20*time.Second}, true)
	w := Window{t0 + 20*time.Second, t0 + 50*time.Second}
	share, episode, pad := NativeShare(v, w, true), LongestNonNative(v, w), PaddingShare(v, w)
	fresh, src := len(Fresh(v.Timeline, w)), SourceFrames(r, ctrl.Native, w)
	d := fmt.Sprintf("first full native %v share %.3f episode %v fresh %d/%d padding %.3f audio gaps %d", first, share, episode, fresh, src, pad, len(v.AudioGaps))
	return Verdict{pad <= 0.10 && first >= 0 && share >= 0.95 && episode <= 2*time.Second && float64(fresh) >= 0.9*float64(src), d}
}

// A5
func b1Flap() Property {
	t := [3]time.Duration{time.Second, 52 * time.Second, 103 * time.Second}
	return Property{ID: "A5 b1_flap",
		Capacity: func(_ int, at time.Duration) int {
			if at >= t[1] && at < t[2] {
				return 256_000
			}
			return 5_000_000
		},
		Build: func(bool) *Scenario {
			l := LinkConfig{Rate: []RatePoint{{0, 5_000_000}, {t[1], 256_000}, {t[2], 5_000_000}}, Delay: delay(40 * ms), Limit: 1000}
			return &Scenario{Name: "b1_flap", Duration: t[2] + 51*time.Second, Native: Detailed, Audio: true, Links: []LinkConfig{l},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			for _, k := range []int{0, 2} {
				first := FirstNative(v, Window{t[k], t[k] + 20*time.Second}, true)
				pad := PaddingShare(v, Window{t[k] + 21*time.Second, t[k] + 51*time.Second})
				if first < 0 || pad > 0.10 {
					return failf("5M interval %d: first full native %v padding %.3f", k, first, pad)
				}
			}
			w := Window{t[1] + 3*time.Second, t[2]}
			f := Frames(v.Timeline, w)
			if g, p := MaxGap(Fresh(v.Timeline, w), w), Pct(G2Gs(f), .95); g > time.Second || p < 0 || p > time.Second {
				return failf("256k interval: max fresh gap %v p95 %v", g, p)
			}
			return passf("ok")
		}}
}

// A6
func viewer256k() Property {
	return Property{ID: "A6 viewer_256k",
		Capacity: func(_ int, t time.Duration) int {
			if t < 5*time.Second {
				return 50_000_000
			}
			return 256_000
		},
		Build: func(bool) *Scenario {
			l := LinkConfig{Rate: []RatePoint{{0, 50_000_000}, {5 * time.Second, 256_000}}, Delay: delay(40 * ms), Limit: 1000}
			return &Scenario{Name: "viewer_256k", Duration: 45 * time.Second, Native: Detailed, Links: []LinkConfig{l},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			w := Window{15 * time.Second, 45 * time.Second}
			g, p := MaxGap(Fresh(v.Timeline, w), w), Pct(G2Gs(Frames(v.Timeline, w)), .95)
			return Verdict{g <= time.Second && p >= 0 && p <= 440*ms, fmt.Sprintf("max fresh gap %v p95 %v", g, p)}
		}}
}

// A7
func rttStep() Property {
	return Property{ID: "A7 rtt_step", Capacity: flat(5_000_000),
		Build: func(bool) *Scenario {
			d := []DelayPoint{{0, 40 * ms}, {30 * time.Second, 115 * ms}}
			return &Scenario{Name: "rtt_step", Duration: 90 * time.Second, Native: Detailed,
				Links:   []LinkConfig{{Rate: rate(5_000_000), Delay: d, Limit: 1000}, {Delay: d}},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 1, NominalRTT: 80 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			w := Window{30 * time.Second, 90 * time.Second}
			downs := 0
			s := Samples(v, w)
			for i := 1; i < len(s); i++ {
				if s[i-1].Forwarded == ctrl.Native && s[i].Forwarded == ctrl.Low {
					downs++
				}
			}
			share, g := NativeShare(v, w, true), MaxGap(Frames(v.Timeline, w), w)
			return Verdict{share >= 0.9 && g <= time.Second && downs <= 1, fmt.Sprintf("full native %.3f max gap %v downswitches %d", share, g, downs)}
		}}
}

// recovery finds the first of 3 fresh frames within 1 s captured after t.
// GATES.md A8: "Recovery (first of 3 fresh frames within 1 s, captured after
// restore)"; Definitions: "Fresh frame: G2G ≤1 s, with an advancing picture".
// A19 measures recovery the same way.
func recovery(v *ViewerResult, t, end time.Duration) (rec, post time.Duration, ok bool) {
	var fresh []Rendered
	for _, f := range v.Timeline {
		if f.Capture >= t && f.G2G() <= time.Second {
			fresh = append(fresh, f)
		}
	}
	for i := 0; i+2 < len(fresh); i++ {
		if fresh[i+2].At-fresh[i].At <= time.Second {
			return fresh[i].At - t, MaxGap(fresh[i:], Window{fresh[i].At, end}), true
		}
	}
	return 0, 0, false
}

// A8
func outage() Property {
	o := []Interval{{20 * time.Second, 22500 * ms}}
	return Property{ID: "A8 outage", Capacity: flat(50_000_000),
		Build: func(bool) *Scenario {
			return &Scenario{Name: "outage", Duration: 40 * time.Second, Native: Detailed,
				Links:   []LinkConfig{{Rate: rate(50_000_000), Delay: delay(20 * ms), Limit: 1000, Outages: o}, {Delay: delay(20 * ms), Outages: o}},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 1, NominalRTT: 40 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			rec, post, ok := recovery(r.Viewers[0], o[0].To, r.Scenario.Duration)
			if !ok {
				return failf("no recovery")
			}
			return Verdict{rec <= time.Second && post < 1500*ms, fmt.Sprintf("recovery %v post gap %v", rec, post)}
		}}
}

// A9
func spam() Property {
	build := func(spam bool) *Scenario {
		sc := &Scenario{Name: "spam", Duration: 45 * time.Second, Native: Detailed}
		for i := range 3 {
			sc.Links = append(sc.Links, fifo(50_000_000, 20*ms), LinkConfig{Delay: delay(20 * ms)})
			vc := ViewerConfig{Join: time.Second, Fwd: 2 * i, Rev: 2*i + 1, NominalRTT: 40 * ms}
			if spam && i == 0 {
				vc.Spam = []Interval{{10 * time.Second, 40 * time.Second}}
			}
			sc.Viewers = append(sc.Viewers, vc)
		}
		return sc
	}
	return Property{ID: "A9 spam", Capacity: flat(50_000_000),
		Build: func(bool) *Scenario { return build(true) }, Control: func(bool) *Scenario { return build(false) },
		Check: func(r, c *Result) Verdict {
			w := Window{10 * time.Second, 40 * time.Second}
			k, ck := NativeKeys(r, w), NativeKeys(c, w)
			if k > ck {
				return failf("native keys %d > control %d", k, ck)
			}
			for i := 1; i < 3; i++ {
				f, cf := len(Gaps(Frames(r.Viewers[i].Timeline, w), w)), len(Gaps(Frames(c.Viewers[i].Timeline, w), w))
				if f > cf {
					return failf("viewer %d freezes %d > control %d", i, f, cf)
				}
				if s := NativeShare(r.Viewers[i], w, true); s < 0.9 {
					return failf("viewer %d full native %.3f", i, s)
				}
			}
			return passf("native keys %d control %d", k, ck)
		}}
}

// fifoScenario is web/torture.mjs's shared deep FIFO: both directions, every
// viewer and the bulk flow in one netem qdisc.
func fifoScenario(name string, bps, n int, video bool) *Scenario {
	sc := &Scenario{Name: name, Duration: 45 * time.Second, Native: Detailed, Audio: video, NoVideo: !video,
		Links: []LinkConfig{fifo(bps, 40*ms)}, TCP: []TCPFlow{{Data: 0, Ack: 0, Start: 5 * time.Second}}, Probe: &QueueProbe{Link: 0}}
	if video {
		for range n {
			sc.Viewers = append(sc.Viewers, ViewerConfig{Join: time.Second, Fwd: 0, Rev: 0, NominalRTT: 80 * ms})
		}
	}
	return sc
}

var score = Window{15 * time.Second, 45 * time.Second}

// reference is o* (docs/GATES.md Definitions; C2 ruling for A11): the most
// preferred option whose total wire hold fits the share. The hold is
// ctrl.Hold over the profiles' expected wire rates: audio wire, the option's
// video payload plus its actual per-packet overhead, and the protection and
// repair the controller applies at the forward link's configured
// non-congestive loss (none on the A10/A11 links). rate(o*) is the option's
// media rate and fps(o*) source fps × {T2: 1, T1: ½, T0: ¼}.
func reference(sc *Scenario, share float64) (o ctrl.Option, rate, fps float64) {
	low := sc.Low
	if low.FPS == 0 {
		low = LowProfile
	}
	var c ctrl.Costs
	if sc.Audio {
		c.AudioWire = audioWire * 8 * 50
	}
	for r, p := range [2]Profile{sc.Native, low} {
		sum := 0.0
		for t := range 3 {
			sum += expectedWire(p, t)
			c.Rate[r][t], c.Protected[r][t] = int(sum), int(expectedWire(p, 0))
		}
	}
	loss, run := 0.0, 1.0
	if len(sc.Viewers) > 0 && sc.Viewers[0].Fwd >= 0 {
		loss, run = configuredLoss(sc.Links[sc.Viewers[0].Fwd].Loss)
	}
	fec, _ := ctrl.DeadlineRepair{}.FEC(ctrl.Estimate{Loss: loss, LossRun: run}, true)
	for _, o := range ctrl.Options {
		if float64(ctrl.Hold(c, o, fec, loss)) <= share {
			p := [2]Profile{sc.Native, low}[o.Rendition]
			r := 0.0
			for t := 0; t <= int(o.TID); t++ {
				r += p.Layer[t]
			}
			return o, r, p.FPS / float64(int(1)<<(2-o.TID))
		}
	}
	return ctrl.Option{Rendition: ctrl.Low}, 0, 0
}

// expectedWire is TID t's mean wire rate under the source model: payload
// plus rtpOverhead per packet, with packets per picture averaged over the
// lognormal picture-size distribution Source draws from (deterministic
// quadrature).
func expectedWire(p Profile, t int) float64 {
	share := [3]float64{4, 4, 2}[t]
	mean := p.Layer[t] * share / p.FPS / 8
	const n = 1000
	pkts := 0.0
	for k := range n {
		z := math.Sqrt2 * math.Erfinv(2*(float64(k)+0.5)/n-1)
		size := max(200, mean*math.Exp(p.Noise*z-p.Noise*p.Noise/2))
		pkts += math.Ceil(size / rtpPayload)
	}
	return p.Layer[t] + p.FPS/share*pkts/n*rtpOverhead*8
}

// configuredLoss is a link's stationary non-congestive loss and mean loss
// run in packets.
func configuredLoss(l Loss) (loss, run float64) {
	loss, run = l.Bernoulli, 1
	if l.P > 0 || l.R > 0 {
		bad := l.P / (l.P + l.R)
		loss += bad*l.BadLoss + (1-bad)*l.GoodLoss
		if l.R > 0 && l.BadLoss == 1 {
			run = 1 / l.R
		}
	}
	return loss, run
}

// A10
func fifoCompetition(bps, n int) Property {
	name := fmt.Sprintf("fifo_%s_%dv", kbit(bps), n)
	return Property{ID: "A10 " + name, Capacity: flat(bps / (n + 1)),
		Build:   func(bool) *Scenario { return fifoScenario(name, bps, n, true) },
		Control: func(bool) *Scenario { return fifoScenario("fifo_tcp_only", bps, n, false) },
		Check: func(r, c *Result) Verdict {
			limit := Pct(ProbeDelays(c, score), .95) + 500*ms
			o, want, fps := reference(r.Scenario, float64(bps)/float64(n+1))
			minFresh := int(0.9 * fps * (score.To - score.From).Seconds())
			for i, v := range r.Viewers {
				fresh := Fresh(v.Timeline, score)
				g, p, got := MaxGap(fresh, score), Pct(G2Gs(Frames(v.Timeline, score)), .95), UniqueRate(v, score)
				native := o.Rendition != ctrl.Native || NativeShare(v, score, false) >= 0.9
				if g > time.Second || p < 0 || p > limit || len(fresh) < minFresh || got < 0.9*want || !native {
					return failf("viewer %d (o* %v/T%d): gap %v p95 %v (≤%v) fresh %d (≥%d) unique %.0f (≥%.0f) native %v", i, o.Rendition, o.TID, g, p, limit, len(fresh), minFresh, got, 0.9*want, native)
				}
			}
			if tcp := TCPGoodput(r, 0, score) / float64(bps); tcp < 0.3 {
				return failf("tcp share %.3f", tcp)
			}
			return passf("o* %v/T%d p95 limit %v", o.Rendition, o.TID, limit)
		}}
}

// A11
func fqCompetition(bps, n int) Property {
	name := fmt.Sprintf("fq_%s_%dv", kbit(bps), n)
	build := func(bool) *Scenario {
		sc := &Scenario{Name: name, Duration: 45 * time.Second, Native: Clock, Audio: true,
			Links: []LinkConfig{{Rate: rate(bps), Queue: FQCoDel, Limit: 1000}}, TCP: []TCPFlow{{Data: 0, Ack: Ideal, Start: 5 * time.Second}}}
		for range n {
			sc.Viewers = append(sc.Viewers, ViewerConfig{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 20 * ms})
		}
		return sc
	}
	// The C2 review (checkpoint ruling "A11 fq_512k_1v: use the most preferred
	// AFFORDABLE option") replaces the "n × offered ≤ link/2" floor: "Select
	// o* from total wire hold (including audio and applicable
	// protection/repair) against link/(n+1), then require ≥90% reference
	// unique media/cadence and native pixels whenever native T0 fits. Require
	// full native only when T2 is the affordable reference. Retain TCP ≥30%,
	// fresh-gap and latency bounds. No special case for fq_512k_1v." The
	// docs/GATES.md A11 row still states the old floor.
	return Property{ID: "A11 " + name, Capacity: flat(bps / (n + 1)), Build: build,
		Check: func(r, _ *Result) Verdict {
			o, want, fps := reference(r.Scenario, float64(bps)/float64(n+1))
			minFresh := 0.9 * fps * (score.To - score.From).Seconds()
			for i, v := range r.Viewers {
				fresh := Fresh(v.Timeline, score)
				g, p := MaxGap(fresh, score), Pct(G2Gs(Frames(v.Timeline, score)), .95)
				if g > time.Second || p < 0 || p > time.Second {
					return failf("viewer %d: gap %v p95 %v", i, g, p)
				}
				got, native, full := UniqueRate(v, score), NativeShare(v, score, false), NativeShare(v, score, true)
				if got < 0.9*want || float64(len(fresh)) < minFresh ||
					o.Rendition == ctrl.Native && native < 0.9 || o == (ctrl.Option{Rendition: ctrl.Native, TID: 2}) && full < 0.9 {
					return failf("viewer %d (o* %v/T%d): unique %.0f (≥%.0f) fresh %d (≥%.0f) native %.3f full native %.3f", i, o.Rendition, o.TID, got, 0.9*want, len(fresh), minFresh, native, full)
				}
			}
			if tcp := TCPGoodput(r, 0, score) / float64(bps); tcp < 0.3 {
				return failf("tcp share %.3f", tcp)
			}
			return passf("o* %v/T%d", o.Rendition, o.TID)
		}}
}

// A12
func lossRow(p float64) Property {
	name := fmt.Sprintf("loss_%d", int(p*100))
	return Property{ID: "A12 " + name, Capacity: func(int, time.Duration) int { return int(1e9 * (1 - p)) },
		Build: func(bool) *Scenario {
			l := LinkConfig{Delay: delay(20 * ms), Jitter: 5 * ms, Loss: Loss{Bernoulli: p}}
			return &Scenario{Name: name, Duration: 61 * time.Second, Native: Clock, Links: []LinkConfig{l},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 0, NominalRTT: 40 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			w := Window{6 * time.Second, 61 * time.Second}
			f := Frames(v.Timeline, w)
			g, q := MaxGap(f, w), Pct(G2Gs(f), .95)
			return Verdict{g <= time.Second && q >= 0 && q <= 420*ms, fmt.Sprintf("max gap %v p95 %v", g, q)}
		}}
}

// A13: persistent non-congestive loss must not veto discovery (C0 review R-2).
func lossPrejoin(p float64) Property {
	name := fmt.Sprintf("loss_prejoin_%d", int(p*100))
	return Property{ID: "A13 " + name, Capacity: func(int, time.Duration) int { return int(50e6 * (1 - p)) },
		Build: func(bool) *Scenario {
			l := LinkConfig{Rate: rate(50_000_000), Delay: delay(20 * ms), Jitter: 5 * ms, Limit: 1000, Loss: Loss{Bernoulli: p}}
			return &Scenario{Name: name, Duration: 52 * time.Second, Native: Detailed, Audio: true, Links: []LinkConfig{l},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 0, NominalRTT: 40 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			first := FirstNative(v, Window{time.Second, 21 * time.Second}, true)
			share := NativeShare(v, Window{21 * time.Second, 51 * time.Second}, true)
			return Verdict{first >= 0 && share >= 0.9, fmt.Sprintf("first full native %v share %.3f", first, share)}
		}}
}

// A14: joins share the room key budget (R-5).
func joinStorm() Property {
	const joiners = 20
	return Property{ID: "A14 join_storm", Capacity: flat(50_000_000),
		Build: func(bool) *Scenario {
			sc := &Scenario{Name: "join_storm", Duration: 50 * time.Second, Native: Detailed}
			for i := range 2 + joiners {
				sc.Links = append(sc.Links, fifo(50_000_000, 20*ms), LinkConfig{Delay: delay(20 * ms)})
				join := time.Second
				if i >= 2 {
					join = 10*time.Second + time.Duration(i-2)*500*ms
				}
				sc.Viewers = append(sc.Viewers, ViewerConfig{Join: join, Fwd: 2 * i, Rev: 2*i + 1, NominalRTT: 40 * ms})
			}
			return sc
		},
		Check: func(r, _ *Result) Verdict {
			w := Window{10 * time.Second, 30 * time.Second}
			keys := NativeKeys(r, w)
			if keys > 8 {
				return failf("native keys %d > 8", keys)
			}
			for i := range 2 {
				v := r.Viewers[i]
				if f := len(Gaps(Frames(v.Timeline, w), w)); f > keys {
					return failf("healthy %d freezes %d > native keys %d", i, f, keys)
				}
				if s := NativeShare(v, w, true); s < 0.9 {
					return failf("healthy %d full native %.3f", i, s)
				}
			}
			for i, v := range r.Viewers[2:] {
				if v.TTFF < 0 || v.TTFF > 4*time.Second {
					return failf("joiner %d ttff %v", i, v.TTFF)
				}
				if FirstNative(v, Window{v.Join, 40 * time.Second}, false) < 0 {
					return failf("joiner %d not native by 40 s", i)
				}
			}
			return passf("native keys %d", keys)
		}}
}

// A15: a real pair change with late old-path feedback (R-10).
func pairChange() Property {
	return Property{ID: "A15 pair_change",
		Capacity: func(_ int, t time.Duration) int {
			if t < 20*time.Second {
				return 50_000_000
			}
			return 256_000
		},
		Build: func(bool) *Scenario {
			return &Scenario{Name: "pair_change", Duration: 50 * time.Second, Native: Detailed,
				Links: []LinkConfig{fifo(50_000_000, 20*ms), {Delay: delay(420 * ms)}, fifo(256_000, 40*ms), {Delay: delay(40 * ms)}},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 1, NominalRTT: 40 * ms,
					Switch: &PathSwitch{At: 20 * time.Second, Fwd: 2, Rev: 3}}}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			for _, s := range Samples(v, Window{23 * time.Second, 50 * time.Second}) {
				if s.Forwarded == ctrl.Native {
					return failf("native forwarded at %v on the 256 kbit/s path", s.At)
				}
			}
			w := Window{25 * time.Second, 50 * time.Second}
			g, p := MaxGap(Fresh(v.Timeline, w), w), Pct(G2Gs(Frames(v.Timeline, w)), .95)
			return Verdict{g <= time.Second && p >= 0 && p <= 440*ms, fmt.Sprintf("max fresh gap %v p95 %v", g, p)}
		}}
}

// A16: capacity collapse with hard queue bounds (R-6).
func collapse() Property {
	return Property{ID: "A16 collapse",
		Capacity: func(v int, t time.Duration) int {
			if v == 0 && t >= 20*time.Second && t < 30*time.Second {
				return 64_000
			}
			return 10_000_000
		},
		Build: func(bool) *Scenario {
			l := LinkConfig{Rate: []RatePoint{{0, 10_000_000}, {20 * time.Second, 64_000}, {30 * time.Second, 10_000_000}}, Delay: delay(20 * ms), Limit: 1000}
			return &Scenario{Name: "collapse", Duration: 50 * time.Second, Native: Detailed, SampleQueue: true,
				Links:   []LinkConfig{l, fifo(50_000_000, 20*ms)},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 40 * ms}, {Join: time.Second, Fwd: 1, Rev: Ideal, NominalRTT: 40 * ms}}}
		},
		Check: func(r, _ *Result) Verdict {
			v, h := r.Viewers[0], r.Viewers[1]
			if v.MaxQueuedBytes > ctrl.QueueBytes || v.MaxQueuedFrames > ctrl.QueueFrames {
				return failf("queue %d B / %d frames exceeds bounds", v.MaxQueuedBytes, v.MaxQueuedFrames)
			}
			w := Window{10 * time.Second, 50 * time.Second}
			if f := len(Gaps(Frames(h.Timeline, w), w)); f > 0 {
				return failf("co-viewer freezes %d", f)
			}
			if fresh := Fresh(v.Timeline, Window{30 * time.Second, 33 * time.Second}); len(fresh) == 0 {
				return failf("no fresh frames within 3 s of restore")
			}
			if FirstNative(v, Window{30 * time.Second, 50 * time.Second}, true) < 0 {
				return failf("not full native by 50 s")
			}
			return passf("max queue %d B / %d frames", v.MaxQueuedBytes, v.MaxQueuedFrames)
		}}
}

// A17: the non-flexible TL1-loss dependency rule (R-7).
func tl1Loss() Property {
	return Property{ID: "A17 tl1_loss", Capacity: flat(50_000_000),
		Build: func(bool) *Scenario {
			n, last, lost := 0, (*Frame)(nil), false
			return &Scenario{Name: "tl1_loss", Duration: 60 * time.Second, Native: Clock,
				Links:   []LinkConfig{fifo(50_000_000, 20*ms), {Delay: delay(20 * ms)}},
				Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: 1, NominalRTT: 40 * ms}},
				IngressLoss: func(f *Frame) bool { // called per packet; decided per picture
					if f != last {
						last, lost = f, false
						if f.Rend == ctrl.Native && f.TID == 1 && f.Packets >= 2 {
							n++
							lost = n%10 == 0
						}
					}
					return lost
				}}
		},
		Check: func(r, _ *Result) Verdict {
			v := r.Viewers[0]
			w := Window{5 * time.Second, 60 * time.Second}
			fresh, src := len(Fresh(v.Timeline, w)), SourceFrames(r, ctrl.Native, w)
			return Verdict{v.DeadComplete == 0 && float64(fresh) >= 0.9*float64(src), fmt.Sprintf("undecodable forwarded pictures %d fresh %d/%d", v.DeadComplete, fresh, src)}
		}}
}

func kbit(bps int) string {
	if bps%1_000_000 == 0 {
		return fmt.Sprintf("%dm", bps/1_000_000)
	}
	return fmt.Sprintf("%dk", bps/1000)
}

// A18 is authorised by C0 r3 F-2: readable native pixels on a path that
// cannot carry native T2. The return history includes a narrow low interval.
func native1500k(prejoin bool) Property {
	name := "native_1500k_prejoin"
	t0 := time.Second
	rates := []RatePoint{{0, 1_500_000}}
	if !prejoin {
		name = "native_1500k_return"
		t0 = 42 * time.Second
		rates = []RatePoint{{0, 1_500_000}, {21 * time.Second, 256_000}, {t0, 1_500_000}}
	}
	return Property{ID: "A18 " + name, Capacity: func(_ int, t time.Duration) int {
		if !prejoin && t >= 21*time.Second && t < t0 {
			return 256_000
		}
		return 1_500_000
	}, Build: func(bool) *Scenario {
		return &Scenario{Name: name, Duration: t0 + 51*time.Second, Native: Detailed, Audio: true,
			Links:   []LinkConfig{{Rate: rates, Delay: delay(40 * ms), Limit: 1000}},
			Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
	}, Check: func(r, _ *Result) Verdict {
		v := r.Viewers[0]
		first := FirstNative(v, Window{t0, t0 + 20*time.Second}, false)
		w := Window{t0 + 20*time.Second, t0 + 50*time.Second}
		share := NativeShare(v, w, false)
		fresh := len(Fresh(v.Timeline, w))
		minimum := .9 * float64(SourceFrames(r, ctrl.Native, w)) / 4
		gap := MaxGap(Fresh(v.Timeline, w), w)
		return Verdict{first >= 0 && share >= .9 && float64(fresh) >= minimum && gap <= time.Second,
			fmt.Sprintf("first native %v share %.3f fresh %d (≥%.1f) gap %v", first, share, fresh, minimum, gap)}
	}}
}

// A19 pc_replacement replaces the A1 follow-up's viewer_replace row (a
// scripted replacement 1.2 s after the restore, the browser's old
// reconnectGrace) with the C2 review's specification (GPT, §5
// "viewer_flap — browser timing AND low-key service"), quoted:
//
//	"Add Tier A A19 `pc_replacement`: healthy co-viewer plus an established
//	subject, independent links; replace the subject after a 2.5 s outage at
//	20 s, with replacement setup/signaling delay explicit and nonzero. Reuse
//	the room/source/key policy, hand off rate only with `NewViewerFrom`,
//	start a new feedback generation and empty decoder/continuity. Phase
//	replacement against the latest low key across seeds. Inject late old-PC
//	feedback. Require first of three post-restore advancing frames with
//	G2G≤1 s within ≤1 s of restore; post-recovery gap<1.5 s; no co-viewer
//	freezes/full-native loss; fixed memory bounds; no extra native keys
//	against control. Run 100 seeds, plus variants with repeated replacement
//	requests and a constrained inherited rate. Assert one low exception per
//	session interval and the shared room cap. This row models the
//	wire/controller portion; deterministic browser tests must independently
//	cover the restore-to-replace timing."
//
// How the row realises it:
//   - Source and outage mirror Tier B viewer_flap: the clock source with
//     audio; the subject's links (both directions) are out for 20–22.5 s; the
//     SFU's media barrier (Consent) stops forwarding 2 s after the subject's
//     last reverse packet. The co-viewer's links are never impaired.
//   - The browser replaces the PC once signaling returns (web/src/session.ts
//     early restore); seeds phase the replacement uniformly over the 2 s
//     after the restore. The old PC's own post-restore low key assembles
//     ≈0.3–0.6 s after it, so the replacement lands before, during and after
//     the latest low key. Setup is 30 ms (replace → joined measured 27–31 ms
//     in flap-diag-{1..4}).
//   - The old PC's late stall and TWCC report arrive 100 ms after it closed
//     (PCReplace, discarded by its generation as production's closed Peer
//     discards them).
//   - Control: the same seed without the replacement (the old PC recovers by
//     itself). Native keys are counted from the restore to the end.
//   - Fixed memory: every viewer's pacer stays within QueueBytes and
//     QueueFrames at every 10 ms sample (as A16).
//   - Variants: "_repeat" requests the replacement again 0.4 and 0.8 s later
//     and replaces a second, concurrently impaired session at the same time
//     (the room cap); the later replacements of a session have no credit
//     left and wait for the room's low budget. "_constrained" puts the
//     subject on 256 kbit/s, so the replacement inherits a constrained rate.
//     Both assert the exception properties (one per session per
//     KeyInterval, the room's one per KeyRetry), the co-viewer and memory;
//     "_constrained" also the subject's recovery. Neither compares native
//     keys with the control: a PC replaced after its predecessor already
//     returned to native must acquire native again, through the room's
//     shared budget (no exception), so each extra replacement can cost one
//     budgeted native key.
func pcReplacement(variant string) Property {
	o := []Interval{{20 * time.Second, 22500 * ms}}
	restore := o[0].To
	build := func(replace bool) *Scenario {
		subject := LinkConfig{Rate: rate(50_000_000), Delay: delay(20 * ms), Limit: 1000, Outages: o}
		if variant == "constrained" {
			subject.Rate = rate(256_000)
		}
		sc := &Scenario{Name: "pc_replacement", Duration: 40 * time.Second, Native: Clock, Audio: true, SampleQueue: true,
			Links: []LinkConfig{subject, {Delay: delay(20 * ms), Outages: o}, fifo(50_000_000, 20*ms), {Delay: delay(20 * ms)}}}
		if variant != "" {
			sc.Name += "_" + variant
		}
		rp := &PCReplace{At: restore, Spread: 2 * time.Second, Setup: 30 * ms}
		if !replace {
			rp = &PCReplace{At: time.Hour} // never: the recovery policy stays off, as in the replaced runs
		}
		sc.Viewers = []ViewerConfig{
			{Join: time.Second, Fwd: 0, Rev: 1, NominalRTT: 40 * ms, Consent: true, Replace: rp},
			{Join: time.Second, Fwd: 2, Rev: 3, NominalRTT: 40 * ms},
		}
		if variant == "repeat" {
			if replace {
				rp.Again = []time.Duration{400 * ms, 800 * ms}
			}
			second := *rp
			second.Again = nil
			sc.Links = append(sc.Links, subject, LinkConfig{Delay: delay(20 * ms), Outages: o})
			sc.Viewers = append(sc.Viewers, ViewerConfig{Join: time.Second, Fwd: 4, Rev: 5, NominalRTT: 40 * ms, Consent: true, Replace: &second})
		}
		return sc
	}
	id := "A19 pc_replacement"
	if variant != "" {
		id += "_" + variant
	}
	return Property{ID: id, Capacity: flat(50_000_000),
		Build: func(bool) *Scenario { return build(true) }, Control: func(bool) *Scenario { return build(false) },
		Check: func(r, c *Result) Verdict {
			end := r.Scenario.Duration
			for i, v := range r.Viewers {
				if v.MaxQueuedBytes > ctrl.QueueBytes || v.MaxQueuedFrames > ctrl.QueueFrames {
					return failf("viewer %d queue %d B / %d frames exceeds bounds", i, v.MaxQueuedBytes, v.MaxQueuedFrames)
				}
			}
			co, w := r.Viewers[1], Window{10 * time.Second, end}
			if f := len(Gaps(Frames(co.Timeline, w), w)); f > 0 {
				return failf("co-viewer freezes %d", f)
			}
			if s, cs := NativeShare(co, w, true), NativeShare(c.Viewers[1], w, true); s < cs {
				return failf("co-viewer full native %.3f < control %.3f", s, cs)
			}
			after := Window{restore, end}
			if k, ck := NativeKeys(r, after), NativeKeys(c, after); k > ck && variant == "" {
				return failf("native keys %d > control %d", k, ck)
			}
			sessions := map[int]time.Time{}
			var room time.Time
			for _, e := range r.Exceptions {
				if last, ok := sessions[e.Session]; ok && e.At.Sub(last) < ctrl.KeyInterval {
					return failf("session %d: two low exceptions %v apart", e.Session, e.At.Sub(last))
				}
				if !room.IsZero() && e.At.Sub(room) < ctrl.KeyRetry {
					return failf("room: two exception PLIs %v apart", e.At.Sub(room))
				}
				sessions[e.Session], room = e.At, e.At
			}
			detail := fmt.Sprintf("exceptions %d", len(r.Exceptions))
			if variant == "repeat" {
				return passf("%s", detail)
			}
			rec, post, ok := recovery(r.Viewers[0], restore, end)
			if !ok {
				return failf("no recovery; %s", detail)
			}
			return Verdict{rec <= time.Second && post < 1500*ms, fmt.Sprintf("recovery %v post gap %v %s", rec, post, detail)}
		}}
}
