package sfu

import (
	"github.com/pion/rtcp"
	"sync/atomic"
	"time"
)

type departure struct {
	pathEpoch atomic.Uint64
	kind      atomic.Uint32
	trial     atomic.Int64
	sequence  atomic.Uint32
	media     atomic.Uint32
	at        atomic.Int64
	bytes     atomic.Int64
	total     atomic.Int64
	delivered atomic.Int64
}

type deliverySample struct {
	at    time.Duration
	bytes int64
}

// Feedback timestamps measure arrival at the receiver, including idle periods
// between batches. A short packet train is not a sustained share of a FIFO.
type deliveryWindow struct {
	samples     [256]deliverySample
	head, count int
	raw, at     time.Duration
	bytes       int64
}

func (w *deliveryWindow) observe(raw time.Duration, bytes int64) int64 {
	if w.count > 0 {
		const period = (1 << 24) * 64 * time.Millisecond
		delta := (raw - w.raw + period) % period
		if delta <= 0 {
			return 0
		}
		if delta > time.Minute {
			*w = deliveryWindow{}
		} else {
			w.at += delta
		}
	}
	w.raw = raw
	w.bytes += bytes
	w.samples[w.head] = deliverySample{w.at, w.bytes}
	w.head = (w.head + 1) % len(w.samples)
	w.count = min(w.count+1, len(w.samples))
	return w.rate(200 * time.Millisecond)
}

func (w *deliveryWindow) rate(window time.Duration) int64 {
	for i := 1; i < w.count; i++ {
		previous := w.samples[(w.head-1-i+len(w.samples))%len(w.samples)]
		if elapsed := w.at - previous.at; elapsed >= window {
			return int64(float64(w.bytes-previous.bytes) * 8 / elapsed.Seconds())
		}
	}
	return 0
}

func (p *Pacer) capacityRate(now time.Time) int64 {
	rate := int64(0)
	if at := p.sustainedRateAt.Load(); at > 0 && now.Sub(time.Unix(0, at)) < time.Second {
		rate = p.sustainedRate.Load()
	}
	at := p.probeRateAt.Load()
	if at > 0 && now.Sub(time.Unix(0, at)) < 10*time.Second {
		proven := p.probeRate.Load()
		if at == p.probeRateAt.Load() {
			rate = max(rate, proven)
		}
	}
	return rate
}

func (p *Pacer) recordProbeRate(rate int64, now time.Time) {
	until := p.probeEnd.Load()
	if until <= 0 || now.UnixNano() > until+int64(500*time.Millisecond) {
		return
	}
	if now.Sub(time.Unix(0, p.probeRateAt.Load())) >= time.Second || rate > p.probeRate.Load() {
		p.probeRate.Store(rate)
		p.probeRateAt.Store(now.UnixNano())
	}
}

// Encoder pauses reduce offered traffic, not the capacity already delivered.
// Retain that proof while feedback stays live and delay remains stable. New
// queue growth removes it before a narrowed path can keep the old window.
func (p *Pacer) recordFlightCapacity(rate int64, rtt time.Duration, now time.Time) {
	baseline := time.Duration(p.stableFlightRTT.Load())
	if baseline <= 0 || rtt-baseline >= rttRise || p.mediaLoss(now) > .02 || !p.trialClear(now) {
		p.stableFlightRate.Store(rate)
		p.stableFlightRTT.Store(int64(rtt))
	} else {
		p.stableFlightRate.Store(max(rate, p.stableFlightRate.Load()))
		if rtt < baseline {
			p.stableFlightRTT.Store(int64(rtt))
		}
	}
	p.stableFlightAt.Store(now.UnixNano())
}

// A sparse rendition may leave a large numerical ceiling unused. Exercise
// only 25% above received capacity, rather than filling a FIFO at that ceiling.
// Stable delivered proof survives the quiet time between bounded trials.
// A trial proves steady video plus repair headroom and a one-second key.
// Key cost is independent of pixel count and has no encoded-size boundary.
func nativeTrialRate(demand, keyBytes int64) int64 {
	return max(demand*120/100, nativeRefreshRate(keyBytes)*110/100)
}

const keyEnvelope = time.Second

func nativeRefreshRate(keyBytes int64) int64 { return keyBytes * 8 }

func (p *Pacer) flightCapacity(now time.Time) int64 {
	rate := p.capacityRate(now)
	if at := p.stableFlightAt.Load(); at > 0 && now.Sub(time.Unix(0, at)) < time.Second {
		rate = max(rate, p.stableFlightRate.Load())
	}
	return rate
}

func (p *Pacer) flightLimited(rate int64, padding bool) bool {
	if !padding && !p.boundFlight.Load() && !p.thumbnail.Load() {
		return false
	}
	baseline := time.Duration(p.minimumFeedbackRTT.Load())
	if baseline <= 0 {
		baseline = time.Duration(p.pliRTT.Load())
	}
	if baseline <= 0 {
		baseline = 50 * time.Millisecond
	}
	proof := p.flightCapacity(time.Now())
	if proof <= 0 {
		proof = initialWireRate
	}
	minimum := int64(16 * wireMTU)
	if padding {
		// Existing packets still have to traverse the current round trip.
		// A physical-only flight window can block all discovery behind a
		// competing FIFO forever. Add at most the 200ms delivery window of new
		// work at the proven rate, within remaining forward-delay headroom.
		headroom := standingDelayLimit - time.Duration(p.networkExcess.Load())
		if headroom <= 0 {
			return true
		}
		transit := max(baseline, min(time.Duration(p.rtt.Load()), baseline+2*standingDelayLimit))
		limit := max(int64(wireMTU), int64(float64(proof)*(transit+min(200*time.Millisecond, headroom)).Seconds()/8))
		return p.departedBytes.Load()-p.acknowledgedBytes.Load() >= limit
	} else if time.Now().UnixNano() >= p.probeUntil.Load() {
		rate = min(rate, proof*125/100)
	}
	// A full feedback interval must fit even on a nearby path. A fixed byte
	// ceiling would cap default 4Mbit/s capture regardless of link capacity.
	// Sparse screens retain sixteen MTUs; congestion-inflated RTT is excluded.
	limit := max(minimum, int64(float64(rate)*(baseline+100*time.Millisecond).Seconds()/8))
	return p.departedBytes.Load()-p.acknowledgedBytes.Load() >= limit
}

// Match Pion's feedback RTT calculation, using the actual paced wire writes.
// TWCC arrives before the periodic receiver report, so a newly narrowed path
// can be cut before it accumulates a second of video. Remote clock offset
// cancels: only arrival deltas within this feedback message are used.
// Validate before consuming any departure. A malformed tail must not hide the
// valid prefix from a later well-formed feedback message.
func validTransportFeedback(f *rtcp.TransportLayerCC) bool {
	if f == nil || f.PacketStatusCount == 0 {
		return false
	}
	remaining, deltas := int(f.PacketStatusCount), 0
	add := func(symbol uint16, n int) bool {
		if symbol > rtcp.TypeTCCPacketReceivedLargeDelta {
			return false
		}
		n = min(n, remaining)
		remaining -= n
		if symbol != rtcp.TypeTCCPacketNotReceived {
			deltas += n
		}
		return true
	}
	for _, chunk := range f.PacketChunks {
		if remaining == 0 {
			break
		}
		switch c := chunk.(type) {
		case *rtcp.RunLengthChunk:
			if c == nil || !add(c.PacketStatusSymbol, int(c.RunLength)) {
				return false
			}
		case *rtcp.StatusVectorChunk:
			if c == nil {
				return false
			}
			for _, symbol := range c.SymbolList {
				if remaining == 0 {
					break
				}
				if !add(symbol, 1) {
					return false
				}
			}
		default:
			return false
		}
	}
	if remaining != 0 || deltas > len(f.RecvDeltas) {
		return false
	}
	for _, delta := range f.RecvDeltas[:deltas] {
		if delta == nil {
			return false
		}
	}
	return true
}

func (p *Pacer) FeedbackRTT(f *rtcp.TransportLayerCC, now time.Time) time.Duration {
	epoch := p.pathEpoch.Load()
	if p.feedbackEpoch != epoch {
		p.delivery = deliveryWindow{}
		p.trialDelivery = deliveryWindow{}
		p.feedbackSeen = false
		p.feedbackHigh = 0
		p.feedbackEpoch = epoch
		// Only a physical path change resets the forward clock (in resetPath).
		// A rendition fallback revokes receipts but keeps standing delay.
	}

	if !validTransportFeedback(f) {
		return 0
	}
	sequence := f.BaseSequenceNumber
	remaining := int(f.PacketStatusCount)
	deltaIndex := 0
	var arrival, last, first, videoLast time.Duration
	var receivedBytes, firstBytes int64
	var videoSentAt int64
	matched := 0
	var acknowledged int64
	minimum := time.Duration(1<<63 - 1)
	valid := true
	high, seen := p.feedbackHigh, p.feedbackSeen
	mediaExpected, mediaMissing := 0, 0
	trialExpected, trialMissing := 0, 0
	var lossTrialID int64
	var trialID, trialBytes int64
	var trialArrival time.Duration
	var missing [32]uint16
	missingN := 0
	visit := func(symbol uint16) {
		if remaining <= 0 {
			return
		}
		remaining--
		received := symbol == rtcp.TypeTCCPacketReceivedSmallDelta || symbol == rtcp.TypeTCCPacketReceivedLargeDelta
		if !seen || int16(sequence-high) > 0 {
			slot := &p.departures[int(sequence)%len(p.departures)]
			tag := uint32(sequence) + 1
			if slot.sequence.Load() == tag && slot.pathEpoch.Load() == p.pathEpoch.Load() {
				media, sent := slot.media.Load(), slot.at.Load()
				if slot.sequence.Load() == tag && slot.pathEpoch.Load() == p.pathEpoch.Load() {
					high, seen = sequence, true
					age := now.Sub(time.Unix(0, sent))
					if id := slot.trial.Load(); slot.kind.Load() == uint32(videoKind) && id > 0 && id >= lossTrialID && sent > 0 && age >= 0 && age <= max(500*time.Millisecond, 2*time.Duration(p.rtt.Load())) {
						if id > lossTrialID {
							trialExpected, trialMissing = 0, 0
							lossTrialID = id
						}
						trialExpected++
						if !received {
							trialMissing++
						}
					}
					if media > 0 && sent > 0 && age >= 0 && age <= max(500*time.Millisecond, 2*time.Duration(p.rtt.Load())) {
						mediaExpected++
						if !received {
							mediaMissing++
						}
					}
				}
			}
		}
		if !received && symbol == rtcp.TypeTCCPacketNotReceived && missingN < len(missing) {
			slot := &p.departures[int(sequence)%len(p.departures)]
			tag := uint32(sequence) + 1
			if slot.sequence.Load() == tag && slot.pathEpoch.Load() == p.pathEpoch.Load() {
				media, at := slot.media.Load(), slot.at.Load()
				if media > 0 && at > 0 && now.Sub(time.Unix(0, at)) >= time.Duration(p.rtt.Load())/2 && slot.sequence.Load() == tag {
					missing[missingN] = uint16(media - 1)
					missingN++
				}
			}
		}
		if received {
			if deltaIndex >= len(f.RecvDeltas) {
				valid = false
				return
			}
			arrival += time.Duration(f.RecvDeltas[deltaIndex].Delta) * time.Microsecond
			deltaIndex++
			last = max(last, arrival)
			slot := &p.departures[int(sequence)%len(p.departures)]
			tag := uint32(sequence) + 1
			if slot.sequence.Load() == tag && slot.pathEpoch.Load() == p.pathEpoch.Load() {
				at := slot.at.Load()
				if slot.sequence.Load() == tag && at > 0 {
					acknowledged = max(acknowledged, slot.total.Load())
					previous := slot.delivered.Load()
					if previous == at {
						p.feedbackDuplicates.Add(1)
					} else if slot.delivered.CompareAndSwap(previous, at) && slot.sequence.Load() == tag {
						minimum = min(minimum, time.Duration(now.UnixNano()-at)+arrival)
						size := slot.bytes.Load()
						if id := slot.trial.Load(); slot.kind.Load() == uint32(videoKind) && id > 0 && id >= trialID {
							if id > trialID {
								trialBytes = 0
								trialID = id
							}
							trialBytes += size
							trialArrival = arrival
						}
						if slot.kind.Load() == uint32(videoKind) {
							if matched == 0 {
								first = arrival
								firstBytes = size
							}
							if matched == 0 || arrival > videoLast {
								videoLast, videoSentAt = arrival, at
							}
							matched++
							receivedBytes += size
						}
					}
				}
			}
		}
		sequence++
	}
	for _, chunk := range f.PacketChunks {
		switch c := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := 0; i < int(c.RunLength) && remaining > 0; i++ {
				visit(c.PacketStatusSymbol)
			}
		case *rtcp.StatusVectorChunk:
			for _, symbol := range c.SymbolList {
				if remaining <= 0 {
					break
				}
				visit(symbol)
			}
		}
	}
	if !valid || remaining != 0 || p.pathEpoch.Load() != epoch {
		return 0
	}
	p.feedbackHigh, p.feedbackSeen = high, seen
	if trialBytes > 0 {
		p.recordTrialDelivery(trialID, time.Duration(f.ReferenceTime)*64*time.Millisecond+trialArrival, trialBytes, now)
	}
	if trialExpected > 0 && lossTrialID >= p.trialLossID {
		if lossTrialID > p.trialLossID {
			p.trialLossSamples = mediaLossWindow{}
			p.trialLossID = lossTrialID
		}
		loss := p.trialLossSamples.observe(now, trialExpected, trialMissing)
		p.trialLossPPM.Store(loss)
		if loss > 20000 {
			for previous := p.failedTrial.Load(); lossTrialID > previous; previous = p.failedTrial.Load() {
				if p.failedTrial.CompareAndSwap(previous, lossTrialID) {
					break
				}
			}
		}
		p.trialLossAt.Store(now.UnixNano())
		p.trialLossFor.Store(lossTrialID)
	}
	if mediaExpected > 0 {
		loss := p.mediaLossSamples.observe(now, mediaExpected, mediaMissing)
		p.mediaLossPPM.Store(loss)
		if loss > 10_000 {
			p.retainParityLoss(now.UnixNano())
		}
		if loss > 250_000 {
			p.retainSevereParityLoss(now.UnixNano())
		}
		p.mediaLossAt.Store(now.UnixNano())
	}
	if minimum == time.Duration(1<<63-1) {
		return 0
	}
	rtt := minimum - last
	if matched > 0 {
		rate := p.delivery.observe(time.Duration(f.ReferenceTime)*64*time.Millisecond+videoLast, receivedBytes)
		// Receiver-arrival minus actual wire-departure deltas cancel clock
		// offset. Feedback batching/reverse-path delay cannot invent a media
		// queue trend. Only video-kind departures enter this clock or proof.
		arrivalAt := time.Duration(f.ReferenceTime)*64*time.Millisecond + videoLast
		const clockPeriod = (1 << 24) * 64 * time.Millisecond
		elapsed := (arrivalAt - p.transitReceivedAt + clockPeriod) % clockPeriod
		if p.transitSendAt > 0 && videoSentAt > p.transitSendAt && elapsed < time.Minute {
			p.transitDelay.Add(int64(elapsed) - (videoSentAt - p.transitSendAt))
		} else {
			p.transitDelay.Store(0)
		}
		p.transitSendAt, p.transitReceivedAt = videoSentAt, arrivalAt
		p.transitAt.Store(videoSentAt)
		p.transitReports.Add(1)
		if rate > 0 {
			p.sustainedRate.Store(rate)
			p.sustainedRateAt.Store(now.UnixNano())
			p.recordProbeRate(rate, now)
			if rate := p.delivery.rate(500 * time.Millisecond); rate > 0 {
				p.returnRate.Store(rate)
				p.returnRateAt.Store(now.UnixNano())
			}
		}
	}
	for previous := p.acknowledgedBytes.Load(); acknowledged > previous; previous = p.acknowledgedBytes.Load() {
		if p.acknowledgedBytes.CompareAndSwap(previous, acknowledged) {
			break
		}
	}
	if matched >= 2 && videoLast-first >= 20*time.Millisecond && receivedBytes > firstBytes {
		rate := int64(float64(receivedBytes-firstBytes) * 8 / (videoLast - first).Seconds())
		p.receiptRate.Store(rate)
		if previous := p.feedbackRate.Load(); previous > 0 && now.Sub(time.Unix(0, p.feedbackRateAt.Load())) < time.Second {
			rate = (previous*7 + rate) / 8
		}
		p.feedbackRate.Store(rate)
		p.feedbackRateAt.Store(now.UnixNano())
	}
	if rtt <= 0 || rtt > 30*time.Second {
		return 0
	}
	for previous := p.minimumFeedbackRTT.Load(); previous == 0 || int64(rtt) < previous; previous = p.minimumFeedbackRTT.Load() {
		if p.minimumFeedbackRTT.CompareAndSwap(previous, int64(rtt)) {
			break
		}
	}
	if matched > 0 {
		p.recordFlightCapacity(p.capacityRate(now), rtt, now)
	}
	// Independent transport feedback can arrive when a NACK was lost. Limit
	// this early repair to short round trips; replay,
	// parity and padding never request another replay of themselves.
	pathRTT := time.Duration(p.rtt.Load())
	if pathRTT >= 50*time.Millisecond && pathRTT < 150*time.Millisecond {
		for _, seq := range missing[:missingN] {
			if p.Replay(seq) {
				p.FeedbackRepairs.Add(1)
			}
		}
	}
	p.notify()
	return rtt
}

// Original-media loss intentionally excludes padding. A capacity trial must
// also account for its padding losses before it can admit another encoding.
func (p *Pacer) trialClear(now time.Time) bool {
	id := p.probeID.Load()
	return id == 0 || p.failedTrial.Load() != id && (p.trialLossFor.Load() != id || now.Sub(time.Unix(0, p.trialLossAt.Load())) >= time.Second || p.trialLossPPM.Load() <= 20000)
}
func trialFeedbackComplete(now time.Time, until int64, rtt time.Duration) bool {
	return until > 0 && !now.Before(time.Unix(0, until).Add(max(50*time.Millisecond, rtt)))
}

// Keep the received window of the completed trial while feedback crosses a
// resident FIFO. Later low-rate media must not erase that trial's measurement.
func (p *Pacer) recordTrialDelivery(id int64, arrival time.Duration, bytes int64, now time.Time) {
	if id > p.trialDeliveryID {
		p.trialDelivery = deliveryWindow{}
		p.trialDeliveryID = id
	}
	if id != p.trialDeliveryID {
		return
	}
	p.trialDelivery.observe(arrival, bytes)
	if rate := p.trialDelivery.rate(500 * time.Millisecond); rate > 0 {
		p.trialReceived.Store(rate)
		p.trialReceivedAt.Store(now.UnixNano())
		p.trialReceivedID.Store(id)
	}
}
func (p *Pacer) trialRate(now time.Time) int64 {
	id := p.trialReceivedID.Load()
	if id == p.probeID.Load() && id > 0 && now.Sub(time.Unix(0, p.trialReceivedAt.Load())) < time.Second {
		return p.trialReceived.Load()
	}
	return 0
}
