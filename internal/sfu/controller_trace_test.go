package sfu

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDelayTrendIsIndependentOfFeedbackCadence(t *testing.T) {
	for _, period := range []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond} {
		var trend delayTrend
		now := time.Unix(100, 0)
		for elapsed := time.Duration(0); elapsed <= time.Second; elapsed += period {
			trend.observe(now.Add(elapsed), elapsed/10)
		}
		if slope := trend.slope(); slope < 99 || slope > 101 {
			t.Fatalf("cadence=%s slope=%f", period, slope)
		}
	}
}

// Port the independent round-four ramp/backoff simulations to the single
// observation API, with assertions (the original overlay only logged results).
func TestReviewRampAndKeyBacklogReachMeasuredDemand(t *testing.T) {
	for _, backlog := range []time.Duration{0, 25 * time.Millisecond} {
		now := time.Unix(100, 0)
		var c wireController
		var elapsed time.Duration
		for ; elapsed <= 1100*time.Millisecond; elapsed += 100 * time.Millisecond {
			queue := time.Duration(0)
			if elapsed < 400*time.Millisecond {
				queue = backlog
			}
			c.step(now.Add(elapsed), rateObservation{demand: 2_500_000, queue: queue})
			if c.rate >= 2_500_000 {
				break
			}
		}
		if c.rate < 2_500_000 {
			t.Fatal("clean capacity or local key debt latched backoff", backlog, elapsed, c.rate)
		}
		t.Logf("backlog=%s demand=2.5Mbit/s reached=%s", backlog, elapsed)
	}
}

func TestReviewIdleControllerResetsFailedProbeBackoff(t *testing.T) {
	c := wireController{rate: initialWireRate}
	now := time.Unix(100, 0)
	for i := 0; i < 6000; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{})
	}
	if c.probeAt.After(now.Add(600 * time.Second)) {
		t.Fatal("idle controller retained a deferred probe", c.probeAt)
	}
}
func TestSlowQueueRampCutsWithoutBeingUndoneByDecodedFloor(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 3000000, proof: 3000000}
	cut := false
	firstCut := time.Duration(0)
	for i := 0; i < 30; i++ {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		before := c.rate
		c.step(at, rateObservation{reports: uint64(i + 1), excess: time.Duration(i) * 10 * time.Millisecond, received: 3000000, required: 3000000, demand: 6000000, productive: true, full: true})
		if c.cutAt.Equal(at) {
			if !cut {
				firstCut = at.Sub(now)
			}
			cut = true
			if c.rate >= before {
				t.Fatal("floor undid delay cut")
			}
		}
	}
	if !cut {
		t.Fatal("100ms/s queue ramp never cut")
	}
	if firstCut > 1500*time.Millisecond {
		t.Fatal("a slow queue ramp was detected too late", firstCut)
	}
}

func TestStationaryFeedbackJitterDoesNotBecomeAQueueRamp(t *testing.T) {
	for _, cadence := range []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond} {
		now := time.Unix(100, 0)
		c := wireController{rate: 2_000_000}
		for i := 0; i < 100; i++ {
			at := now.Add(time.Duration(i) * cadence)
			c.step(at, rateObservation{reports: uint64(i + 1), delayAt: at, delay: time.Duration(i%2) * 40 * time.Millisecond, excess: 80 * time.Millisecond, received: 2_000_000, mediaDemand: 1_000_000, required: 1_200_000, demand: 2_000_000, productive: true})
		}
		if !c.cutAt.IsZero() {
			t.Fatal("stationary jitter cut a productive path", cadence, c.rate)
		}
	}
}
func TestStandingQueueHasACapEvenWithoutATrend(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 4000000}
	for i := 0; i < 20; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{reports: uint64(i + 1), excess: 700 * time.Millisecond, received: 500000, demand: 4000000})
	}
	if c.rate > 500000 {
		t.Fatal("standing excess did not bound the offered rate", c.rate)
	}
}

func TestCadenceQuantizationCannotBorrowAHalfRateLayer(t *testing.T) {
	for _, tc := range []struct {
		fps, source int64
		full        bool
	}{{28000, 30000, true}, {15000, 30000, false}, {13000, 15000, true}, {12000, 15000, false}, {3000, 4000, true}, {2000, 4000, false}, {1000, 1000, true}, {0, 1000, false}} {
		if fullCadence(tc.fps, tc.source) != tc.full {
			t.Fatal("cadence did not respect whole-frame edges", tc)
		}
	}
}

func TestActualTrialReceiptsCanBridgeDecoderLagBelowTheStandingCap(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 600_000, proof: 100_000}
	for i := range 20 {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{
			reports: uint64(i + 1), excess: time.Duration(i) * 10 * time.Millisecond,
			sentRate: 200_000, received: 200_000, mediaDemand: 180_000,
			required: 220_000, demand: 600_000, trial: true,
		})
	}
	if c.rate < 220_000 || c.proof < 180_000 {
		t.Fatal("actual received media was erased while the decoder report lagged", c.rate, c.proof)
	}
	for i := 20; i < 40; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{
			reports: uint64(i + 1), excess: standingDelayLimit + 100*time.Millisecond,
			sentRate: 200_000, received: 200_000, mediaDemand: 180_000,
			required: 220_000, demand: 600_000, trial: true,
		})
	}
	if c.rate >= 180_000 {
		t.Fatal("a trial without current full decoding bypassed the standing cap")
	}
}

func TestCompetingQueueCannotConsumeAnUnusedDeliveredReservation(t *testing.T) {
	for _, demand := range []int{180_000, 2_000_000} {
		now := time.Unix(100, 0)
		c := wireController{rate: demand * 3}
		for i := 0; i < 20; i++ {
			c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{
				reports: uint64(i + 1), excess: time.Duration(100+i*5) * time.Millisecond,
				sentRate: demand, received: demand, mediaDemand: demand,
				required: demand * 12 / 10, demand: demand * 3, productive: true,
			})
		}
		if !c.cutAt.IsZero() {
			t.Fatal("cutting unused rate cannot drain another flow's queue", demand, c.rate)
		}
		// The same source must yield when delivery really falls behind.
		for i := 20; i < 50; i++ {
			c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{
				reports: uint64(i + 1), excess: time.Duration(100+i*5) * time.Millisecond,
				sentRate: demand, received: demand / 2, mediaDemand: demand,
				required: demand * 12 / 10, demand: demand * 3,
			})
		}
		if c.cutAt.IsZero() {
			t.Fatal("actual delivery collapse borrowed an old reservation", demand)
		}
	}
}

func TestSmallDeliveryDeficitStillCutsAnUnusedCeiling(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 6_000_000, proof: 2_000_000}
	for i := 0; i < 20; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{
			reports: uint64(i + 1), excess: time.Duration(100+i*3) * time.Millisecond,
			sentRate: 2_000_000, received: 1_940_000, mediaDemand: 2_000_000,
			required: 2_400_000, demand: 6_000_000, productive: true,
		})
	}
	if c.cutAt.IsZero() || c.rate >= 2_000_000 {
		t.Fatal("a 30ms/s ramp borrowed application-limited protection", c.rate)
	}
}

func TestStandingExcessUsesTheSameForwardClockAsTheTrend(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 4_000_000}
	for i := range 30 {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		c.step(at, rateObservation{reports: uint64(i + 1), delayAt: at,
			delay:  time.Duration(i%2) * 10 * time.Millisecond,
			excess: 700 * time.Millisecond, received: 500_000, demand: 4_000_000})
	}
	if !c.cutAt.IsZero() {
		t.Fatal("reverse-path RTT or feedback scheduling cut a clear forward path")
	}
	for i := 30; i < 60; i++ {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		c.step(at, rateObservation{reports: uint64(i + 1), delayAt: at,
			delay: standingDelayLimit + 100*time.Millisecond, received: 500_000, demand: 4_000_000})
	}
	if c.rate > 500_000 || c.queueExcess < standingDelayLimit {
		t.Fatal("a real standing forward queue escaped the cap", c.rate, c.queueExcess)
	}
}

func TestDecodedReservationLivesInsideTheCutAndExpiresWithCadence(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 600_000, proof: 600_000}
	cut := false
	for i := range 15 {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		c.step(at, rateObservation{reports: uint64(i + 1), excess: time.Duration(i) * 10 * time.Millisecond,
			sentRate: 180_000, received: 150_000, mediaDemand: 180_000,
			required: 220_000, demand: 600_000, productive: true, full: true})
		cut = cut || c.cutAt.Equal(at)
	}
	if !cut || c.rate != 220_000 || c.proof < 180_000 || c.proof > 600_000 {
		t.Fatal("variable-size pictures consumed a complete decoded reservation", c.rate, c.proof)
	}
	for i := 15; i < 35; i++ {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		c.step(at, rateObservation{reports: uint64(i + 1), excess: time.Duration(i) * 10 * time.Millisecond,
			sentRate: 180_000, received: 100_000, mediaDemand: 180_000,
			required: 220_000, demand: 600_000, productive: true})
	}
	if c.rate >= 180_000 || c.proof >= 180_000 {
		t.Fatal("a decoder below source cadence retained the old reservation", c.rate, c.proof)
	}
}

func TestStandingCapKeepsOnlyTheActuallyDeliveredDecodedReservation(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 600_000, proof: 600_000}
	for i := range 20 {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{
			reports: uint64(i + 1), excess: standingDelayLimit + 100*time.Millisecond,
			sentRate: 180_000, received: 180_000, mediaDemand: 180_000,
			required: 220_000, demand: 600_000, productive: true, full: true,
		})
	}
	if c.rate != 220_000 || c.cutAt.IsZero() {
		t.Fatal("standing excess did not cap the unused ceiling at decoded demand", c.rate)
	}
	for i := 20; i < 40; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{
			reports: uint64(i + 1), excess: standingDelayLimit + 100*time.Millisecond,
			sentRate: 180_000, received: 100_000, mediaDemand: 180_000,
			required: 220_000, demand: 600_000, productive: true, full: true,
		})
	}
	if c.rate >= 180_000 {
		t.Fatal("decoder claims retained a reservation after delivery collapsed", c.rate)
	}
}

func TestReplacementInheritsAppliedRateInsteadOfUnusedCeiling(t *testing.T) {
	now := time.Now()
	old := &Peer{Pacer: &Pacer{}}
	old.Pacer.effective.Store(6_000_000)
	old.Pacer.probeRate.Store(200_000)
	old.Pacer.probeRateAt.Store(now.UnixNano())
	r := &Room{}
	r.hasLow.Store(true)
	r.keyBytes.Store(300_000)
	next := &Peer{Room: r, Pacer: &Pacer{}}
	next.InheritPath(old)
	if next.Pacer.effective.Load() != 220_000 || !next.Low.Load() {
		t.Fatal("replacement spent an unproven ceiling", next.Pacer.effective.Load())
	}
}

func TestUnknownLowMediaCannotBorrowItsPaddingTrial(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.thumbnail.Store(true)
	p.effective.Store(6_000_000)
	p.probeCeiling.Store(3_000_000)
	if p.wireRate(now) != initialWireRate {
		t.Fatal("unknown low path spent the room ceiling")
	}
	p.startTrial(now.Add(750 * time.Millisecond).UnixNano())
	if p.wireRate(now) != 3_000_000 || p.mediaRate(now) != initialWireRate {
		t.Fatal("encoded low media borrowed the exploratory rate")
	}
}
func TestSelectedPairRebasesBothRTTClocksAfter150msStep(t *testing.T) {
	now := time.Unix(100, 0)
	p := &Peer{Pacer: &Pacer{}}
	p.initialRTT.Store(int64(40 * time.Millisecond))
	p.observePathRTT(now, 40*time.Millisecond, false)
	p.observePathRTT(now, 80*time.Millisecond, true)
	if p.initialRTT.Load() != int64(40*time.Millisecond) || p.twccMinimum.Load() != int64(80*time.Millisecond) {
		t.Fatal("test did not establish the original path")
	}
	p.resetPath(now.Add(time.Second))
	p.observePathRTT(now.Add(2*time.Second), 190*time.Millisecond, false)
	p.observePathRTT(now.Add(2*time.Second), 230*time.Millisecond, true)
	if p.initialRTT.Load() != int64(190*time.Millisecond) || p.twccMinimum.Load() != int64(230*time.Millisecond) || p.Pacer.minimumFeedbackRTT.Load() != int64(230*time.Millisecond) {
		t.Fatal("old path minimum survived")
	}
	p.observePathRTT(now.Add(40*time.Second), 240*time.Millisecond, true)
	p.observePathRTT(now.Add(40*time.Second), 200*time.Millisecond, false)
	if p.twccMinimum.Load() != int64(240*time.Millisecond) {
		t.Fatal("minimum did not expire")
	}
	if p.initialRTT.Load() != int64(200*time.Millisecond) {
		t.Fatal("receiver baseline did not expire")
	}
}

func TestOldPathFeedbackCannotSeedReplacementMinimumOrProof(t *testing.T) {
	p := &Peer{Pacer: &Pacer{}}
	now := time.Unix(100, 0)
	slot := &p.Pacer.departures[0]
	slot.sequence.Store(1)
	slot.at.Store(now.UnixNano())
	slot.bytes.Store(1200)
	slot.kind.Store(uint32(videoKind))
	p.resetPath(now.Add(time.Millisecond))
	f := &rtcp.TransportLayerCC{PacketStatusCount: 1, PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 1, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: 1000}}}
	p.observeFeedback(now.Add(50*time.Millisecond), f)
	if p.twccMinimum.Load() != 0 || p.Pacer.sustainedRateAt.Load() != 0 || p.Pacer.transitReports.Load() != 0 {
		t.Fatal("delayed feedback on the previous pair seeded the replacement path")
	}
}

func TestFallbackFencesNativeReceiptsWithoutRebasingDelay(t *testing.T) {
	p := &Peer{Pacer: &Pacer{}}
	now := time.Unix(100, 0)
	p.initialRTT.Store(int64(80 * time.Millisecond))
	p.Pacer.minimumFeedbackRTT.Store(int64(80 * time.Millisecond))
	p.Pacer.probeRate.Store(5_000_000)
	p.Pacer.probeRateAt.Store(now.UnixNano())
	p.Pacer.sustainedRateAt.Store(now.UnixNano())
	p.Pacer.transitDelay.Store(int64(150 * time.Millisecond))
	p.Pacer.transitSendAt = now.Add(-100 * time.Millisecond).UnixNano()
	p.Pacer.transitReceivedAt = 200 * time.Second
	slot := &p.Pacer.departures[0]
	slot.sequence.Store(1)
	slot.at.Store(now.UnixNano())
	slot.bytes.Store(1200)
	slot.kind.Store(uint32(videoKind))
	p.pathMu.Lock()
	p.Pacer.resetProof()
	p.pathMu.Unlock()
	f := &rtcp.TransportLayerCC{PacketStatusCount: 1, PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 1, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: 1000}}}
	p.observeFeedback(now.Add(80*time.Millisecond), f)
	if p.Pacer.capacityRate(now) != 0 || p.initialRTT.Load() != int64(80*time.Millisecond) || p.Pacer.minimumFeedbackRTT.Load() != int64(80*time.Millisecond) || p.Pacer.transitDelay.Load() != int64(150*time.Millisecond) {
		t.Fatal("fallback reused native proof or erased path delay")
	}
	for i := 1; i <= 4; i++ {
		sent := now.Add(time.Duration(i) * 100 * time.Millisecond)
		arrival := 200*time.Second + time.Duration(i+1)*100*time.Millisecond
		slot := &p.Pacer.departures[i]
		slot.sequence.Store(uint32(i + 1))
		slot.pathEpoch.Store(p.Pacer.pathEpoch.Load())
		slot.at.Store(sent.UnixNano())
		slot.bytes.Store(1200)
		slot.kind.Store(uint32(videoKind))
		f.BaseSequenceNumber = uint16(i)
		f.ReferenceTime = uint32(arrival / (64 * time.Millisecond))
		f.RecvDeltas[0].Delta = int64((arrival - time.Duration(f.ReferenceTime)*64*time.Millisecond) / time.Microsecond)
		p.observeFeedback(sent.Add(80*time.Millisecond), f)
	}
	if rate := p.Pacer.capacityRate(now.Add(time.Second)); rate <= 0 || rate > 128_000 || p.Pacer.transitDelay.Load() != int64(150*time.Millisecond) {
		t.Fatal("new low proof or the preserved forward clock is wrong", rate, p.Pacer.transitDelay.Load())
	}
}
func TestNativeKeyBudgetCannotBeRacedOrBypassedByCause(t *testing.T) {
	now := time.Unix(100, 0)
	p := &Peer{}
	for tick := 0; tick < 300; tick++ {
		at := now.Add(time.Duration(tick) * 100 * time.Millisecond)
		var accepted atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if p.takeNativeKey(at) {
					accepted.Add(1)
				}
			})
		}
		wg.Wait()
		want := int32(0)
		if tick%100 == 0 {
			want = 1
		}
		if accepted.Load() != want {
			t.Fatalf("tick=%d requests=%d want=%d", tick, accepted.Load(), want)
		}
	}
}

func TestReplacementSharesTheOriginalNativeKeyBudget(t *testing.T) {
	old := &Peer{nativeKeyOwner: new(atomic.Int64)}
	next := &Peer{}
	next.InheritPath(old)
	now := time.Unix(100, 0)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := range 100 {
		p := old
		if i%2 == 0 {
			p = next
		}
		wg.Go(func() {
			if p.takeNativeKey(now) {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 || next.takeNativeKey(now.Add(9*time.Second)) || !next.takeNativeKey(now.Add(10*time.Second)) || old.takeNativeKey(now.Add(10*time.Second)) {
		t.Fatal("replacement reset or duplicated the admitted viewer's budget", accepted.Load())
	}
}

func TestPaddingFlightUsesProvenRateAndStandingHeadroom(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.probeRate.Store(256_000)
	p.probeRateAt.Store(now.UnixNano())
	p.minimumFeedbackRTT.Store(int64(80 * time.Millisecond))
	p.departedBytes.Store(12_000)
	if !p.flightLimited(10_000_000, true) {
		t.Fatal("padding borrowed an unproven offered ceiling")
	}
	p.departedBytes.Store(4_000)
	if p.flightLimited(10_000_000, true) {
		t.Fatal("padding cannot exercise a proven flight window")
	}
	p.networkExcess.Store(int64(standingDelayLimit - 20*time.Millisecond))
	if !p.flightLimited(10_000_000, true) {
		t.Fatal("padding exceeded remaining standing-delay headroom")
	}
	p.networkExcess.Store(int64(standingDelayLimit))
	p.departedBytes.Store(0)
	if !p.flightLimited(10_000_000, true) {
		t.Fatal("padding continued at the standing cap")
	}
}

func TestReplacementConnectionCannotSpendTheJoinExemption(t *testing.T) {
	now := time.Now()
	old := &Peer{nativeKeyOwner: new(atomic.Int64)}
	if !old.takeNativeKey(now) {
		t.Fatal("initial request denied")
	}
	r := &Room{}
	next := &Peer{Room: r}
	next.InheritPath(old)
	next.viewerICEConnected(now)
	if r.PLIsSent.Load() != 0 || r.joinPending || next.refreshRequestedAt.Load() != 0 {
		t.Fatal("replacement bypassed the shared non-join budget")
	}
}

func TestDeniedRecoveryCannotChangeAnAdmittedRenditionTarget(t *testing.T) {
	p := &Peer{Room: &Room{}}
	p.refreshEncoding.Store(1)
	p.nativeKeyAt.Store(time.Now().UnixNano())
	for range 100 {
		// Explicit viewer PLI/FIR and data-channel recovery use this path;
		// rendition upgrades and decoder-stall recovery also enter For.
		p.requestKeyframe()
		p.requestKeyframeFor(false)
	}
	if p.refreshEncoding.Load() != 1 || p.refreshRequestedAt.Load() != 0 || p.Room.PLIsSent.Load() != 0 {
		t.Fatal("a denied request changed admission or refreshed the publisher")
	}
	p.requestKeyframeFor(true)
	if p.refreshEncoding.Load() != 2 {
		t.Fatal("a spent native budget starved independent low recovery")
	}
}

func TestFreshDelayClockDoesNotRegressRepeatedOldReceiverReports(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 2_000_000}
	for i := 0; i < 20; i++ {
		at := now.Add(time.Duration(i) * 100 * time.Millisecond)
		// Receiver RTT changes in coarse steps; the actual newer feedback
		// shows unchanged propagation and sufficient delivered capacity.
		c.step(at, rateObservation{reports: uint64(i + 1), delayAt: at, delay: 10 * time.Millisecond, excess: time.Duration(i/5) * 60 * time.Millisecond, received: 2_000_000, mediaDemand: 1_000_000, sentRate: 1_000_000, required: 1_200_000, demand: 2_000_000, productive: true})
	}
	if !c.cutAt.IsZero() || c.trend.slope() > delaySlopeLimit {
		t.Fatal("stale receiver steps invented a growing feedback queue")
	}
}
func TestHeartbeatWorkIsBoundedPerPeer(t *testing.T) {
	now := time.Unix(100, 0)
	p := &Peer{}
	n := 0
	for i := 0; i < 10000; i++ {
		if p.acceptHeartbeat(now.Add(time.Duration(i) * time.Microsecond)) {
			n++
		}
	}
	if n != 2 || !p.acceptHeartbeat(now.Add(200*time.Millisecond)) {
		t.Fatal("heartbeat response work unbounded", n)
	}
}
func TestKeyEnvelopeHasNoEncodedSizeBoundary(t *testing.T) {
	for _, size := range []int64{1000, 28000, 99999, 100000, 100001, 150000, 300000, 600000} {
		rate := size * 8
		if nativeRefreshTooSlow(size, rate) || !nativeUpgradeFits(size, rate, rate) || nativeUpgradeFits(size+1, rate, rate) || !nativeRefreshTooSlow(size+1, rate) {
			t.Fatal("serialization boundary", size)
		}
	}
}

func TestWireCreditAndFlightShareOneRateEnvelope(t *testing.T) {
	for _, rate := range []int64{128000, 256000, 1000000, 3000000, 10000000} {
		for _, rtt := range []time.Duration{time.Millisecond, 40 * time.Millisecond, 160 * time.Millisecond, 800 * time.Millisecond} {
			for _, low := range []bool{false, true} {
				p := &Pacer{}
				p.rate.Store(rate * 10)
				p.effective.Store(rate * 10)
				p.probeRate.Store(rate)
				p.probeRateAt.Store(time.Now().UnixNano())
				p.minimumFeedbackRTT.Store(int64(rtt))
				p.acknowledgedBytes.Store(1)
				p.boundFlight.Store(true)
				p.thumbnail.Store(low)
				if actual := p.wireRate(time.Now()); actual > max(128000, rate*110/100) {
					t.Fatal("unused ceiling escaped video proof", rate, actual)
				}
				limit := max(int64(16*wireMTU), int64(float64(rate)*(rtt+100*time.Millisecond).Seconds()/8))
				p.departedBytes.Store(limit)
				if p.flightLimited(rate, false) {
					t.Fatal("physical BDP was lost", rate, rtt, low)
				}
				p.departedBytes.Store(limit + 1)
				if !p.flightLimited(rate, false) {
					t.Fatal("flight exceeded its proven-rate envelope", rate, rtt, low)
				}
				now := time.Now()
				bucket := wireBudget{last: now}
				bucket.accrue(now.Add(time.Hour), rate)
				if bucket.credit > float64(wireBurstBytes(rate)) {
					t.Fatal("idle time created unbounded credit")
				}
			}
		}
	}
}
func TestRandomRepairedLossDoesNotCutAStableProductivePath(t *testing.T) {
	for _, raw := range []float64{.01, .05, .1, .2, .3} {
		now := time.Unix(100, 0)
		c := wireController{rate: 2000000}
		for i := 0; i < 300; i++ {
			c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{reports: uint64(i + 1), loss: raw, received: 2000000, demand: 2000000, required: 2000000, productive: true, full: true})
		}
		if c.rate != 2000000 {
			t.Fatal("repaired raw loss was confused with a queue", raw, c.rate)
		}
	}
}
func TestPaddingStopsWhenItsByteAllowanceIsSpent(t *testing.T) {
	p := &Pacer{writers: map[uint32]interceptor.RTPWriter{1: nil}}
	p.twccID.Store(1)
	p.mediaPT.Store(96)
	p.probeUntil.Store(time.Now().Add(time.Second).UnixNano())
	p.wireBytes.Store(1000000)
	p.paddingBytes.Store(500000)
	if _, ok := p.paddingPacket(time.Now()); ok {
		t.Fatal("low video can spend unlimited padding")
	}
}

func TestDiscoveryAllowanceCanCompleteOneMeasuredTrial(t *testing.T) {
	p := &Pacer{writers: map[uint32]interceptor.RTPWriter{1: nil}}
	p.twccID.Store(1)
	p.mediaPT.Store(96)
	p.probeUntil.Store(time.Now().Add(time.Second).UnixNano())
	p.probeCeiling.Store(6_000_000)
	p.videoWireBytes.Store(600_000)
	p.paddingBytes.Store(500_000)
	if _, ok := p.paddingPacket(time.Now()); !ok {
		t.Fatal("a fixed byte cap prevented one complete native discovery trial")
	}
	p.videoWireBytes.Store(800_000)
	p.paddingBytes.Store(700_000)
	if _, ok := p.paddingPacket(time.Now()); ok {
		t.Fatal("discovery spent more than one trial plus earned video credit")
	}
}
func TestOwnedRepairCacheDoesNotCopyImmutablePayload(t *testing.T) {
	var cache packetCache
	packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: 7}, Payload: []byte{1, 2, 3}}
	cache.putOwnedFor(packet, time.Now(), time.Second)
	if &cache.entries[7].p.Payload[0] != &packet.Payload[0] {
		t.Fatal("cache copied an already owned immutable payload")
	}
}
func TestAudioFeedbackCannotCreateVideoCapacityProof(t *testing.T) {
	p := &Pacer{}
	now := time.Now()
	for i := 0; i < 4; i++ {
		slot := &p.departures[i]
		slot.sequence.Store(uint32(i + 1))
		slot.at.Store(now.Add(-time.Duration(400-i*100) * time.Millisecond).UnixNano())
		slot.bytes.Store(1200)
		slot.kind.Store(uint32(audioKind))
		slot.total.Store(int64((i + 1) * 1200))
	}
	feedback := &rtcp.TransportLayerCC{PacketStatusCount: 4, PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 4, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: 1000}, {Delta: 100000}, {Delta: 100000}, {Delta: 100000}}}
	p.FeedbackRTT(feedback, now)
	if p.receiptRate.Load() != 0 || p.sustainedRate.Load() != 0 || p.probeRate.Load() != 0 || p.transitReports.Load() != 0 {
		t.Fatal("audio inflated video delivery proof")
	}
	if p.acknowledgedBytes.Load() != 4800 {
		t.Fatal("audio stopped charging the shared wire budget")
	}
}

func TestReceiverTransitSeparatesQueueGrowthFromFeedbackBatching(t *testing.T) {
	for _, queueRamp := range []bool{false, true} {
		p := &Pacer{}
		var trend delayTrend
		for i := 0; i < 20; i++ {
			sent := time.Unix(100, 0).Add(time.Duration(i) * 100 * time.Millisecond)
			queue := time.Duration(0)
			if queueRamp {
				queue = time.Duration(i) * 5 * time.Millisecond
			}
			arrival := 200*time.Second + time.Duration(i)*100*time.Millisecond + queue
			reference := arrival / (64 * time.Millisecond)
			slot := &p.departures[i]
			slot.sequence.Store(uint32(i + 1))
			slot.at.Store(sent.UnixNano())
			slot.bytes.Store(1200)
			slot.total.Store(int64((i + 1) * 1200))
			slot.kind.Store(uint32(videoKind))
			f := &rtcp.TransportLayerCC{BaseSequenceNumber: uint16(i), PacketStatusCount: 1, ReferenceTime: uint32(reference), PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 1, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: int64((arrival - time.Duration(reference)*64*time.Millisecond) / time.Microsecond)}}}
			// Large reverse feedback scheduling changes must not appear in
			// forward transit; arrival/departure clocks carry the actual ramp.
			p.FeedbackRTT(f, sent.Add(80*time.Millisecond+time.Duration(i%2)*80*time.Millisecond))
			trend.observe(time.Unix(0, p.transitAt.Load()), time.Duration(p.transitDelay.Load()))
		}
		want := 0.0
		if queueRamp {
			want = 50
		}
		if slope := trend.slope(); slope < want-.1 || slope > want+.1 {
			t.Fatal("receiver transit mixed forward growth and feedback scheduling", queueRamp, slope)
		}
	}
}

func BenchmarkRepairCacheOwnership(b *testing.B) {
	for _, owned := range []bool{false, true} {
		name := "defensive-copy"
		if owned {
			name = "owned-video"
		}
		b.Run(name, func(b *testing.B) {
			packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: 7}, Payload: make([]byte, 1200)}
			var c packetCache
			now := time.Now()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if owned {
					c.putOwnedFor(packet, now, time.Second)
				} else {
					c.putFor(packet, now, time.Second)
				}
			}
		})
	}
}

func TestIdleCeilingCanBeExercisedBelowTheStandingCap(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 128000}
	for i := 0; i < 30; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{reports: uint64(i + 1), excess: time.Duration(100+i*3) * time.Millisecond, sentRate: 80000, received: 90000, demand: 250000, required: 220000, mediaDemand: 180000, productive: true})
	}
	if c.rate < 220000 {
		t.Fatal("a sparse decoder could never exercise its unused ceiling", c.rate)
	}
	if c.proof >= 180000 {
		t.Fatal("a numerical probe was confused with actual received capacity")
	}
}
func TestAdmittedFullPictureIsRecordedAfterATemporalDownshift(t *testing.T) {
	p := &Pacer{}
	p.SetTemporal(2)
	p.SetTemporal(1)
	p.recordFullFrame(42, true)
	if frames := p.FullLayerFrames(); len(frames) != 1 || frames[0] != 42 {
		t.Fatal("a complete previously admitted TID2 picture disappeared from delivered proof")
	}
	p.SetThumbnail(true)
	p.recordFullFrame(43, true)
	if len(p.FullLayerFrames()) != 0 {
		t.Fatal("a low encoding claimed native proof")
	}
}

func TestPublisherReplacementClearsAndFencesKeySize(t *testing.T) {
	engine, err := New("127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	old, err := engine.Peer("replace", true)
	if err != nil {
		t.Fatal(err)
	}
	old.Room.keyBytes.Store(300000)
	current, err := engine.Peer("replace", true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Room.keyBytes.Load() != 0 {
		t.Fatal("publisher replacement retained key cost")
	}
	old.distribute(frame{key: true, packets: []*rtp.Packet{{Header: rtp.Header{SSRC: 1, Marker: true}, Payload: []byte{0x0c, 0}}}})
	if current.Room.keyBytes.Load() != 0 {
		t.Fatal("late old-publisher key restored the fenced cost")
	}
}

func TestReceivedTrialBridgesDecoderSamplingWithoutGrowingQueue(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 220000, proof: 220000}
	for i := 0; i < 20; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{reports: uint64(i + 1), trial: true, productive: true, sentRate: 220000, received: 220000, required: 220000, mediaDemand: 180000, demand: 220000, excess: 200 * time.Millisecond})
	}
	if c.rate < 220000 {
		t.Fatal("unchanged standing delay discarded delivered media or latched backoff", c.rate)
	}
	for i := 20; i < 40; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{reports: uint64(i + 1), trial: true, productive: true, sentRate: 220000, received: 220000, required: 220000, mediaDemand: 180000, demand: 220000, excess: standingDelayLimit + 100*time.Millisecond})
	}
	if c.rate >= 220000 {
		t.Fatal("a successful trial bypassed the standing cap")
	}
}

func TestFlexibleEnhancementLossPreservesIndependentNativeBase(t *testing.T) {
	p := &Pacer{}
	p.pictures.mark(100, true)
	p.lostPicture(pacedPacket{tid: 1, flexible: true, pictureKnown: true, picture: 101})
	if p.waitKey {
		t.Fatal("flexible enhancement requested a native refresh")
	}
	if !p.allowTemporalPicture(pacedPacket{tid: 0, flexible: true, picture: 102, references: [3]uint8{2}, referenceCount: 1}) {
		t.Fatal("independent base was lost")
	}
	if p.allowTemporalPicture(pacedPacket{tid: 1, flexible: true, picture: 103, references: [3]uint8{2}, referenceCount: 1}) {
		t.Fatal("dependent enhancement crossed missing reference")
	}
	p.pictures.mark(102, false)
	if !p.allowTemporalPicture(pacedPacket{tid: 1, flexible: true, picture: 104, references: [3]uint8{2}, referenceCount: 1}) {
		t.Fatal("explicit base reference could not resume enhancement")
	}
}
func TestFlexiblePictureProofCannotAliasAnOldWrap(t *testing.T) {
	for _, mask := range []uint16{127, 32767} {
		var h pictureHistory
		for i := 0; i <= int(mask); i++ {
			number := h.sequence(uint16(i), mask)
			h.mark(number, false)
		}
		missing := h.sequence(0, mask)
		next := h.sequence(1, mask)
		if missing != uint64(mask)+1 || h.contains(next, [3]uint8{1}, 1) {
			t.Fatal("missing current picture borrowed old-wrap proof", mask, missing, next)
		}
	}
}
func TestFlexiblePictureTailRetainsFirstPacketLayerMetadata(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mediaPT.Store(98)
	p.SetTargetBitrate(10000000)
	p.SetTemporal(2)
	p.mu.Lock()
	p.pictures.mark(0, true)
	p.mu.Unlock()
	sent := make(chan struct{}, 2)
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, a interceptor.Attributes) (int, error) {
		sent <- struct{}{}
		return len(b), nil
	}))
	p.Write(&rtp.Header{Version: 2, SSRC: 1, PayloadType: 98, Timestamp: 99}, []byte{0xf8, 0x80, 2, 0x50, 4, 0xaa}, nil)
	p.Write(&rtp.Header{Version: 2, SSRC: 1, PayloadType: 98, Timestamp: 99, SequenceNumber: 1, Marker: true}, []byte{0xd4, 0x80, 2, 4, 0xaa}, nil)
	for range 2 {
		select {
		case <-sent:
		case <-time.After(time.Second):
			t.Fatal("flexible picture did not complete")
		}
	}
	deadline := time.Now().Add(time.Second)
	for len(p.FullLayerFrames()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if frames := p.FullLayerFrames(); len(frames) != 1 || frames[0] != 99 {
		t.Fatal("tail lost the actual TID2 admission", frames)
	}
}

func TestReferenceBurstUsesSerializationEnvelopeWithoutBeingAKey(t *testing.T) {
	for _, packets := range []int{2, 20, 80, 120} {
		p := &Pacer{}
		p.mediaPT.Store(98)
		p.rate.Store(1_000_000)
		for i := range packets {
			payload := make([]byte, 1200)
			if i == 0 {
				copy(payload, []byte{0xf8, 0x80, 2, 0x10, 4})
			} else {
				copy(payload, []byte{0xd0, 0x80, 2, 4})
			}
			if i == packets-1 {
				payload[0] |= 4
			}
			p.Write(&rtp.Header{Version: 2, SSRC: 1, PayloadType: 98, Timestamp: 99, SequenceNumber: uint16(i), Marker: i == packets-1}, payload, nil)
		}
		want := packets == 20 || packets == 80
		if (p.graceBytes.Load() > 0) != want || p.keyframeBytes.Load() != 0 {
			t.Fatal("reference burst was confused with a key or admitted beyond its envelope", packets, p.graceBytes.Load())
		}
	}
}

func TestPaddingDiscoveryCannotAccelerateAnAdmittedNativeKey(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mediaPT.Store(98)
	p.rate.Store(10_000_000)
	p.effective.Store(10_000_000)
	p.probeRate.Store(1_000_000)
	p.probeRateAt.Store(time.Now().UnixNano())
	p.probeCeiling.Store(10_000_000)
	p.startTrial(time.Now().Add(time.Second).UnixNano())
	p.smoothWire.Store(true)
	var bytes atomic.Uint64
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		bytes.Add(uint64(h.MarshalSize() + len(payload) + wireOverhead))
		return len(payload), nil
	}))
	start := time.Now()
	for i := range 30 {
		payload := make([]byte, 1200)
		payload[0], payload[1], payload[2] = 0x80, 0x80, 1
		if i == 0 {
			payload[0] |= 8
		}
		if i == 29 {
			payload[0] |= 4
		}
		p.Write(&rtp.Header{Version: 2, SSRC: 1, PayloadType: 98, Timestamp: 99, SequenceNumber: uint16(i), Marker: i == 29}, payload, nil)
	}
	time.Sleep(120 * time.Millisecond)
	maximum := uint64(1_100_000*time.Since(start).Seconds()/8) + 2*wireMTU
	if got := bytes.Load(); got == 0 || got > maximum {
		t.Fatal("padding's exploratory rate leaked into encoded video pacing", got, maximum)
	}
}

func TestFlightSupportsProductionRateWithoutBorrowingCongestedRTT(t *testing.T) {
	p := &Pacer{}
	p.boundFlight.Store(true)
	p.minimumFeedbackRTT.Store(int64(time.Millisecond))
	p.sustainedRate.Store(4_000_000)
	p.sustainedRateAt.Store(time.Now().UnixNano())
	p.acknowledgedBytes.Store(1)
	p.departedBytes.Store(40_001)
	if p.flightLimited(4_000_000, false) {
		t.Fatal("a fixed byte cap prevented production-rate playback")
	}
	p.rtt.Store(int64(2 * time.Second))
	p.departedBytes.Store(100_001)
	if !p.flightLimited(4_000_000, false) {
		t.Fatal("congestion inflated the physical flight window")
	}
}

func TestAdmittedKeyUsesProvenCapacityAboveSteadyDemand(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mediaPT.Store(98)
	p.twccID.Store(1)
	p.effective.Store(8_000_000)
	p.probeRate.Store(8_000_000)
	p.probeRateAt.Store(time.Now().UnixNano())
	p.flightDemand.Store(2_000_000)
	p.wireReservation.Store(2_000_000)
	p.boundFlight.Store(true)
	p.minimumFeedbackRTT.Store(int64(25 * time.Millisecond))
	p.smoothWire.Store(true)
	done := make(chan struct{})
	count := 0
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, a interceptor.Attributes) (int, error) {
		count++
		// A batch represents roughly 100ms at the proven rate. A flight
		// capped at steady demand cannot even reach its next feedback batch.
		if count%80 == 0 {
			p.acknowledgedBytes.Store(p.departedBytes.Load())
		}
		if count == 250 {
			close(done)
		}
		return len(b), nil
	}))
	for i := range 250 {
		payload := make([]byte, 1200)
		payload[0], payload[1], payload[2] = 0x80, 0x80, 1
		if i == 0 {
			payload[0] |= 8
		}
		if i == 249 {
			payload[0] |= 4
		}
		p.Write(&rtp.Header{Version: 2, SSRC: 1, PayloadType: 98, Timestamp: 99, SequenceNumber: uint16(i), Marker: i == 249}, payload, nil)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an admitted key could not use its receiver-proven reservation")
	}
}
