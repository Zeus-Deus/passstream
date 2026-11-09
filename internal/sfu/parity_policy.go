package sfu

import (
	"time"
)

// A nearby managed queue can drop excess parity while keeping delay short.
// Prefer cheap NACK repair briefly when fast round trips occur during the loss
// episode. Successful parity repair still spends that shared queue's capacity.
// A fast sample from before
// a propagation change must not disable protection on the new Wi-Fi path.
type parityPolicy struct {
	firstLoss, lastLoss, offUntil time.Time
}

func (p *parityPolicy) queueLoss(now time.Time) bool {
	return now.Before(p.offUntil) && now.Sub(p.lastLoss) < 2*time.Second
}

func (p *parityPolicy) allow(now time.Time, rawLoss float64, residual uint32, fastRTTAt int64) bool {
	if rawLoss > .01 || residual > 2 {
		if p.firstLoss.IsZero() {
			p.firstLoss = now
		}
		p.lastLoss = now
	} else if now.Sub(p.lastLoss) > 10*time.Second {
		p.firstLoss = time.Time{}
	}
	fast := time.Unix(0, fastRTTAt)
	if (rawLoss > .01 || residual >= 3) && !p.firstLoss.IsZero() && fast.After(p.firstLoss) && now.Sub(fast) < 10*time.Second {
		// Let queue and estimator history drain before restoring parity.
		p.offUntil = now.Add(10 * time.Second)
	}
	return !now.Before(p.offUntil)
}

// One compressed TWCC arrival on Wi-Fi cannot establish a nearby queue.
func corroboratedFastRTT(twcc, receiver time.Duration) bool {
	return twcc > 0 && twcc < 30*time.Millisecond && receiver > 0 && receiver < 40*time.Millisecond
}

func (p *Peer) observeReceiverRTT(now time.Time, rtt time.Duration) {
	p.observePathRTT(now, rtt, false)
	p.rttNanos.Store(int64(rtt))
	p.receiverRTTAt.Store(now.UnixNano())
	if rtt > 0 && rtt < 40*time.Millisecond {
		if p.fastReceiverReports.Load() < 2 {
			p.fastReceiverReports.Add(1)
		}
	} else {
		p.fastReceiverReports.Store(0)
	}
	if rtt > 0 && rtt < 20*time.Millisecond && p.initialRTT.Load() < int64(30*time.Millisecond) {
		p.fastRTTAt.Store(now.UnixNano())
	}
}

func (p *Peer) confirmFeedbackProximity(twcc time.Duration) {
	if p.initialRTT.Load() < int64(30*time.Millisecond) && p.fastReceiverReports.Load() >= 2 && corroboratedFastRTT(twcc, time.Duration(p.rttNanos.Load())) {
		// Corroboration cannot turn an old clean-path report into new evidence
		// after loss starts, or classify Wi-Fi from one low jitter sample.
		p.fastRTTAt.Store(p.receiverRTTAt.Load())
	}
}
