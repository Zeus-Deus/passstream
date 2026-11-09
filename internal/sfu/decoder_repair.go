package sfu

import "time"

// Feedback can outlive a failed replay. Recover before the browser's longer
// PC-replacement timeout, without refreshing a decoder that is still advancing.
type decoderRepairRecovery struct{ requested time.Time }

func (r *decoderRepairRecovery) request(now time.Time, p *Peer, sourceAt time.Time) bool {
	if p.Pacer == nil || !p.decoderReported.Load() || p.Sent.Load() == 0 {
		return false
	}
	failed := p.Pacer.unrepairableReferenceAt.Load()
	fresh := p.freshAt.Load()
	feedback := p.decodedFeedbackAt.Load()
	if failed == 0 || now.Sub(time.Unix(0, failed)) > 2*time.Second {
		return false
	}
	// A heartbeat interval alone is not a stall. Require a fresh report that
	// explicitly kept the old counter, after the last admitted refresh.
	stopped := now.Sub(time.Unix(0, fresh)) >= 400*time.Millisecond && feedback-fresh >= int64(300*time.Millisecond)
	current := now.Sub(time.Unix(0, feedback)) <= 500*time.Millisecond && feedback >= p.Pacer.catchupAt.Load()
	// A delayed old picture can arrive after the failed replay. Its report
	// does not repair that reference. Lost heartbeat replies also must not
	// indefinitely defer a cheap independent refresh on a constrained path.
	feedbackGap := p.pathLimited.Load() && p.Room.hasLow.Load() && !p.Low.Load() &&
		now.Sub(time.Unix(0, fresh)) >= max(500*time.Millisecond, 3*time.Duration(p.initialRTT.Load())) &&
		now.Sub(time.Unix(0, feedback)) >= 500*time.Millisecond
	activeSource := !sourceAt.IsZero() && now.Sub(sourceAt) <= 500*time.Millisecond
	if !(stopped && current || feedbackGap) || !activeSource || now.Sub(r.requested) < time.Second {
		return false
	}
	if now.UnixNano() < p.Pacer.pliAfter.Load() && !p.livenessGap(now) || p.Pacer.KeyframePending() || p.Pacer.CatchupDelay(now) > 0 || !p.Pacer.unrepairableReferenceAt.CompareAndSwap(failed, 0) {
		return false
	}
	r.requested = now
	if p.pathLimited.Load() && p.Room.hasLow.Load() && !p.Low.Load() {
		p.requestKeyframeFor(true)
	} else {
		p.requestKeyframe()
	}
	return true
}

// A requested independent fallback remains the target until its key arrives.
// Only downshifts get this grace; native admission still needs fresh capacity.
func (p *Peer) pendingLowRefresh(now time.Time) bool {
	at := p.refreshRequestedAt.Load()
	return !p.Low.Load() && p.refreshEncoding.Load() == 2 && at > 0 && now.Sub(time.Unix(0, at)) < 2*time.Second
}
