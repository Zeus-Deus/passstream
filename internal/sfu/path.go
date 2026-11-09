package sfu

import (
	"github.com/pion/rtcp"
	"time"
)

// Window minima expire old propagation evidence. Both clocks are reset at the
// selected-pair boundary, before observations on the replacement path arrive.
type pathMinimum struct{ samples []rttSample }

func (m *pathMinimum) observe(now time.Time, rtt time.Duration) time.Duration {
	kept := m.samples[:0]
	least := rtt
	for _, s := range m.samples {
		if now.Sub(s.at) < rttWindow {
			kept = append(kept, s)
			least = min(least, s.rtt)
		}
	}
	m.samples = append(kept, rttSample{now, rtt})
	return least
}
func (p *Peer) resetPath(now time.Time) {
	p.pathMu.Lock()
	defer p.pathMu.Unlock()
	p.receiverMinimum = pathMinimum{}
	p.feedbackMinimum = pathMinimum{}
	p.initialRTT.Store(0)
	p.rttNanos.Store(0)
	p.receiverRTTAt.Store(0)
	p.twccMinimum.Store(0)
	p.twccRTT.Store(0)
	p.fastRTTAt.Store(0)
	p.fastReceiverReports.Store(0)
	if p.Pacer != nil {
		p.Pacer.resetProof()
		p.Pacer.transitReports.Store(0)
		p.Pacer.transitAt.Store(0)
		p.Pacer.transitDelay.Store(0)
		p.Pacer.transitSendAt, p.Pacer.transitReceivedAt = 0, 0
		p.Pacer.minimumFeedbackRTT.Store(0)
	}
	p.pathEpoch.Add(1)
}

// Called with the peer's pathMu held. A fallback revokes the old native
// reservation; delayed native receipts cannot prove the next low-to-native
// upgrade. The same departure fence also handles a selected-path change.
func (p *Pacer) resetProof() {
	p.pathEpoch.Add(1)
	p.acknowledgedBytes.Store(p.departedBytes.Load())
	p.probeUntil.Store(0)
	p.probeID.Store(0)
	p.probeEnd.Store(0)
	p.probeRate.Store(0)
	p.probeRateAt.Store(0)
	p.sustainedRateAt.Store(0)
	p.feedbackRateAt.Store(0)
	p.returnRateAt.Store(0)
	p.stableFlightRate.Store(0)
	p.stableFlightAt.Store(0)
}
func (p *Peer) observePathRTT(now time.Time, rtt time.Duration, feedback bool) {
	if rtt <= 0 {
		return
	}
	p.pathMu.Lock()
	defer p.pathMu.Unlock()
	if feedback {
		least := p.feedbackMinimum.observe(now, rtt)
		p.twccMinimum.Store(int64(least))
		if p.Pacer != nil {
			p.Pacer.minimumFeedbackRTT.Store(int64(least))
		}
	} else {
		// Keep the pre-media ICE/handshake sample for the same finite window;
		// the first queued receiver report cannot erase that propagation base.
		if len(p.receiverMinimum.samples) == 0 {
			if baseline := p.initialRTT.Load(); baseline > 0 {
				p.receiverMinimum.observe(now, time.Duration(baseline))
			}
		}
		least := p.receiverMinimum.observe(now, rtt)
		if p.pathEpoch.Load() > 0 {
			p.initialRTT.Store(int64(least))
		}
	}
}

// Serialize the pair boundary with publication of feedback minima. Queued
// feedback for departures on the previous path cannot seed the new minimum.
func (p *Peer) observeFeedback(now time.Time, f *rtcp.TransportLayerCC) {
	p.pathMu.Lock()
	defer p.pathMu.Unlock()
	if d := p.Pacer.FeedbackRTT(f, now); d > 0 {
		p.twccRTT.Store(int64(d))
		p.confirmFeedbackProximity(d)
		p.twccReports.Add(1)
		least := p.feedbackMinimum.observe(now, d)
		p.twccMinimum.Store(int64(least))
		p.Pacer.minimumFeedbackRTT.Store(int64(least))
	}
}
