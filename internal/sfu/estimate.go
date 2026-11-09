package sfu

import (
	"math"
	"time"
)

// Unknown paths start conservatively. A room with measured traffic seeds a new
// viewer at that demand; each viewer still earns further capacity independently.
const initialWireRate = 256_000

// wireController bounds total RTP, including repair. Before the first real
// delay/RTT signal it probes multiplicatively; thereafter probes are smaller.
// A source keyframe is application backlog, not evidence of a network queue.
type wireController struct {
	rate, proof, receivedRate, sentRate int
	proofAt, cutAt, queueSince, probeAt time.Time
	trend                               delayTrend
	queueMinimum                        pathMinimum
	queueExcess                         time.Duration
	rttReports                          uint64
	limited                             bool
}

func repairReserve(wire, repair uint64) float64 {
	if wire == 0 {
		return .1
	}
	// One parity packet per media packet already uses half the wire budget.
	// Replaying a burst needs additional room; cap combined repair at 70%.
	return min(.7, max(.1, float64(repair)/float64(wire)+.05))
}
func admittedLayer(rates [3]int, budget int, reserve float64) uint8 {
	total := 0
	layer := uint8(0)
	for i, r := range rates {
		total += r
		if nativeWireRequired(total, reserve) <= int64(budget) {
			layer = uint8(i)
		} else {
			break
		}
	}
	return layer
}

// Base-first refreshes and a sustained receive window establish native demand
// without requiring a share larger than a full stream needs on a fair queue.
func nativeWireRequired(demand int, reserve float64) int64 {
	return int64(math.Ceil(float64(demand) * 110 / 100 / (1 - reserve)))
}

// The first measurement is a rate, not a fraction of a zero-rate history.
func smoothMediaRate(previous, current int64) int64 {
	if previous == 0 {
		return current
	}
	return (previous*3 + current*2) / 5
}

func selectedTemporalFPS(source int64, layer uint8) int64 {
	return max(1000, source>>uint(2-min(layer, 2)))
}

// Delay is regressed against wall time, so feedback cadence cannot change the
// queue-growth threshold. One second smooths Wi-Fi jitter without hiding a ramp.
type delayPoint struct {
	at time.Time
	ms float64
}
type delayTrend struct {
	samples [32]delayPoint
	n       int
}

const delaySlopeLimit = 25.0

// Reserve most of the one-second freshness envelope for capture and playout.
// At this forward residence, cap offered load to an actual decoded reservation.
// Cutting an unused reservation cannot drain a competing flow's FIFO.
const standingDelayLimit = 300 * time.Millisecond

// One whole frame at either edge of a 500ms heartbeat window is 2fps.
// Compare cadence with that quantization allowance, rather than a percentage
// whose meaning changes with capture frame rate.
const cadenceQuantization = 2000

func fullCadence(fps, source int64) bool {
	// At low capture rates, quantization must not label a half-rate temporal
	// layer as the complete source.
	return fps > source/2 && fps+cadenceQuantization >= source
}

func (t *delayTrend) observe(now time.Time, excess time.Duration) {
	n := 0
	for _, s := range t.samples[:t.n] {
		if now.Sub(s.at) <= time.Second {
			t.samples[n] = s
			n++
		}
	}
	if n == len(t.samples) {
		copy(t.samples[:], t.samples[1:])
		n--
	}
	t.samples[n] = delayPoint{now, float64(excess) / float64(time.Millisecond)}
	t.n = n + 1
}
func (t *delayTrend) slope() float64 {
	// Use most of the one-second regression window before treating its slope
	// as persistent. A single feedback/propagation step must not masquerade
	// as a ramp while its old sample remains in the window.
	if t.n < 3 || t.samples[t.n-1].at.Sub(t.samples[0].at) < 750*time.Millisecond {
		return 0
	}
	var x, y, xx, xy float64
	for _, s := range t.samples[:t.n] {
		a := s.at.Sub(t.samples[0].at).Seconds()
		x += a
		y += s.ms
		xx += a * a
		xy += a * s.ms
	}
	n := float64(t.n)
	d := n*xx - x*x
	if d <= 0 {
		return 0
	}
	return (n*xy - x*y) / d
}

type rateObservation struct {
	reports                                           uint64
	delayAt                                           time.Time
	delay                                             time.Duration
	excess                                            time.Duration
	loss, residual                                    float64
	received, demand, required, mediaDemand, sentRate int
	productive, full, stalled, queueLoss, trial       bool
	queue                                             time.Duration
}

func (c *wireController) step(now time.Time, s rateObservation) {
	if c.rate == 0 {
		c.rate = initialWireRate
	}
	newReport := s.reports > 0 && s.reports != c.rttReports
	if newReport {
		c.rttReports = s.reports
		// Repeating a stale receiver report at the TWCC cadence turns one
		// propagation step into a fictitious sustained queue ramp. Regress
		// only the delay clock and timestamp belonging to this new feedback.
		if s.delayAt.IsZero() {
			c.trend.observe(now, s.excess)
		} else {
			c.trend.observe(s.delayAt, s.delay)
		}
	}
	growing := c.trend.slope() > delaySlopeLimit
	excess := s.excess
	if !s.delayAt.IsZero() {
		if newReport {
			c.queueExcess = max(0, s.delay-c.queueMinimum.observe(now, s.delay))
		}
		// Standing excess and slope describe the same forward queue. RTT
		// also includes reverse traffic and feedback scheduling, which this
		// sender cannot drain by cutting its video rate.
		excess = c.queueExcess
	}
	// Delivered wire under repaired random loss understates send capacity.
	// Normalize only with no queue signal and enough receipts for the media;
	// loss on an AQM queue or a delivery collapse cannot earn this floor.
	received := s.received
	if !s.queueLoss && !growing && excess < rttConfirm && received >= s.mediaDemand && s.loss > 0 {
		received = int(float64(received) / (1 - min(.75, s.loss)))
	}
	if s.received > 0 {
		if c.receivedRate == 0 {
			c.receivedRate = s.received
		} else {
			c.receivedRate = (c.receivedRate*9 + s.received) / 10
		}
	}
	if c.sentRate == 0 {
		c.sentRate = s.sentRate
	} else {
		c.sentRate = (c.sentRate*9 + s.sentRate) / 10
	}
	// Compare like one-second windows: a sparse frame or delayed feedback
	// batch must not classify an unused ceiling as offered load. The 2%
	// tolerance covers packet quantization; a 25ms/s forward ramp implies
	// a larger delivery deficit and remains actionable.
	deliveredAll := c.sentRate > 0 && c.receivedRate >= c.sentRate*98/100
	// Leave the same 10% margin as media admission. An 80% cutoff would
	// classify a source using its funded reservation as congestion-limited.
	applicationLimited := c.sentRate > 0 && c.sentRate < c.rate*90/100 && s.queue < 20*time.Millisecond && excess < keyEnvelope
	queued := excess >= standingDelayLimit || excess >= rttConfirm && growing && !(applicationLimited && deliveredAll)
	// Proof records actual receipts, not a prediction that delay will stay
	// clear. A rising competing queue cannot erase delivered bytes; this
	// tick's congestion decision still clips old proof to current delivery.
	if received > 0 && !s.queueLoss {
		c.proof = max(c.proof, received)
		c.proofAt = now
	}
	if !c.proofAt.IsZero() && now.Sub(c.proofAt) > 10*time.Second {
		c.proof = 0
	}
	floor := 0
	if s.mediaDemand > 0 && (s.productive || s.trial) && !s.queueLoss && (!growing || s.full || s.trial && received >= s.mediaDemand) && (excess < standingDelayLimit || s.full && c.receivedRate >= s.mediaDemand) && c.proof >= s.mediaDemand {
		floor = s.required
	}
	lossy := s.loss > .05 && (s.queueLoss || (s.residual > .02 && !s.productive || s.stalled) && received < s.mediaDemand)
	if queued && !lossy && floor >= c.rate {
		// A held floor did not cut offered load. Do not carry its old trend
		// confirmation into a later standing-cap crossing: that cap needs
		// its own sustained evidence, even if the decoder is still advancing.
		c.queueSince = time.Time{}
		c.limited = true
		return
	}
	if queued || lossy {
		if c.queueSince.IsZero() {
			c.queueSince = now
		}
	} else {
		c.queueSince = time.Time{}
	}
	if newReport && (queued || lossy) && now.Sub(c.queueSince) >= 300*time.Millisecond && now.Sub(c.cutAt) >= 500*time.Millisecond {
		next := c.rate * 85 / 100
		if s.received > 0 {
			next = min(next, s.received*9/10)
		}
		// Resolve the one decoded/delivered reservation inside the cut. At
		// standing excess it requires both current full cadence and delivery:
		// a decoder claim alone cannot keep spending old wide-path proof.
		next = max(next, min(c.rate, floor))
		next = max(128000, next)
		if next >= c.rate {
			c.limited = true
			return
		}
		c.rate = next
		// Cutting offered load does not rewrite what the receiver delivered.
		// Revoke old wide-path proof to this current receipt window, rather
		// than to the numerical cut (which otherwise compounds itself).
		// A decoder sustaining the complete source proves its admitted
		// reservation despite variable-size pictures crossing a feedback
		// window. Keep that one floor inside the cut, not as a later undo.
		proof := s.received
		if s.full {
			proof = max(proof, min(c.rate, floor))
		}
		c.proof = min(c.proof, proof)
		c.cutAt = now
		c.limited = true
		// Only actual wire trials own exponential backoff. Numerical cuts
		// cannot repeatedly postpone the trial scheduler's next experiment.
		c.probeAt = now.Add(minBackoff)
		return
	}
	if !lossy {
		c.rate = max(c.rate, floor)
	}
	if now.Before(c.probeAt) || s.stalled {
		return
	}
	ceiling := max(initialWireRate, s.demand)
	if c.rate < ceiling {
		c.rate = min(ceiling, c.rate*125/100)
	}
	c.probeAt = now.Add(100 * time.Millisecond)
}
