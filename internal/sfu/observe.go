package sfu

import "time"

// SetObserver attaches signaling telemetry without changing the media protocol.
// Callbacks are replaced safely when the same PC resumes on another socket.
func (p *Peer) SetObserver(heartbeat func(*bool), pli func() bool) {
	p.observerMu.Lock()
	p.heartbeatObserver = heartbeat
	p.pliAllowed = pli
	p.observerMu.Unlock()
}
func (p *Peer) observe(hidden *bool) {
	p.observerMu.RLock()
	f := p.heartbeatObserver
	p.observerMu.RUnlock()
	if f != nil {
		f(hidden)
	}
}

// pliPermitted applies the server's per-viewer PLI limit to keyframe
// requests that a viewer originates. The SFU's own recovery requests bypass it.
func (p *Peer) pliPermitted() bool {
	p.observerMu.RLock()
	f := p.pliAllowed
	p.observerMu.RUnlock()
	return f == nil || f()
}
func (p *Peer) requestPLI() {
	p.requestKeyframe()
}

// ObservedStats uses receiver reports and actual bytes written by the pacer.
func (p *Peer) ObservedStats() (rtt, loss float64, wire uint64, ready bool) {
	rtt = float64(p.rttNanos.Load()) / 1e6
	loss = float64(p.residualLoss.Load()) * 100 / 256
	if p.Pacer != nil {
		wire = p.Pacer.wireBytes.Load()
	}
	ready = p.reports.Load() > 0
	return
}

// Age is how long ago the peer was created.
func (p *Peer) Age() time.Duration { return time.Since(p.born) }

// RefreshTransitMS is an advisory for an admitted key and its dependent frames.
// It does not extend the controller's bounded serialization envelope.
func (p *Peer) RefreshTransitMS() int64 {
	if p.Publish || p.Pacer == nil {
		return 0
	}
	return min(maxKeyframeCatchup, p.Pacer.CatchupDelay(time.Now())).Milliseconds()
}

// AppliedWireRate reports the actual shared wire budget, including proof caps.
func (p *Peer) AppliedWireRate() int64 {
	if p.Pacer == nil {
		return 0
	}
	return p.Pacer.wireRate(time.Now())
}
