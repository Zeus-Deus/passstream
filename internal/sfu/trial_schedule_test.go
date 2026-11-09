package sfu

import (
	"testing"
	"time"
)

func TestCanceledTrialKeepsItsIdentityAndLateLoss(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	end := now.Add(750 * time.Millisecond).UnixNano()
	p.startTrial(end)
	p.probeUntil.Store(0)
	p.trialLossFor.Store(end)
	p.trialLossAt.Store(now.UnixNano())
	p.trialLossPPM.Store(100000)
	p.failedTrial.Store(end)
	if p.trialClear(now) || p.trialClear(now.Add(2*time.Second)) {
		t.Fatal("cancellation/age erased the failed trial")
	}
	p.startTrial(now.Add(3 * time.Second).UnixNano())
	if !p.trialClear(now) {
		t.Fatal("new trial inherited old loss")
	}
}
func TestStoppedTrialPreservesDepartureIdentityAndDeadline(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	end := now.Add(250 * time.Millisecond).UnixNano()
	p.startTrial(end)
	p.trialLossFor.Store(end)
	p.trialLossAt.Store(now.UnixNano())
	p.trialLossPPM.Store(100000)
	p.probeUntil.Store(0)
	if p.probeID.Load() != end || p.trialClear(now) || p.probeEnd.Load() != end {
		t.Fatal("stopping padding changed the trial or lost its tail")
	}
}
func TestWireTrialWaitsForFeedbackAndBacksOffActualFailures(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	var trials capacityTrials
	trials.observe(now, p, 2*time.Second, 0, false)
	trials.start(now, p, now.Add(750*time.Millisecond).UnixNano(), minBackoff)
	p.failedTrial.Store(p.probeID.Load())
	trials.observe(now.Add(time.Second), p, 2*time.Second, 0, false)
	if trials.ready(now.Add(2*time.Second)) || p.probeUntil.Load() != 0 {
		t.Fatal("canceled trial skipped feedback tail")
	}
	trials.observe(now.Add(2750*time.Millisecond), p, 2*time.Second, 0, false)
	if trials.backoff != 4*time.Second || trials.ready(now.Add(3*time.Second)) || !trials.ready(now.Add(4*time.Second)) {
		t.Fatal("wire failure ignored backoff", trials)
	}
	trials.start(now.Add(4*time.Second), p, now.Add(4750*time.Millisecond).UnixNano(), 8*time.Second)
	trials.observe(now.Add(5*time.Second), p, 50*time.Millisecond, 0, true)
	if trials.backoff != minBackoff || !trials.next.Equal(now.Add(6*time.Second)) {
		t.Fatal("successful wire trial retained backoff", trials)
	}
	trials.backoff = 30 * time.Second
	trials.next = now.Add(time.Hour)
	trials.observe(now.Add(5*time.Second+stableWindow), p, 50*time.Millisecond, 0, true)
	if trials.backoff != minBackoff || trials.next.After(now.Add(7*time.Second+stableWindow)) {
		t.Fatal("stable window retained wire backoff", trials)
	}
}
func TestQueueTroughDoesNotBypassFailedWireTrialBackoff(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	trials := capacityTrials{backoff: 8 * time.Second, next: now.Add(8 * time.Second)}
	trials.observe(now, p, time.Millisecond, 0, true)
	if trials.ready(now) {
		t.Fatal("idle AQM decoding reset wire backoff")
	}
	trials.observe(now.Add(100*time.Millisecond), p, time.Second, 0, true)
	trials.observe(now.Add(200*time.Millisecond), p, time.Millisecond, 0, true)
	if trials.ready(now.Add(200*time.Millisecond)) || trials.backoff != 8*time.Second {
		t.Fatal("clearing the trial's own queue erased its backoff")
	}
}

func TestQueueTroughDuringFeedbackTailPreservesFailureBackoff(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	var trials capacityTrials
	trials.observe(now, p, time.Second, 0, true)
	trials.start(now, p, now.Add(750*time.Millisecond).UnixNano(), minBackoff)
	p.failedTrial.Store(p.probeID.Load())
	trials.observe(now.Add(time.Second), p, 2*time.Second, 0, true)
	if trials.ready(now.Add(time.Second)) {
		t.Fatal("queue clear skipped feedback tail")
	}
	trials.observe(now.Add(2750*time.Millisecond), p, 2*time.Second, 0, true)
	if trials.ready(now.Add(2750*time.Millisecond)) || trials.backoff != 4*time.Second || !trials.ready(now.Add(4*time.Second)) {
		t.Fatal("clearing the queue bypassed or extended the failed trial's backoff")
	}
}
func TestCompletedInsufficientTrialBacksOffWithoutASecondDelayController(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	var trials capacityTrials
	trials.start(now, p, now.Add(capacityTrialDuration).UnixNano(), minBackoff)
	p.trialReceivedID.Store(p.probeID.Load())
	p.trialReceivedAt.Store(now.Add(time.Second).UnixNano())
	p.trialReceived.Store(128_000)
	trials.observe(now.Add(time.Second), p, 100*time.Millisecond, 256_000, false)
	if !trials.failed || p.trialClear(now.Add(time.Second)) || trials.backoff != 4*time.Second {
		t.Fatal("completed insufficient delivery did not fail/back off", trials)
	}
}

func TestAbortedUnsampledTrialCannotCompoundBackoff(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	trials := capacityTrials{backoff: 8 * time.Second}
	trials.start(now, p, now.Add(capacityTrialDuration).UnixNano(), minBackoff)
	p.probeUntil.Store(0)
	trials.observe(now.Add(time.Second), p, 100*time.Millisecond, 256_000, false)
	if trials.pending || trials.failed || trials.backoff != 8*time.Second || !trials.next.Equal(now.Add(8*time.Second)) {
		t.Fatal("inconclusive delivery erased or compounded the previous failed-trial backoff", trials)
	}
}

func TestTrialTestsItsOfferedStepWithoutInventingNativeProof(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.probeCeiling.Store(128_000)
	trials := capacityTrials{backoff: 8 * time.Second}
	trials.start(now, p, now.Add(capacityTrialDuration).UnixNano(), minBackoff)
	p.trialReceivedID.Store(p.probeID.Load())
	p.trialReceivedAt.Store(now.Add(time.Second).UnixNano())
	p.trialReceived.Store(125_000)
	trials.observe(now.Add(time.Second), p, 100*time.Millisecond, 256_000, false)
	if trials.failed || trials.backoff != minBackoff || p.capacityRate(now.Add(time.Second)) >= 256_000 {
		t.Fatal("an offered intermediate step was confused with failed or proven native capacity", trials)
	}
}

func TestLateTrialLossStillDefersTheNextWireExperiment(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	var trials capacityTrials
	trials.observe(now, p, 50*time.Millisecond, 0, true)
	trials.start(now, p, now.Add(750*time.Millisecond).UnixNano(), minBackoff)
	trials.observe(now.Add(time.Second), p, 50*time.Millisecond, 0, true)
	if trials.pending || trials.failed {
		t.Fatal("clean feedback tail did not finish")
	}
	p.failedTrial.Store(p.probeID.Load())
	trials.observe(now.Add(1500*time.Millisecond), p, 50*time.Millisecond, 0, true)
	if trials.backoff != 4*time.Second || trials.ready(now.Add(2*time.Second)) {
		t.Fatal("late loss was discarded between trials", trials)
	}
	trials.observe(now.Add(1600*time.Millisecond), p, 50*time.Millisecond, 0, true)
	if trials.backoff != 4*time.Second {
		t.Fatal("one late failure was counted twice")
	}
}

func TestWireFailurePreservesALongerTransportBackoff(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	var trials capacityTrials
	trials.observe(now, p, 50*time.Millisecond, 0, false)
	trials.start(now, p, now.Add(750*time.Millisecond).UnixNano(), 8*time.Second)
	p.failedTrial.Store(p.probeID.Load())
	trials.observe(now.Add(time.Second), p, 50*time.Millisecond, 0, false)
	if trials.ready(now.Add(4*time.Second)) || !trials.ready(now.Add(8*time.Second)) {
		t.Fatal("wire failure shortened the controller's backoff", trials)
	}
}
