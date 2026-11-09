package ctrl

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// wireCharge is one slot of the padding-accounting ring: the video and
// padding wire bytes sent within one PaddingWindow/len(charges) of local send
// time. Bucketing at insertion keeps the accounting a fixed-size ring however
// long the session or fast the sender (C2-1); the window is quantised to one
// slot.
type wireCharge struct {
	at             time.Time // latest charge in the slot
	slot           int64
	video, padding int
}

// Discovery owns both wire credit and unique feedback accounting. A13/F-1
// exercise lossy equal-spacing clusters; native_1500k exercises partial results.
type Discovery struct {
	MaxOverdrive  float64
	generation    uint64
	serial        int
	cluster       *Cluster
	target        int
	capped, dirty bool
	sent          map[uint64]Sent
	reported      map[uint64]bool
	arrived       map[uint64]time.Time // first reported arrival of each acked cluster packet
	next          time.Time
	backoff       time.Duration
	charges       [300]wireCharge
	origin        time.Time // slot zero of the charges ring
	head          int64     // latest slot charged or budgeted
	bootstrap     int
	ceiling       int // Estimate.Ceiling when the cluster started
}

func (p *Discovery) Observe(_ time.Time, e Estimate) {
	if p.cluster != nil && (e.Overuse || e.Congestive || e.Draining) {
		p.dirty = true
	}
}

// slot maps local time to its charges slot and advances the ring's head.
func (p *Discovery) slot(at time.Time) int64 {
	if p.origin.IsZero() {
		p.origin = at
	}
	n := int64(at.Sub(p.origin) / (PaddingWindow / time.Duration(len(p.charges))))
	p.head = max(p.head, n)
	return n
}
func (p *Discovery) budget(now time.Time) int {
	p.slot(now)
	v, pad := 0, 0
	for _, c := range p.charges {
		if c.slot > p.head-int64(len(p.charges)) {
			v += c.video
			pad += c.padding
		}
	}
	return max(0, BootstrapPadding-p.bootstrap+int(PaddingShare*float64(v))-pad)
}
func (p *Discovery) AllowPadding(now time.Time, bytes int) bool { return p.budget(now) >= bytes }
func (p *Discovery) OnSent(s Sent) {
	if s.Generation != p.generation {
		return
	}
	video, padding := 0, 0
	if s.Kind == KindPadding {
		used := min(s.Size, BootstrapPadding-p.bootstrap)
		p.bootstrap += used
		padding = s.Size - used
	} else if s.Kind == KindVideo {
		video = s.Size
	}
	if video+padding > 0 {
		// Charged on insertion by local send time; a slot already outside
		// the window is expired, an older occupant of the ring index is
		// overwritten.
		if n := p.slot(s.At); n > p.head-int64(len(p.charges)) {
			c := &p.charges[(n%int64(len(p.charges))+int64(len(p.charges)))%int64(len(p.charges))]
			if c.slot != n {
				*c = wireCharge{slot: n}
			}
			c.at = s.At
			c.video += video
			c.padding += padding
		}
	}
	if p.cluster != nil && s.Cluster == p.cluster.ID {
		p.sent[s.Seq] = s
	}
}
func (p *Discovery) OnFeedback(now time.Time, f Feedback) (ProbeResult, bool) {
	if p.cluster == nil {
		return ProbeResult{}, false
	}
	for _, a := range f.Arrivals {
		s, ok := p.sent[a.Seq]
		if !ok || s.Generation != p.generation {
			continue
		}
		p.reported[a.Seq] = true
		if _, dup := p.arrived[a.Seq]; a.At.IsZero() || dup {
			continue
		}
		p.arrived[a.Seq] = a.At
	}
	return p.finish(now, false)
}

// sustained is the send dispersion of pkts (in send order) with the burst
// credit they used excluded: the bytes after the first, less that credit,
// and the send span in seconds. The credit is the most the cumulative bytes
// after the first ran ahead of a pacer that starts empty at the first packet
// and sends at the planned rate: bytes the pacer could emit back-to-back from
// its starting allowance prove no rate, so they cannot shrink the span. The
// bytes therefore never exceed bps × span.
func sustained(pkts []Sent, bps float64) (float64, float64) {
	bytes, credit := 0.0, 0.0
	for _, s := range pkts[1:] {
		bytes += float64(s.Size)
		credit = max(credit, bytes-bps*s.At.Sub(pkts[0].At).Seconds()/8)
	}
	return bytes - credit, pkts[len(pkts)-1].At.Sub(pkts[0].At).Seconds()
}

// evaluate measures the finished cluster and classifies what its evidence
// may be used for (C2-2). D is unique delivered bytes over both the send and
// the receive span of the same acked packets: the receive-dispersion rate,
// bounded by the burst-excluded send rate (within one packet) and the planned
// rate, so a compressed or delayed delivery tail cannot credit more than was
// sustainably offered.
func (p *Discovery) evaluate(timeout bool) (ProbeClass, int) {
	c := p.cluster
	if timeout && len(p.reported) == 0 && len(p.sent) > 0 {
		return ProbeTimedOut, 0
	}
	bps := float64(c.Rate)
	all := make([]Sent, 0, len(p.sent))
	for _, s := range p.sent {
		all = append(all, s)
	}
	slices.SortFunc(all, func(a, b Sent) int { return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Seq, b.Seq)) })
	var acked []Sent
	total, largest, largestAcked := 0, 0, 0
	for _, s := range all {
		total += s.Size
		largest = max(largest, s.Size)
		if _, ok := p.arrived[s.Seq]; ok {
			acked = append(acked, s)
			largestAcked = max(largestAcked, s.Size)
		}
	}
	// A cluster that fits in the pacer's burst allowance at its rate could
	// have left as one back-to-back burst: it shows no sustained offer.
	burst := max(BurstFloor, int(bps*PacerBurst.Seconds()/8))
	rate, deficit := 0, 0.0
	if len(acked) >= ProbeMinPackets && total > burst {
		var first, last time.Time
		delivered, firstBytes := 0, 0
		for _, s := range acked {
			a := p.arrived[s.Seq]
			delivered += s.Size
			if first.IsZero() || a.Before(first) {
				first, firstBytes = a, s.Size
			}
			last = later(last, a)
		}
		delivered -= firstBytes
		sent, sendSpan := sustained(acked, bps)
		if span := last.Sub(first).Seconds(); sent > 0 && sendSpan > 0 && span > 0 {
			// Dispersion resolves one packet on either side: the sender may
			// have earned up to one more packet's tokens by its last
			// departure. The send-side bound allows that, so its granularity
			// does not bias D low, but never exceeds the planned rate.
			send := sent * 8 / sendSpan
			bound := min(bps, (sent+float64(largestAcked))*8/sendSpan)
			rate = int(min(bound, float64(delivered)*8/span))
			// What the sustained offer put in during the receive span
			// beyond what arrived in it.
			deficit = send*span/8 - float64(delivered)
		}
	}
	// Feedback lost on the reverse path leaves some packets unreported.
	// When the cluster's first and last packets were reported, the spans
	// are whole and D, measured over the acked packets only, credits no
	// unreported byte: it is a measurement of the reported subset, not a
	// failure of the experiment (C2 §5 A12/A13: at 20% reverse loss most
	// clusters missed some feedback and each counted as a failure).
	if len(p.reported) < len(p.sent) && (len(all) == 0 || !p.reported[all[0].Seq] || !p.reported[all[len(all)-1].Seq]) {
		return ProbeMissing, rate
	}
	if rate == 0 {
		return ProbeUndersized, 0
	}
	// The sender sustained the planned offer if, burst credit excluded, it
	// never fell more than one packet behind it over its send span. A cluster
	// the padding budget ended early at the full rate is a shorter complete
	// experiment, not a send-limited one.
	if sent, span := sustained(all, bps); bps*span/8-sent > float64(largest) {
		return ProbeSendLimited, rate
	}
	if deficit > float64(largestAcked) {
		return ProbeSaturated, rate
	}
	if p.dirty {
		return ProbeDirty, rate
	}
	return ProbeDelivered, rate
}
func (p *Discovery) finish(now time.Time, timeout bool) (ProbeResult, bool) {
	if p.cluster == nil || now.Before(p.cluster.Until.Add(OveruseTime)) || (!timeout && len(p.reported) < len(p.sent) && !p.spanned()) {
		return ProbeResult{}, false
	}
	class, rate := p.evaluate(timeout)
	complete := class == ProbeSaturated || class == ProbeDelivered
	r := ProbeResult{Cluster: p.cluster.ID, Class: class, Generation: p.generation, Target: p.target, Rate: rate, Clean: !p.dirty,
		Success: complete && !p.dirty && !p.capped && rate >= p.target, Capped: p.capped}
	// A ceiling-limited cluster that measured more than the ceiling it was
	// sized from shows the path improving: keep climbing without back-off
	// (A18 native_1500k_return, A5 b1_flap).
	progress := p.ceiling > 0 && r.Clean && float64(rate) >= ProbeHeadroom*float64(p.ceiling)
	// A measurement failure (too few acked packets, or feedback that never
	// covered the cluster) is not evidence about the path: it is retried
	// at the current back-off without doubling it, so a noisy short sample
	// does not stretch discovery (C2 §5 A12/A13).
	measurement := class == ProbeUndersized || class == ProbeMissing || class == ProbeTimedOut
	if r.Success || progress {
		p.backoff = 0
		p.next = now
	} else if measurement {
		p.next = now.Add(max(p.backoff, ProbeBackoffMin))
	} else {
		if p.backoff == 0 {
			p.backoff = ProbeBackoffMin
		} else {
			p.backoff = min(ProbeBackoffMax, 2*p.backoff)
		}
		p.next = now.Add(p.backoff)
	}
	p.cluster = nil
	return r, true
}

// spanned reports that the cluster's first and last packets were both
// reported: feedback has moved past the cluster, so a packet still
// unreported lost its feedback for good (TWCC reports are not repeated).
func (p *Discovery) spanned() bool {
	first, last, have := uint64(0), uint64(0), false
	for q := range p.sent {
		if !have || q < first {
			first = q
		}
		if !have || q > last {
			last = q
		}
		have = true
	}
	return have && p.reported[first] && p.reported[last]
}

func (p *Discovery) Tick(now time.Time) (ProbeResult, bool) {
	if p.cluster == nil {
		return ProbeResult{}, false
	}
	return p.finish(now, !now.Before(p.cluster.Until.Add(ProbeTimeout)))
}
func (p *Discovery) Maybe(now time.Time, target, media int, e Estimate, key bool) (Cluster, bool) {
	if key {
		return Cluster{}, false
	}
	return p.maybe(now, target, media, p.budget(now), e, false)
}

// An admitted native key funds a real experiment without consuming the
// bootstrap. Use only the burst plus the minimum measurable sample, rather
// than a full ProbeLength at the fast-key rate. Recent loss/congestion and
// a measured capacity ceiling still prohibit this experiment.
func (p *Discovery) MaybeKey(now time.Time, target, bytes int, e Estimate) (Cluster, bool) {
	if !keyProbeClean(now, e) || e.Ceiling > 0 && e.Ceiling < target {
		return Cluster{}, false
	}
	return p.maybe(now, target, 0, bytes, e, true)
}

func keyProbeClean(now time.Time, e Estimate) bool {
	return e.Loss == 0 && !e.Overuse && !e.Congestive && !e.Draining &&
		(e.LostAt.IsZero() || now.Sub(e.LostAt) >= ProbeEvidenceTTL) &&
		(e.CongestedAt.IsZero() || now.Sub(e.CongestedAt) >= ProbeEvidenceTTL)
}

func (p *Discovery) maybe(now time.Time, target, media, budget int, e Estimate, mediaOnly bool) (Cluster, bool) {
	// Probe when evidence or R is the binding limit: after a cut R regrows at
	// IncreasePerSecond, while one clean cluster restores it (A7 rtt_step).
	// No upward experiment into unresolved network debt (C2 §5 A5).
	busy := p.cluster != nil && !(mediaOnly && !p.cluster.MediaOnly && target > p.target)
	if busy || now.Before(p.next) && !(mediaOnly && p.backoff == 0) || target <= min(e.Rate, e.Proven) || e.Overuse || e.Congestive || e.Draining {
		return Cluster{}, false
	}
	if budget < ProbeMinPackets*BurstFloor {
		return Cluster{}, false
	}
	overdrive := p.MaxOverdrive
	if overdrive <= 0 {
		overdrive = defaultConfig().ProbeMaxOverdrive
	}
	// F-1 compensates the offer for non-congestive loss. ProbeHeadroom keeps
	// a clean path from measuring exactly target − ε by dispersion.
	loss := min(e.Loss, 1)
	factor := math.Inf(1)
	if loss < 1 {
		factor = ProbeHeadroom / (1 - loss)
	}
	rate := int(math.Ceil(float64(target) * min(factor, overdrive)))
	// Once a saturated probe measured the path, offering far above it only
	// queues the excess (≈0.6 s per probe on 256 kbit/s); clean failures
	// still ratchet D up through the cap (A6 viewer_256k, A15 pair_change).
	if e.Ceiling > 0 {
		// Current-generation media can already contradict an older
		// capacity bound (A16 return). Use at most the current paced
		// rate: a compressed delivery burst above R must not escalate an
		// experiment into a competing flow's queue (A10).
		rate = min(rate, int(overdrive*float64(max(e.Ceiling, min(e.Proven, e.Rate)))))
	}
	// After recent congestion the experiment is capped by the current
	// sustainable evidence (C2 §5 A10); a clean cold join is not.
	if !e.CongestedAt.IsZero() && now.Sub(e.CongestedAt) < AppLimitedLookback {
		rate = min(rate, int(overdrive*float64(max(e.Proven, e.Rate))))
	}
	if mediaOnly && budget <= max(BurstFloor, int(float64(rate)*PacerBurst.Seconds()/8))+ProbeMinPackets*BurstFloor {
		return Cluster{}, false
	}
	duration := ProbeLength
	if rate > media {
		pad := budget
		if e.Ceiling > 0 && !mediaOnly {
			// A ceiling-limited cluster only tests the bound: the smallest
			// measurable cluster keeps the ratchet affordable on low, where
			// the padding share is tiny (A18 native_1500k_return).
			pad = min(pad, ProbeMinPackets*BurstFloor)
			// Above ~5 Mbit/s the fixed ratchet fits entirely inside one
			// burst and cannot measure sustained delivery (A16). When
			// real credit affords it, include a minimum sample beyond
			// that burst. Low-rate ratchets retain their existing cost.
			burst := max(BurstFloor, int(float64(rate)*PacerBurst.Seconds()/8))
			if need := burst + ProbeMinPackets*BurstFloor; pad <= burst && budget >= need {
				pad = need
			}
		}
		duration = min(duration, time.Duration(float64(pad*8)/float64(rate-media)*float64(time.Second)))
	}
	if mediaOnly {
		sample := max(BurstFloor, int(float64(rate)*PacerBurst.Seconds()/8)) + ProbeMinPackets*BurstFloor
		duration = min(duration, time.Duration(float64(sample*8)/float64(rate)*float64(time.Second)))
	}
	p.serial++
	c := Cluster{ID: p.serial, Generation: p.generation, Rate: rate, Until: now.Add(duration), MediaOnly: mediaOnly}
	p.cluster = &c
	p.target, p.ceiling = target, e.Ceiling
	p.capped = factor > overdrive
	p.dirty = false
	p.sent = map[uint64]Sent{}
	p.reported = map[uint64]bool{}
	p.arrived = map[uint64]time.Time{}
	return c, true
}
func (p *Discovery) Ready(now time.Time) bool {
	return p.cluster == nil && !now.Before(p.next) && p.budget(now) >= ProbeMinPackets*BurstFloor
}
func (p *Discovery) OnGeneration(_ time.Time, g uint64) {
	*p = Discovery{generation: g, serial: p.serial, MaxOverdrive: p.MaxOverdrive}
}
func (p *Discovery) BootstrapUsed() int { return p.bootstrap }

// Retained counts the prober's retained accounting entries (C2-1).
func (p *Discovery) Retained() int {
	return len(p.charges) + len(p.sent) + len(p.reported) + len(p.arrived)
}
