package sfu

import (
	"testing"
	"time"
)

func TestResidualLossOnAFastPathTemporarilyPrefersNACK(t *testing.T) {
	now := time.Unix(100, 0)
	var p parityPolicy
	if !p.allow(now, .2, 0, 0) {
		t.Fatal("raw loss alone disabled protection")
	}
	if p.allow(now.Add(2*time.Second), .2, 3, now.Add(time.Second).UnixNano()) {
		t.Fatal("residual queue loss kept redundant parity")
	}
	if p.allow(now.Add(4*time.Second), 0, 0, 0) {
		t.Fatal("parity returned before queue history drained")
	}
	if !p.allow(now.Add(12*time.Second), 0, 0, 0) {
		t.Fatal("parity holdoff did not expire")
	}
}

func TestPreImpairmentOrStaleFastRTTDoesNotDisableWiFiProtection(t *testing.T) {
	now := time.Unix(100, 0)
	for _, fast := range []int64{0, now.Add(-time.Second).UnixNano(), now.Add(-20 * time.Second).UnixNano()} {
		var p parityPolicy
		if !p.allow(now, .3, 40, fast) || !p.allow(now.Add(2*time.Second), .3, 40, fast) {
			t.Fatal("old fast path disabled the current loss protection")
		}
	}
	var p parityPolicy
	p.allow(now, .2, 0, 0)
	if p.allow(now.Add(time.Second), .2, 0, now.Add(time.Second).UnixNano()) {
		t.Fatal("successful repair kept spending proactive copies on a cheap NACK path")
	}
}

func TestStableDeliveryCanProbeBeforeTheRepairPreferenceExpires(t *testing.T) {
	now := time.Unix(100, 0)
	var p parityPolicy
	p.allow(now, .2, 0, 0)
	p.allow(now.Add(time.Second), .2, 0, now.Add(time.Second).UnixNano())
	if !p.queueLoss(now.Add(time.Second)) || p.queueLoss(now.Add(4*time.Second)) {
		t.Fatal("current queue loss and the longer NACK preference were conflated")
	}
	if p.allow(now.Add(4*time.Second), 0, 0, 0) {
		t.Fatal("quiet delivery prematurely restored competing parity")
	}
}

func TestCompressedFeedbackNeedsRepeatedFreshReceiverReports(t *testing.T) {
	now := time.Now()
	p := &Peer{}
	p.observeReceiverRTT(now, 35*time.Millisecond)
	p.confirmFeedbackProximity(20 * time.Millisecond)
	if p.fastRTTAt.Load() != 0 {
		t.Fatal("one low Wi-Fi sample established a nearby queue")
	}
	p.observeReceiverRTT(now.Add(time.Second), 35*time.Millisecond)
	p.confirmFeedbackProximity(20 * time.Millisecond)
	var policy parityPolicy
	if !policy.allow(now.Add(2*time.Second), .1, 0, p.fastRTTAt.Load()) {
		t.Fatal("old receiver reports became new evidence after loss began")
	}
	p.observeReceiverRTT(now.Add(3*time.Second), 80*time.Millisecond)
	p.observeReceiverRTT(now.Add(4*time.Second), 35*time.Millisecond)
	p.confirmFeedbackProximity(20 * time.Millisecond)
	if !policy.allow(now.Add(4*time.Second), .1, 0, p.fastRTTAt.Load()) {
		t.Fatal("a propagation change retained old proximity confirmation")
	}
	p.observeReceiverRTT(now.Add(5*time.Second), 35*time.Millisecond)
	p.confirmFeedbackProximity(20 * time.Millisecond)
	if policy.allow(now.Add(5*time.Second), .1, 0, p.fastRTTAt.Load()) {
		t.Fatal("repeated current nearby reports could not select cheap repair")
	}
}

func TestNearbyQueueRequiresReceiverCorroboration(t *testing.T) {
	if corroboratedFastRTT(20*time.Millisecond, 80*time.Millisecond) || corroboratedFastRTT(20*time.Millisecond, 0) {
		t.Fatal("compressed Wi-Fi feedback disabled parity")
	}
	if !corroboratedFastRTT(20*time.Millisecond, 20*time.Millisecond) {
		t.Fatal("corroborated nearby queue was ignored")
	}
}

func TestKnownPropagationCannotBecomeANearbyQueueFromCompressedFeedback(t *testing.T) {
	now := time.Now()
	p := &Peer{}
	p.initialRTT.Store(int64(42 * time.Millisecond))
	p.observeReceiverRTT(now, 35*time.Millisecond)
	p.observeReceiverRTT(now.Add(time.Second), 35*time.Millisecond)
	p.confirmFeedbackProximity(20 * time.Millisecond)
	p.observeReceiverRTT(now.Add(2*time.Second), 15*time.Millisecond)
	if p.fastRTTAt.Load() != 0 {
		t.Fatal("compressed feedback discarded the known physical propagation")
	}
}
