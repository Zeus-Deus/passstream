package ctrl

import (
	"math"
	"time"
)

type ratePoint struct {
	at   time.Time
	rate int
}
type delayPoint struct {
	at    time.Time
	value time.Duration
}
type ackPoint struct {
	at    time.Time // receiver arrival
	bytes int64     // cumulative acked bytes
	sent  time.Time // local send time of the newest packet in the point
	probe bool      // a probe-cluster packet arrived at this instant
}
type sentRecord struct {
	Sent
	state uint8
}

// Bandwidth uses transport evidence only. Its history and generation fence
// serve A7 rtt_step, A10 FIFO, A13 loss_prejoin and A15 pair_change.
type Bandwidth struct {
	Config                                          Config
	rate                                            int
	generation                                      uint64
	sent                                            [8192]sentRecord
	ack                                             []ackPoint
	bytes                                           int64
	rates                                           []ratePoint
	minima, trend                                   []delayPoint
	last, hold, overSince, overAt, lossCut, probeAt time.Time
	probeRate, acked                                int
	slope                                           float64
	over                                            bool
	excess                                          time.Duration
	rtt                                             time.Duration
	loss                                            float64
	lossRun                                         float64
	lossSamples                                     []lossPoint
	clusterAt                                       time.Time // last probe-cluster packet sent
	ceiling                                         int       // wire capacity a saturated probe measured; 0 = none
	ceilingAt                                       time.Time
	// clockSend/clockArrival: local send and receiver arrival time of the
	// newest packet whose timing was accepted (receiver-clock plausibility).
	clockSend, clockArrival time.Time
	// Congestion/drain episode (C2 §5 A5): the network holds debt this
	// sender created. lastSent is the newest local send time; episodeSent
	// the newest send time when the episode began, so a later packet is new
	// work queued behind the debt; cleared when new work arrived; settled
	// when, after that, the excess transit fell below OveruseTime. A queue
	// that stands (a competitor's) settles within PathWindow, once the path
	// minimum re-bases onto it, so probing resumes (C2 §5 A10: keep probing
	// eventually after debt clears).
	lastSent, episodeSent time.Time
	episode               bool
	cleared, settled      time.Time
	// inherited: the episode came with a replacement PC's handoff. This
	// generation has no baseline from before the debt, so it ends once a
	// valid delay trend is neither falling (debt still draining) nor
	// rising.
	inherited     bool
	handedCeiling int       // the previous PC's live ceiling, installed at the first update
	congestedAt   time.Time // latest congestion cut or drain episode
	service       int       // bps: the bottleneck's service rate for this sender during the episode
	lostAt        time.Time // latest media packet reported lost
	keyRate       int       // fast-key evidence Clamp kept for key delivery
	keyAt         time.Time // when that evidence was measured
	inflight      int       // bytes sent in this generation and not yet reported
}
type lossPoint struct {
	at   time.Time
	lost bool
}

func (b *Bandwidth) OnSent(s Sent) {
	r := &b.sent[s.Seq%uint64(len(b.sent))]
	if r.state == 0 && !r.At.IsZero() && r.Generation == b.generation {
		b.inflight -= r.Size // never reported before the ring wrapped
	}
	*r = sentRecord{Sent: s}
	if s.Generation == b.generation {
		b.inflight += s.Size
	}
	b.lastSent = later(b.lastSent, s.At)
	if s.Cluster != 0 {
		b.clusterAt = s.At
	}
}

// capped drops the oldest entries beyond n. Every history is hard-capped at
// the send ring's size, independently of the receiver's clock: evidence about
// more packets than the ring remembers cannot be matched anyway (C2-1).
func capped[T any](s []T, n int) []T {
	if len(s) > n {
		return append(s[:0], s[len(s)-n:]...)
	}
	return s
}

func (b *Bandwidth) OnFeedback(now time.Time, f Feedback) {
	kept := b.minima[:0]
	least, haveLeast := time.Duration(0), false
	for _, p := range b.minima {
		if now.Sub(p.at) < PathWindow {
			kept = append(kept, p)
			if !haveLeast || p.value < least {
				least, haveLeast = p.value, true
			}
		}
	}
	b.minima = kept
	// The receiver controls its clock, so its arrival times are evidence only
	// while they are physically plausible: not more than PathWindow behind
	// the latest accepted arrival, a transit within PathWindow of the path
	// minimum (outside every window the estimator uses), and a clock that
	// advances across reports while this sender keeps sending. Implausible
	// timing is discarded; the packets still count as delivered for loss.
	plausible := func(s *sentRecord, a Arrival) bool {
		if !b.clockArrival.IsZero() && a.At.Before(b.clockArrival.Add(-PathWindow)) {
			return false
		}
		return !haveLeast || (a.At.Sub(s.At)-least).Abs() <= PathWindow
	}
	var newestSend, newestArrival time.Time
	for _, a := range f.Arrivals {
		s := &b.sent[a.Seq%uint64(len(b.sent))]
		if s.At.IsZero() || s.Seq != a.Seq || s.Generation != b.generation || s.state == 2 || a.At.IsZero() || !plausible(s, a) {
			continue
		}
		if s.At.After(newestSend) {
			newestSend = s.At
		}
		if a.At.After(newestArrival) {
			newestArrival = a.At
		}
	}
	timing := b.clockArrival.IsZero() || !newestSend.After(b.clockSend) || newestArrival.After(b.clockArrival)
	if timing && !newestSend.IsZero() {
		b.clockSend = later(b.clockSend, newestSend)
		b.clockArrival = later(b.clockArrival, newestArrival)
	}
	var transit time.Duration
	have := false
	for _, a := range f.Arrivals {
		s := &b.sent[a.Seq%uint64(len(b.sent))]
		if s.At.IsZero() || s.Seq != a.Seq || s.Generation != b.generation || s.state == 2 {
			continue
		}
		if s.state == 0 {
			b.inflight -= s.Size
		}
		if a.At.IsZero() {
			if s.state == 0 && s.Kind != KindPadding {
				b.lossSamples = append(b.lossSamples, lossPoint{now, true})
				b.lostAt = now
			}
			s.state = 1
			continue
		}
		if s.state == 0 && s.Kind != KindPadding {
			b.lossSamples = append(b.lossSamples, lossPoint{now, false})
		}
		s.state = 2
		if !timing || !plausible(s, a) {
			continue
		}
		b.bytes += int64(s.Size)
		if n := len(b.ack); n > 0 && b.ack[n-1].at.Equal(a.At) {
			b.ack[n-1].bytes = b.bytes // same receiver instant: one point
			b.ack[n-1].sent = later(b.ack[n-1].sent, s.At)
			b.ack[n-1].probe = b.ack[n-1].probe || s.Cluster != 0
		} else {
			b.ack = append(b.ack, ackPoint{a.At, b.bytes, s.At, s.Cluster != 0})
		}
		t := a.At.Sub(s.At)
		// New work (sent after the episode began) arrived: the old
		// outstanding work queued ahead of it has been served.
		if b.episode && !b.inherited && s.At.After(b.episodeSent) {
			b.episode, b.cleared, b.settled = false, now, time.Time{}
		}
		if !have || t < transit {
			transit = t
			have = true
		}
	}
	b.lossSamples = capped(b.lossSamples, len(b.sent))
	b.ack = capped(b.ack, len(b.sent))
	if len(b.ack) > 0 {
		latest := b.ack[len(b.ack)-1].at
		i := 0
		for i+1 < len(b.ack) && latest.Sub(b.ack[i+1].at) >= AckedWindow {
			i++
		}
		if span := latest.Sub(b.ack[i].at); span >= AckedWindow {
			b.acked = int(float64(b.bytes-b.ack[i].bytes) * 8 / span.Seconds())
			b.rates = capped(append(b.rates, ratePoint{now, b.acked}), len(b.sent))
		}
		b.ack = append(b.ack[:0], b.ack[i:]...)
	}
	if have {
		if !haveLeast || transit < least {
			least = transit
		}
		b.minima = capped(append(b.minima, delayPoint{now, transit}), len(b.sent))
		b.excess = transit - least
		kept = b.trend[:0]
		for _, p := range b.trend {
			if now.Sub(p.at) <= TrendWindow {
				kept = append(kept, p)
			}
		}
		b.trend = capped(append(kept, delayPoint{now, b.excess}), len(b.sent))
		b.slope = 0
		if len(b.trend) >= 3 && now.Sub(b.trend[0].at) > TrendWindow/2 {
			var x, y, xx, xy float64
			for _, p := range b.trend {
				a := p.at.Sub(b.trend[0].at).Seconds()
				v := p.value.Seconds() * 1000
				x += a
				y += v
				xx += a * a
				xy += a * v
			}
			n := float64(len(b.trend))
			if d := n*xx - x*x; d > 0 {
				b.slope = (n*xy - x*y) / d
				if b.inherited && b.episode && math.Abs(b.slope) <= OveruseSlope {
					b.episode, b.inherited, b.cleared, b.settled = false, false, now, now
				}
			}
		}
		if b.slope > OveruseSlope {
			if b.overSince.IsZero() {
				b.overSince = now
			}
			b.over = now.Sub(b.overSince) >= OveruseTime
		} else {
			b.overSince = time.Time{}
			b.over = false
		}
		if b.over {
			b.overAt = now
		}
		if !b.episode && !b.cleared.IsZero() && b.settled.IsZero() && b.excess < OveruseTime {
			b.settled = now
		}
		b.drain(now)
	}
	b.update(now)
}

// drain starts a congestion/drain episode (C2 §5 A5) when the bottleneck
// has stood busy for OveruseTime (excess transit ≥ OveruseTime, so arrivals
// over the newest OveruseTime measure its service rate S for this sender)
// and S fell sharply below the rate the same packets were sent at: the
// network is holding debt this sender is still adding to. A flat standing
// queue is not clean merely because the delay slope returned to zero, and a
// delay-slope detector over TrendWindow saw the 5 Mbit/s → 256 kbit/s
// cliff of b1_flap only after ≈0.25 s and cut R in four steps over another
// 0.3 s, by which time ≈3 s of debt was queued.
//
// On entry: R = Backoff × S at once; every stale delivery and probe proof
// is cancelled (the acked history restarts at the latest arrival); and S,
// measured over a busy bottleneck, is saturation evidence: it bounds R and
// later probe offers as a saturated probe's ceiling does.
func (b *Bandwidth) drain(now time.Time) {
	n := len(b.ack)
	if n < 2 || !b.episode && b.excess < OveruseTime {
		return
	}
	latest := b.ack[n-1]
	probe := latest.probe
	i := n - 2
	for i > 0 && latest.at.Sub(b.ack[i].at) < OveruseTime {
		probe = probe || b.ack[i].probe
		i--
	}
	span, sendSpan := latest.at.Sub(b.ack[i].at), latest.sent.Sub(b.ack[i].sent)
	if span < OveruseTime {
		return
	}
	bits := float64(latest.bytes-b.ack[i].bytes) * 8
	service := bits / span.Seconds()
	if b.episode {
		// While the debt drains the bottleneck stays busy: arrivals over
		// the newest OveruseTime (at most AckedWindow since the episode
		// began) keep measuring its service rate.
		b.service = int(service)
		return
	}
	// A probe overshoots by design and is judged by the prober. Otherwise
	// the reduction is sharp when the arrivals fell below what the
	// ordinary decrease would set, Backoff × the acked rate, and below
	// the rate the same packets were sent at: a delay-triggered cut then
	// leaves R above the service and the queue keeps growing. It is an
	// episode only once this sender's unreported work takes longer than
	// the freshness bound to drain at S: smaller standing queues (a picture
	// serialising on a slow link, a loss-based competitor's buffer) are the
	// ordinary decrease's business (A10 FIFO, A11 FQ-CoDel).
	if probe || sendSpan <= 0 || service >= Backoff*float64(b.acked) || service >= Backoff*bits/sendSpan.Seconds() ||
		float64(b.inflight*8)/service < FrameDeadline.Seconds() {
		return
	}
	b.service = int(service)
	b.episode, b.episodeSent, b.cleared = true, b.lastSent, time.Time{}
	b.rate = max(RateMin, min(b.rate, int(Backoff*service)))
	b.hold = now.Add(AckedWindow)
	if b.rtt > 0 {
		b.hold = now.Add(min(b.rtt, AckedWindow))
	}
	b.cut(now)
	b.ack = append(b.ack[:0], latest)
	b.acked = 0
	b.ceiling, b.ceilingAt = int(service/(1-min(b.loss, 0.5))), now
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (b *Bandwidth) OnReport(_ time.Time, r ReceiverReport) {
	if r.RTT > 0 {
		b.rtt = r.RTT
	}
}
func (b *Bandwidth) OnProbe(now time.Time, r ProbeResult) {
	if r.Generation != b.generation || r.Rate <= 0 {
		return
	}
	// Only a saturated, complete cluster (the receiver fell measurably
	// behind a sustained offer) measured the bottleneck: D is its unique
	// delivery, D/(1−L̂) its wire capacity. That bounds R and later offers
	// even when the probe's own overshoot raised delay; growing past it only
	// bursts pictures into the bottleneck queue and trips overuse (A18
	// native_1500k, A15 pair_change). It is not admission evidence unless
	// clean. A send-limited, undersized, missing or timed-out cluster, or one
	// whose delivery kept pace, is no upper bound (C2-2); one that delivered
	// above a live bound only raises it (the ceiling ratchet).
	// A success below the bound (a cheaper rung) does not contradict it.
	live := b.ceiling > 0 && now.Sub(b.ceilingAt) < ProbeEvidenceTTL
	wire := int(float64(r.Rate) / (1 - min(b.loss, 0.5)))
	switch {
	case r.Success && r.Rate < b.ceiling && live:
	case r.Success || r.Capped && r.Class != ProbeSaturated:
		b.ceiling = 0
	case r.Class == ProbeSaturated || live && wire > b.ceiling:
		b.ceiling, b.ceilingAt = wire, now
	}
	if !r.Clean {
		return
	}
	// A complete experiment replaces the probe evidence; an incomplete one
	// (send-limited, or measured on part of its feedback) only adds to it:
	// a short noisy sample must not lower R (C2 §5 A12/A13).
	if r.Class != ProbeSaturated && r.Class != ProbeDelivered && !b.probeAt.IsZero() && now.Sub(b.probeAt) < ProbeEvidenceTTL && r.Rate < b.probeRate {
		return
	}
	b.probeAt, b.probeRate = now, r.Rate
	b.rate = max(b.rate, r.Rate)
}
func (b *Bandwidth) Clamp(rate int) {
	if b.probeRate > rate && !b.probeAt.IsZero() {
		b.keyRate, b.keyAt = b.probeRate, b.probeAt
	}
	b.rate, b.probeRate = min(b.rate, rate), min(b.probeRate, rate)
	for i := range b.rates {
		b.rates[i].rate = min(b.rates[i].rate, rate)
	}
}
func (b *Bandwidth) OnGeneration(_ time.Time, g uint64) {
	*b = Bandwidth{Config: b.Config, rate: b.rate, generation: g, rtt: b.rtt}
}
func (b *Bandwidth) update(now time.Time) {
	if b.rate == 0 {
		b.rate = RateInit
	}
	if b.handedCeiling > 0 || b.inherited {
		// A handed-off ceiling, and a congested handoff's rate (the most
		// the previous PC found sustainable), bound R and probe offers.
		c := b.handedCeiling
		if b.inherited {
			c = max(c, b.rate)
			if b.handedCeiling > 0 {
				c = min(b.handedCeiling, b.rate)
			}
		}
		b.ceiling, b.ceilingAt, b.handedCeiling = c, now, 0
	}
	samples := b.lossSamples[:0]
	lost, runs, run := 0, 0, 0
	for _, p := range b.lossSamples {
		if now.Sub(p.at) < LossWindow {
			samples = append(samples, p)
			if p.lost {
				lost++
				run++
			} else if run > 0 {
				runs++
				run = 0
			}
		}
	}
	b.lossSamples = samples
	if run > 0 {
		runs++
	}
	b.loss, b.lossRun = 0, 0
	if len(samples) > 0 {
		b.loss = float64(lost) / float64(len(samples))
		if runs > 0 {
			b.lossRun = float64(lost) / float64(runs)
		}
	}
	congested := b.loss > CongestiveLoss && ((!b.overAt.IsZero() && now.Sub(b.overAt) < TrendWindow) || b.excess >= OveruseTime)
	// A probe's own overshoot is attributed to the probe (it is not clean
	// evidence) and does not cut R (A18 native_1500k). A cluster of at most
	// ProbeLength at ≤ the overdrive cap cannot queue ProbeLength of excess
	// delay; more is real congestion and is cut at once, even mid-probe
	// (A6 viewer_256k: capacity collapsing under a probe).
	probing := !b.clusterAt.IsZero() && now.Before(b.clusterAt.Add(ProbeTimeout)) && b.excess < ProbeLength
	if (b.over && !probing && !now.Before(b.hold)) || (congested && (b.lossCut.IsZero() || now.Sub(b.lossCut) >= LossWindow)) {
		if b.acked > 0 {
			b.rate = max(RateMin, min(b.rate, int(Backoff*float64(b.acked))))
		}
		// One RTT, but never longer than the acked window: a queue this
		// sender is overfilling inflates RTT, and each later cut would wait
		// longer while R stays above capacity (A6 viewer_256k, A5 b1_flap).
		hold := AckedWindow
		if b.rtt > 0 {
			hold = min(b.rtt, AckedWindow)
		}
		b.hold = now.Add(hold)
		if congested {
			b.lossCut = now
		}
		b.congestedAt = now
	} else if !b.last.IsZero() && !now.Before(b.hold) && !b.over && b.slope >= -OveruseSlope && !b.episode {
		// No growth while the network drains this sender's debt.
		b.rate = int(math.Ceil(float64(b.rate) * math.Pow(IncreasePerSecond, now.Sub(b.last).Seconds())))
	}
	b.last = now
}

// cut records a drain episode's cut (C2 §5 A5): delivery and probe
// evidence measured before it no longer describe the path.
func (b *Bandwidth) cut(now time.Time) {
	b.congestedAt = now
	b.rates, b.probeAt, b.probeRate = b.rates[:0], time.Time{}, 0
}

func (b *Bandwidth) Estimate(now time.Time) Estimate {
	b.update(now)
	// Instantaneous service (the best acked window) is Proven, which bounds
	// R's app-limited cap and temporal upgrades. Sustainable evidence for a
	// native key is conservative after recent congestion: the least acked
	// window over the lookback, so a compressed burst through a
	// competitor's queue trough does not prove a two-second native key
	// (C2 §5 A10). A clean path keeps the best window: a cold join need not
	// wait.
	recent := !b.congestedAt.IsZero() && now.Sub(b.congestedAt) < AppLimitedLookback
	best, least := 0, 0
	keep := b.rates[:0]
	for _, p := range b.rates {
		if now.Sub(p.at) < AppLimitedLookback {
			keep = append(keep, p)
			best = max(best, p.rate)
			if least == 0 || p.rate < least {
				least = p.rate
			}
		}
	}
	b.rates = keep
	proven, sustained := best, best
	if recent {
		sustained = least
	}
	cap := max(RateInit, int(AppLimitedFactor*float64(best)))
	if !b.probeAt.IsZero() && now.Sub(b.probeAt) < ProbeEvidenceTTL {
		proven = max(proven, b.probeRate)
		cap = max(cap, b.probeRate)
		if !recent || b.probeAt.After(b.congestedAt) {
			sustained = max(sustained, b.probeRate)
		}
	}
	ceiling := 0
	if b.ceiling > 0 && now.Sub(b.ceilingAt) < ProbeEvidenceTTL {
		ceiling = b.ceiling
		cap = min(cap, max(ceiling, proven))
	}
	maximum := b.Config.MaxRate
	if maximum <= 0 {
		maximum = defaultConfig().MaxRate
	}
	b.rate = max(RateMin, min(b.rate, cap, maximum))
	// The episode lasts until new work arrived (the old outstanding work
	// was served); then the excess transit must fall below OveruseTime and
	// stay one AckedWindow (a feedback window of post-drain delivery): no
	// upward experiment or upgrade probes into unresolved debt.
	draining := b.episode || !b.cleared.IsZero() && (b.settled.IsZero() || now.Sub(b.settled) < AckedWindow)
	e := Estimate{Ceiling: ceiling, Rate: b.rate, Proven: proven, Overuse: b.over, Congestive: !b.lossCut.IsZero() && now.Sub(b.lossCut) < LossWindow, Loss: b.loss, LossRun: b.lossRun, RTT: b.rtt, Draining: draining, CongestedAt: b.congestedAt, Sustained: sustained, LostAt: b.lostAt}
	// Fast-key evidence serves key delivery while it is current: within
	// ProbeEvidenceTTL, below any ceiling measured since, and with no
	// congestion after it was measured.
	if b.keyRate > 0 && now.Sub(b.keyAt) < ProbeEvidenceTTL && b.congestedAt.Before(b.keyAt) && (ceiling == 0 || b.ceilingAt.Before(b.keyAt)) {
		e.KeyRate = b.keyRate
	}
	if b.episode && b.service > 0 {
		e.Service, e.Debt = b.service, max(0, b.inflight)
	}
	return e
}

// Retained counts the estimator's history entries (per-viewer memory
// accounting, C2-1).
func (b *Bandwidth) Retained() int {
	return len(b.ack) + len(b.rates) + len(b.minima) + len(b.trend) + len(b.lossSamples)
}
