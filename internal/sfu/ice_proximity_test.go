package sfu

import (
	"testing"
	"time"
)

func TestNearbyLossNeedsTwoNewICEResponsesAfterItsEpoch(t *testing.T) {
	now := time.Now()
	var m iceLossProximity
	if m.observe("a", 10, time.Millisecond, time.Time{}) != iceProximityUnknown {
		t.Fatal("cached RTT established proximity")
	}
	if m.observe("a", 11, time.Millisecond, now) != iceProximityUnknown {
		t.Fatal("pre-loss response established proximity")
	}
	if m.observe("a", 11, time.Millisecond, now) != iceProximityUnknown {
		t.Fatal("cached response counted twice")
	}
	if m.observe("a", 12, time.Millisecond, now) != iceProximityUnknown {
		t.Fatal("one new response disabled repair")
	}
	if m.observe("a", 13, time.Millisecond, now) != iceProximityNear {
		t.Fatal("two independent fast responses could not prefer NACK")
	}
	// Quiet playback must not re-enable competing parity between loss episodes.
	if m.observe("a", 14, time.Millisecond, time.Time{}) != iceProximityNear {
		t.Fatal("fresh proximity could not renew quiet preference")
	}
	if m.observe("a", 14, time.Millisecond, time.Time{}) != iceProximityUnknown {
		t.Fatal("cached proximity renewed its preference")
	}
}

func TestNewCandidatePairRevokesOldProximity(t *testing.T) {
	now := time.Now()
	var m iceLossProximity
	m.observe("a", 10, time.Millisecond, now)
	m.observe("a", 11, time.Millisecond, now)
	m.observe("a", 12, time.Millisecond, now)
	p := &Peer{}
	var policy parityPolicy
	policy.allow(now, .2, 10, 0)
	p.applyICEProximity(now.Add(time.Second), iceProximityNear, &policy)
	if policy.allow(now.Add(time.Second), .2, 10, p.fastRTTAt.Load()) {
		t.Fatal("nearby queue retained parity")
	}
	for _, kind := range []string{"pair", "counter-reset"} {
		copyMonitor := m
		var evidence iceProximityEvidence
		switch kind {
		case "pair":
			evidence = copyMonitor.observe("b", 13, time.Millisecond, now)
		case "counter-reset":
			evidence = copyMonitor.observe("a", 1, time.Millisecond, now)
		}
		if evidence != iceProximityReset {
			t.Fatal("old path proof survived", kind)
		}
		p.applyICEProximity(now.Add(2*time.Second), evidence, &policy)
		if !policy.allow(now.Add(2*time.Second), .2, 10, p.fastRTTAt.Load()) {
			t.Fatal("Wi-Fi inherited a NACK-only hold", kind)
		}
	}
	p.initialRTT.Store(int64(80 * time.Millisecond))
	if p.applyICEProximity(now, iceProximityNear, &policy) {
		t.Fatal("known propagation accepted a compressed nearby claim")
	}
}

func TestQuietUnprovedPathCannotDisableParity(t *testing.T) {
	var m iceLossProximity
	for n := uint64(1); n < 10; n++ {
		if m.observe("a", n, time.Millisecond, time.Time{}) != iceProximityUnknown {
			t.Fatal("loss-free startup established a future queue-loss exemption")
		}
	}
}

// Queued checks are not proof of a new path. In particular they must not clear
// a fast receiver report sharing the peer's evidence timestamp.
func TestSlowerICECheckCannotEraseIndependentFastReceiverEvidence(t *testing.T) {
	now := time.Now()
	var m iceLossProximity
	m.observe("a", 10, time.Millisecond, now)
	m.observe("a", 11, time.Millisecond, now)
	m.observe("a", 12, time.Millisecond, now)
	p := &Peer{}
	p.observeReceiverRTT(now.Add(time.Second), time.Millisecond)
	var policy parityPolicy
	policy.allow(now, .2, 10, 0)
	p.applyICEProximity(now.Add(time.Second), iceProximityNear, &policy)
	at := p.fastRTTAt.Load()
	evidence := m.observe("a", 13, 80*time.Millisecond, now)
	if evidence != iceProximityUnknown || p.applyICEProximity(now.Add(2*time.Second), evidence, &policy) || p.fastRTTAt.Load() != at {
		t.Fatal("same-pair queue delay erased independent proximity")
	}
	if policy.allow(now.Add(2*time.Second), 0, 0, p.fastRTTAt.Load()) {
		t.Fatal("queued check prematurely restored competing parity")
	}
	if !policy.allow(now.Add(12*time.Second), 0, 0, p.fastRTTAt.Load()) {
		t.Fatal("slower path renewed the bounded preference")
	}
}
