package sfu

import "time"

type iceProximityEvidence uint8

const (
	iceProximityUnknown iceProximityEvidence = iota
	iceProximityNear
	iceProximityReset
)

// Connectivity checks give independent delay evidence when RTP feedback is
// scheduled late. Never reuse cached pre-loss RTT: first establish a baseline,
// then require two newer fast responses after loss starts. A changed pair
// revokes the preference; slower checks stop renewing its bounded hold. Queue
// delay on the same pair cannot erase independent fast receiver evidence.
type iceLossProximity struct {
	initialized, confirmed bool
	pair                   string
	responses              uint64
	epoch                  time.Time
	fast                   int
}

func (m *iceLossProximity) observe(pair string, responses uint64, rtt time.Duration, epoch time.Time) iceProximityEvidence {
	if !m.initialized {
		*m = iceLossProximity{initialized: true, pair: pair, responses: responses, epoch: epoch}
		return iceProximityUnknown
	}
	if pair != m.pair || responses < m.responses {
		*m = iceLossProximity{initialized: true, pair: pair, responses: responses, epoch: epoch}
		return iceProximityReset
	}
	if !m.confirmed && !epoch.Equal(m.epoch) {
		m.responses, m.epoch, m.fast = responses, epoch, 0
		return iceProximityUnknown
	}
	if responses == m.responses {
		return iceProximityUnknown
	}
	m.responses = responses
	if rtt <= 0 || rtt >= 20*time.Millisecond {
		m.fast, m.confirmed = 0, false
		return iceProximityUnknown
	}
	if epoch.IsZero() && !m.confirmed {
		m.fast = 0
		return iceProximityUnknown
	}
	m.fast++
	if m.fast >= 2 {
		m.confirmed = true
		return iceProximityNear
	}
	return iceProximityUnknown
}

func (p *Peer) applyICEProximity(now time.Time, evidence iceProximityEvidence, policy *parityPolicy) bool {
	switch evidence {
	case iceProximityNear:
		if p.initialRTT.Load() >= int64(30*time.Millisecond) {
			return false
		}
		p.fastRTTAt.Store(now.UnixNano())
		// New fast responses renew the already proven NACK preference during
		// quiet playback, avoiding another parity burst every ten seconds.
		policy.offUntil = now.Add(10 * time.Second)
		return true
	case iceProximityReset:
		p.fastRTTAt.Store(0)
		policy.offUntil = time.Time{}
		return true
	}
	return false
}

func (p *Peer) confirmICELossProximity(now time.Time, monitor *iceLossProximity, policy *parityPolicy) bool {
	if policy.firstLoss.IsZero() && !monitor.confirmed || p.PC == nil {
		return false
	}
	sctp := p.PC.SCTP()
	if sctp == nil || sctp.Transport() == nil {
		return false
	}
	stats, ok := sctp.Transport().ICETransport().GetSelectedCandidatePairStats()
	if !ok {
		return false
	}
	evidence := monitor.observe(stats.ID, stats.ResponsesReceived, time.Duration(stats.CurrentRoundTripTime*float64(time.Second)), policy.firstLoss)
	return p.applyICEProximity(now, evidence, policy)
}
