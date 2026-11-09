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
// requests that a viewer originates.
func (p *Peer) pliPermitted() bool {
	p.observerMu.RLock()
	f := p.pliAllowed
	p.observerMu.RUnlock()
	return f == nil || f()
}

// requestKeyframe handles a viewer's PLI, FIR or keyframe heartbeat: an
// untrusted decoder report that only ctrl's key policy may act on.
func (p *Peer) requestKeyframe() {
	if !p.pliPermitted() || p.view == nil {
		return
	}
	p.view.stall(time.Now())
}

// ObservedStats uses receiver reports and the bytes actually written.
func (p *Peer) ObservedStats() (rtt, loss float64, wire uint64, ready bool) {
	rtt = float64(p.rttNanos.Load()) / 1e6
	loss = float64(p.residualLoss.Load()) * 100 / 256
	if p.view != nil {
		wire = p.view.wireBytes.Load()
	}
	ready = p.reports.Load() > 0
	return
}

// Age is how long ago the peer was created.
func (p *Peer) Age() time.Duration { return time.Since(p.born) }

// RefreshTransitMS is an advisory for an admitted key and the pictures
// queued behind it, bounded by the key envelope.
func (p *Peer) RefreshTransitMS() int64 {
	if p.Publish || p.view == nil {
		return 0
	}
	return p.view.keyTransit(time.Now()).Milliseconds()
}

// AppliedWireRate reports the viewer's current controller rate R.
func (p *Peer) AppliedWireRate() int64 {
	if p.view == nil {
		return 0
	}
	return int64(p.view.rate(time.Now()))
}
