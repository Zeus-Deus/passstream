package sfu

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"testing"
	"time"
)

func TestCurrentDecoderSampleProvesATemporalUpgradeWithoutSmoothingLag(t *testing.T) {
	p := &Peer{}
	now := time.Now()
	p.reportDecoded(now, 1)
	p.reportDecoded(now.Add(500*time.Millisecond), 3)
	p.reportDecoded(now.Add(time.Second), 10)
	if p.decodedFPS.Load() != 8000 || p.decoderRate() != 14000 {
		t.Fatal("a complete current window inherited the old temporal rate")
	}
	p.reportDecoded(now.Add(1500*time.Millisecond), 11)
	if p.decoderRate() >= 10000 {
		t.Fatal("a later slow sample kept the upgrade's old fast rate")
	}
}

func TestInputBurstLosingTID1KeepsIndependentBaseReferences(t *testing.T) {
	r := &Room{}
	p := &Peer{Room: r}
	r.publisher = p
	v := &Peer{frames: make(chan frame, 1)}
	r.viewers = map[*Peer]bool{v: true}
	p.distribute(frame{tid: 2})
	p.distribute(frame{tid: 1, temporalRecovery: true})
	if v.Epoch.Load() != 0 || v.temporalEpoch.Load() != 1 {
		t.Fatal("a disposable temporal-reference loss demanded a room keyframe")
	}
	p.distribute(frame{tid: 0})
	if v.Epoch.Load() != 1 {
		t.Fatal("an actual base-reference loss was ignored")
	}
}

func TestReturningAuthenticatedFeedbackResumesMediaBeforeConsentTick(t *testing.T) {
	now := time.Now()
	for _, state := range []webrtc.PeerConnectionState{webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed} {
		if mediaTransportReady(state, now, now.UnixNano()) {
			t.Fatal("unestablished or failed transport resumed", state)
		}
	}
	if !mediaTransportReady(webrtc.PeerConnectionStateConnected, now, 0) || !mediaTransportReady(webrtc.PeerConnectionStateDisconnected, now, now.Add(-100*time.Millisecond).UnixNano()) || mediaTransportReady(webrtc.PeerConnectionStateDisconnected, now, now.Add(-time.Second).UnixNano()) {
		t.Fatal("returning feedback waited for ICE consent or stale feedback reopened media")
	}
}

func TestRecoveryKeysCanObtainFeedbackOnAnEstablishedDisconnectedPath(t *testing.T) {
	for _, state := range []webrtc.PeerConnectionState{webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting, webrtc.PeerConnectionStateConnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed} {
		if recoveryKeyframeReady(state, true) || recoveryKeyframeReady(state, false) {
			t.Fatal("recovery-key exception escaped its established disconnected path", state)
		}
	}
	if !recoveryKeyframeReady(webrtc.PeerConnectionStateDisconnected, true) || recoveryKeyframeReady(webrtc.PeerConnectionStateDisconnected, false) {
		t.Fatal("recovery key waited for consent or bypassed an incomplete handshake")
	}
}

func TestMeasuredRoomRateSeedsEnoughHeadroomForANativeKeyframe(t *testing.T) {
	r := &Room{}
	r.hasLow.Store(true)
	r.keyBytes.Store(250_000)
	r.tidRate[0].Store(1_000_000)
	r.tidRate[1].Store(400_000)
	r.tidRate[2].Store(600_000)
	if got := r.startingWireRate(); got != 6_000_000 {
		t.Fatal("measured stream was restarted at an unrelated slow-start ceiling", got)
	}
}

func TestWireCreditScalesWithBudget(t *testing.T) {
	for _, tc := range []struct{ rate, want int64 }{{64_000, 6000}, {256_000, 6000}, {512_000, 6400}, {2_000_000, 25000}} {
		now := time.Now()
		b := wireBudget{last: now}
		b.accrue(now.Add(time.Hour), tc.rate)
		if b.credit != float64(tc.want) {
			t.Fatalf("%d bit/s credit %g want %d", tc.rate, b.credit, tc.want)
		}
	}
}

func TestQueueLossPacingSpreadsPacketsDespiteAvailableBurstCredit(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.SetTargetBitrate(128_000)
	p.mediaPT.Store(98)
	p.smoothWire.Store(true)
	sent := make(chan time.Time, 3)
	p.AddStream(1, interceptor.RTPWriterFunc(func(_ *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
		sent <- time.Now()
		return len(payload), nil
	}))
	for i, flags := range []byte{8, 0, 4} {
		payload := make([]byte, 500)
		payload[0] = flags
		p.Write(&rtp.Header{SSRC: 1, PayloadType: 98, Timestamp: 1, Marker: i == 2}, payload, nil)
	}
	var previous time.Time
	for i := 0; i < 3; i++ {
		select {
		case at := <-sent:
			if !previous.IsZero() && at.Sub(previous) < 10*time.Millisecond {
				t.Fatal("four-MTU credit was dumped into the managed queue", at.Sub(previous))
			}
			previous = at
		case <-time.After(2 * time.Second):
			t.Fatal("packet spacing starved a complete frame")
		}
	}
}

func TestNativeTrialCeilingLimitsOnlyTheActiveLowRenditionProbe(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.rate.Store(1_000_000)
	p.effective.Store(500_000)
	p.probeRate.Store(500_000)
	p.probeRateAt.Store(now.UnixNano())
	p.probeCeiling.Store(250_000)
	p.startTrial(now.Add(time.Second).UnixNano())
	p.thumbnail.Store(true)
	if p.wireRate(now) != 250_000 || p.wireRate(now.Add(time.Second)) != 500_000 {
		t.Fatal("wire trial ignored its target or leaked beyond its deadline")
	}
	p.thumbnail.Store(false)
	if p.wireRate(now) != 250_000 || p.wireRate(now.Add(time.Second)) != 500_000 {
		t.Fatal("native media bypassed an active wire trial or retained its expired ceiling")
	}
}

func TestSmallNativeFallbackWaitsForSustainedBaseRatePressure(t *testing.T) {
	now := time.Now()
	var f renditionFallback
	if f.update(now, false, true) || f.update(now.Add(500*time.Millisecond), false, true) {
		t.Fatal("a short refresh transient discarded the native decoder")
	}
	f.update(now.Add(700*time.Millisecond), false, false)
	if f.update(now.Add(900*time.Millisecond), false, true) || !f.update(now.Add(1900*time.Millisecond), false, true) {
		t.Fatal("recovered or sustained pressure did not restart/complete the hold")
	}
	f = renditionFallback{}
	if f.update(now, false, true) || !f.update(now.Add(time.Second), false, true) || !f.update(now, true, true) {
		t.Fatal("large-screen or current low-rendition fallback changed")
	}
}

func TestConstrainedReplacementStartsOnTheIndependentRendition(t *testing.T) {
	r := &Room{}
	r.hasLow.Store(true)
	r.keyBytes.Store(20_000)
	previous := &Peer{Pacer: &Pacer{}}
	previous.Pacer.effective.Store(128_000)
	previous.pathLimited.Store(true)
	p := &Peer{Room: r, Pacer: &Pacer{}}
	p.InheritPath(previous)
	if !p.Low.Load() || p.Pacer.effective.Load() != 128_000 {
		t.Fatal("replacement restarted a large native key under its inherited ceiling")
	}
	previous.Pacer.effective.Store(4_000_000)
	p = &Peer{Room: r, Pacer: &Pacer{}}
	p.InheritPath(previous)
	if p.Low.Load() {
		t.Fatal("a good-path replacement unnecessarily changed rendition")
	}
}

func TestSmallNativeFlightAllowsOneFeedbackIntervalWithoutAnUnboundedTrain(t *testing.T) {
	p := &Pacer{}
	p.boundFlight.Store(true)
	p.keyframeBytes.Store(20_000)
	p.minimumFeedbackRTT.Store(int64(40 * time.Millisecond))
	p.flightDemand.Store(180_000)
	p.acknowledgedBytes.Store(1)
	p.departedBytes.Store(8_001)
	if p.flightLimited(256_000, false) {
		t.Fatal("the native refresh was starved below one feedback interval")
	}
	p.departedBytes.Store(16*wireMTU + 1)
	if !p.flightLimited(256_000, false) {
		t.Fatal("the native refresh can create an unbounded train")
	}
	p.rtt.Store(int64(time.Second))
	p.departedBytes.Store(9_001)
	if p.flightLimited(256_000, false) {
		t.Fatal("queued feedback starved native delivery")
	}
	p.departedBytes.Store(40_001)
	if !p.flightLimited(256_000, false) {
		t.Fatal("congested RTT enlarged the native flight window without a bound")
	}
}

func TestLargeKeyTrialProvesRefreshSerializationBeforeLeavingTheLowRendition(t *testing.T) {
	if nativeTrialRate(2_000_000, 250_000) != 2_400_000 || nativeTrialRate(2_000_000, 150_000) != 2_400_000 || nativeTrialRate(200_000, 20_000) != 240_000 {
		t.Fatal("native trial ignored either its refresh cost or steady headroom")
	}
	for _, bytes := range []int64{150_000, 250_000, 517_001} {
		rate := nativeRefreshRate(bytes)
		if rate < bytes*8 || rate-1 >= bytes*8 || nativeTrialRate(2_000_000, bytes) <= rate {
			t.Fatal("refresh proof rounded below its one-second budget or lacked trial headroom", bytes, rate)
		}
	}
}

func TestNativeUpgradeRechecksTheActualRefreshAgainstReceivedProof(t *testing.T) {
	if !nativeUpgradeFits(250_000, 8_000_000, 8_000_000) {
		t.Fatal("the proved keyframe did not fit")
	}
	if nativeUpgradeFits(1_100_000, 8_000_000, 8_000_000) {
		t.Fatal("a doubled incoming key borrowed proof for the smaller old key")
	}
	if nativeUpgradeFits(1_100_000, 16_000_000, 8_000_000) {
		t.Fatal("an unused numerical ceiling substituted for received capacity")
	}
	if !nativeUpgradeFits(1_100_000, 16_000_000, 16_000_000) {
		t.Fatal("a larger key could not use independently proved delivery")
	}
}

func TestFreshLossRevokesIdleCapacityEvenWithoutQueueGrowth(t *testing.T) {
	p := &Pacer{}
	now := time.Now()
	p.recordFlightCapacity(2_000_000, 20*time.Millisecond, now)
	p.mediaLossAt.Store(now.UnixNano())
	p.mediaLossPPM.Store(100_000)
	p.recordFlightCapacity(128_000, 20*time.Millisecond, now.Add(100*time.Millisecond))
	if p.flightCapacity(now.Add(100*time.Millisecond)) != 128_000 {
		t.Fatal("a managed queue's losses retained old unused capacity")
	}
}

func TestPendingRenditionRequestsCoalesceButReturningTrafficBypassesThem(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1}
	p := &Peer{Room: r}
	p.Low.Store(true)
	p.rtcpAt.Store(time.Now().UnixNano())
	p.requestKeyframeFor(false)
	if r.PLIsSent.Load() != 1 {
		t.Fatal("initial native refresh was not requested")
	}
	r.mu.Lock()
	r.pliAt = time.Now().Add(-time.Second)
	r.pliBusy = false
	r.mu.Unlock()
	p.requestKeyframeFor(false)
	if r.PLIsSent.Load() != 1 {
		t.Fatal("a pending rendition repeatedly refreshed the publisher")
	}
	p.pliGapUntil.Store(time.Now().Add(time.Second).UnixNano())
	p.requestKeyframeFor(false)
	if r.PLIsSent.Load() != 1 {
		t.Fatal("a returning transport bypassed its native-key budget")
	}
}

func TestSmallNativeDeltaDoesNotExpireWaitingForEarlierFeedback(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.boundFlight.Store(true)
	p.keyframeBytes.Store(20_000)
	p.minimumFeedbackRTT.Store(int64(40 * time.Millisecond))
	p.acknowledgedBytes.Store(1)
	p.departedBytes.Store(100_000)
	if !p.flightLimited(256_000, false) {
		t.Fatal("test did not fill the outstanding-byte window")
	}
	sent := make(chan struct{}, 1)
	p.AddStream(1, interceptor.RTPWriterFunc(func(_ *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) {
		sent <- struct{}{}
		return len(b), nil
	}))
	p.Write(&rtp.Header{Version: 2, SSRC: 1, PayloadType: 98, Marker: true}, []byte{0x48, 0}, nil)
	select {
	case <-sent:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("a steady delta waited beyond its own queue deadline")
	}
}
func TestLivenessGapBypassesPacedKeyframeHoldoff(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1}
	p := &Peer{Room: r, Pacer: &Pacer{}}
	p.Pacer.pliAfter.Store(time.Now().Add(5 * time.Second).UnixNano())
	p.rtcpAt.Store(time.Now().UnixNano())
	p.requestKeyframe()
	if r.PLIsSent.Load() != 0 {
		t.Fatal("healthy viewer bypassed holdoff")
	}
	p.rtcpAt.Store(time.Now().Add(-2 * time.Second).UnixNano())
	p.requestKeyframe()
	if r.PLIsSent.Load() != 1 {
		t.Fatal("liveness gap inherited outage keyframe holdoff")
	}
}

func TestReturningRTCPRequestsAKeyframeBeforeTheNextHeartbeat(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1}
	p := &Peer{Room: r, Pacer: &Pacer{}}
	p.Sent.Store(1)
	p.Pacer.pliAfter.Store(now.Add(5 * time.Second).UnixNano())
	p.rtcpAt.Store(now.Add(-2 * time.Second).UnixNano())
	p.noteRTCP(now)
	if r.PLIsSent.Load() != 1 || !p.livenessGap(now) {
		t.Fatal("returning feedback waited for heartbeat or inherited an old refresh holdoff")
	}
	p.noteRTCP(now.Add(100 * time.Millisecond))
	if r.PLIsSent.Load() != 1 {
		t.Fatal("ordinary feedback repeatedly requested a keyframe")
	}
}
func TestPLIHoldoffEndsAfterSerializationAndTwoRTTs(t *testing.T) {
	p := &Pacer{done: make(chan struct{}), wake: make(chan struct{}, 1)}
	defer p.Close()
	p.SetTargetBitrate(256_000)
	p.mediaPT.Store(98)
	p.rtt.Store(int64(40 * time.Millisecond))
	payload := make([]byte, 20_000)
	payload[0] = 8
	now := time.Now()
	p.Write(&rtp.Header{PayloadType: 98, Marker: true}, payload, nil)
	hold := time.Unix(0, p.pliAfter.Load()).Sub(now)
	if hold < 700*time.Millisecond || hold > 750*time.Millisecond {
		t.Fatalf("holdoff %s includes excessive repair grace", hold)
	}
}
func TestJoinPLIsAreCoalescedForHalfASecond(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1, pliAt: time.Now().Add(-time.Second)}
	r.pli(true)
	for i := 0; i < 16; i++ {
		r.pli(true)
	}
	if r.PLIsSent.Load() != 1 {
		t.Fatal("join burst emitted multiple keyframes")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.joinPending || r.joinTimer == nil {
		t.Fatal("pending joiners have no coalesced retry")
	}
	r.joinTimer.Stop()
}
func TestDiscardBehindRepairDoesNotAllocate(t *testing.T) {
	packets := make([]pacedPacket, 3)
	p := &Pacer{}
	if got := testing.AllocsPerRun(100, func() {
		packets[0] = pacedPacket{repair: true}
		packets[1] = pacedPacket{h: rtp.Header{Timestamp: 1}, tid: 2, end: true}
		packets[2] = pacedPacket{h: rtp.Header{Timestamp: 2}, tid: 2, end: true}
		p.queue = packets[:3]
		p.bytes = 36
		p.discardFrameAt(1)
	}); got != 0 {
		t.Fatal("expiry behind repair allocated", got)
	}
}

func TestPacedMiddleLayerExpiryKeepsBasesAndWaitsForASwitchingPoint(t *testing.T) {
	drops := 0
	p := &Pacer{OnDrop: func() { drops++ }}
	p.queue = []pacedPacket{
		{repair: true},
		{h: rtp.Header{Timestamp: 1}, tid: 1, temporalRecovery: true, end: true},
		{h: rtp.Header{Timestamp: 2}, tid: 0, temporalRecovery: true, end: true},
	}
	p.bytes = 36
	p.discardFrameAt(1)
	if p.waitKey || drops != 0 || !p.waitTemporal || len(p.queue) != 2 {
		t.Fatal("recoverable enhancement expiry discarded independent base references")
	}
	if !p.allowTemporalPicture(p.queue[1]) || !p.waitTemporal {
		t.Fatal("base playback stopped or prematurely resumed enhancements")
	}
	for _, x := range []pacedPacket{{tid: 1, temporalRecovery: true}, {tid: 1, temporalRecovery: true, up: true}, {tid: 2, temporalRecovery: true, up: true}, {tid: 1, up: true}} {
		if p.allowTemporalPicture(x) {
			t.Fatal("a dependent or unrecognized picture resumed enhancement playback")
		}
	}
	if !p.allowTemporalPicture(pacedPacket{tid: 0, temporalRecovery: true, up: true}) || p.waitTemporal {
		t.Fatal("a retained switching point failed to recover enhancement playback")
	}
	p.lostPicture(pacedPacket{tid: 0, temporalRecovery: true})
	if !p.waitKey || drops != 1 {
		t.Fatal("an actual base loss did not request a keyframe")
	}
}

func TestUnrecognizedPacedMiddleReferencesStillRequireAKey(t *testing.T) {
	p := &Pacer{}
	p.lostPicture(pacedPacket{tid: 1})
	if !p.waitKey || p.waitTemporal {
		t.Fatal("unknown dependency structures received L1T3 recovery semantics")
	}
}

func TestZeroDecoderCounterDoesNotConfirmAStall(t *testing.T) {
	p := &Peer{}
	now := time.Now()
	p.reportDecoded(now, 0)
	if p.decoderSupported.Load() || p.decoderReported.Load() || p.freshAt.Load() != 0 {
		t.Fatal("zero counter claimed decoder feedback")
	}
	p.reportDecoded(now, 1)
	p.reportDecoded(now.Add(time.Second), 1)
	if !p.decoderReported.Load() || p.freshAt.Load() != now.UnixNano() {
		t.Fatal("unchanged counter refreshed decoder liveness")
	}
	p.reportDecoded(now.Add(2*time.Second), 2)
	if p.freshAt.Load() != now.Add(2*time.Second).UnixNano() {
		t.Fatal("advancing counter did not refresh decoder liveness")
	}
}

func TestReplacementInheritsThisViewersPathBudget(t *testing.T) {
	old := &Peer{Pacer: &Pacer{}}
	old.Pacer.effective.Store(180_000)
	old.pathLimited.Store(true)
	next := &Peer{Pacer: &Pacer{}}
	next.Pacer.effective.Store(2_000_000)
	next.InheritPath(old)
	if next.Pacer.effective.Load() != 180_000 || !next.pathLimited.Load() {
		t.Fatal("replacement reset a constrained path")
	}
}

func TestBudgetDoesNotDoubleCountCongestionAsAnotherLayerCut(t *testing.T) {
	if got := admittedLayer([3]int{50_000, 50_000, 100_000}, 300_000, 0); got != 2 {
		t.Fatal("affordable layers were cut twice", got)
	}
}

func TestFeedbackGapBypassIsBriefAndNotExtendedByHealthyFeedback(t *testing.T) {
	p := &Peer{}
	now := time.Now()
	p.rtcpAt.Store(now.Add(-2 * time.Second).UnixNano())
	p.noteRTCP(now)
	if !p.livenessGap(now.Add(100 * time.Millisecond)) {
		t.Fatal("PLI lost evidence when feedback returned")
	}
	p.noteRTCP(now.Add(100 * time.Millisecond))
	if p.livenessGap(now.Add(600 * time.Millisecond)) {
		t.Fatal("healthy feedback extended outage bypass")
	}
	p.decoderReported.Store(true)
	p.freshAt.Store(now.Add(-3 * time.Second).UnixNano())
	if p.livenessGap(now.Add(600 * time.Millisecond)) {
		t.Fatal("paced decoding stall mistaken for transport outage")
	}
}

func TestFirstMediaRateHasNoInventedZeroHistory(t *testing.T) {
	if got := smoothMediaRate(0, 400_000); got != 400_000 {
		t.Fatal("startup underreported source demand", got)
	}
}

func TestFullLayerEvidenceIsBoundedAndClearedOnDownswitch(t *testing.T) {
	p := &Pacer{}
	p.SetTemporal(2)
	for i := 0; i < 20; i++ {
		p.recordFullFrame(uint32(i))
	}
	got := p.FullLayerFrames()
	if len(got) != 16 || got[0] != 4 || got[15] != 19 {
		t.Fatal("wire timestamp evidence lost its bound or order", got)
	}
	p.SetTemporal(1)
	p.recordFullFrame(21)
	if len(p.FullLayerFrames()) != 0 {
		t.Fatal("lower layer kept stale full-layer evidence")
	}
	p.SetTemporal(2)
	p.recordFullFrame(22)
	if got := p.FullLayerFrames(); len(got) != 1 || got[0] != 22 {
		t.Fatal("upswitch reused old evidence", got)
	}
}

func TestPacedKeyframeRepairOutlivesItsSerializationButRemainsBounded(t *testing.T) {
	now := time.Now()
	var c packetCache
	c.keyframe(10)
	c.putFor(&rtp.Packet{Header: rtp.Header{SequenceNumber: 10}}, now, maxKeyframeCatchup)
	if packet, deadline := c.replaySpaced(10, now.Add(2*time.Second), time.Millisecond); packet == nil || !deadline.Equal(now.Add(maxKeyframeCatchup)) {
		t.Fatal("keyframe expired before a paced picture could decode")
	}
	if packet, _ := c.replaySpaced(10, now.Add(7*time.Second), time.Millisecond); packet != nil {
		t.Fatal("unbounded old-key replay")
	}
	c.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 11}}, now)
	if c.get(11, now.Add(2*time.Second)) != nil {
		t.Fatal("ordinary replay inherited keyframe lifetime")
	}
	c.keyframe(12)
	if c.get(10, now.Add(3*time.Second)) != nil {
		t.Fatal("superseded keyframe remained replayable")
	}
}

func TestReplayGetsAFreshTransportSequenceAtTheWire(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.twccID.Store(3)
	sent := make(chan [2]uint16, 3)
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		ext := h.GetExtension(3)
		var seq uint16 = 65535
		if len(ext) == 2 {
			seq = uint16(ext[0])<<8 | uint16(ext[1])
		}
		sent <- [2]uint16{h.SequenceNumber, seq}
		return 1, nil
	}))
	h := rtp.Header{SSRC: 1, SequenceNumber: 55, Marker: true}
	_ = h.SetExtension(3, []byte{0, 99})
	_, _ = p.Write(&h, []byte{1}, nil)
	first := <-sent
	p.Replay(first[0])
	second := <-sent
	if first[0] != second[0] || first[1] != 0 || second[1] != 1 {
		t.Fatal("replay reused transport feedback identity", first, second)
	}
	if p.departures[0].media.Load() != uint32(first[0])+1 || p.departures[1].media.Load() != 0 {
		t.Fatal("transport repair did not distinguish original media from replay")
	}
}

func TestTransportFeedbackRTTCancelsReceiverClockAndBatchWait(t *testing.T) {
	p := &Pacer{}
	now := time.Now()
	for i, age := range []time.Duration{130 * time.Millisecond, 110 * time.Millisecond} {
		p.departures[i].at.Store(now.Add(-age).UnixNano())
		p.departures[i].sequence.Store(uint32(i) + 1)
	}
	f := &rtcp.TransportLayerCC{BaseSequenceNumber: 0, PacketStatusCount: 2, ReferenceTime: 123456,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 2, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}},
		RecvDeltas:   []*rtcp.RecvDelta{{Delta: 1000}, {Delta: 20000}},
	}
	if got := p.FeedbackRTT(f, now); got != 110*time.Millisecond {
		t.Fatal("receiver clock or feedback batching inflated RTT", got)
	}
	f.RecvDeltas = nil
	if p.FeedbackRTT(f, now) != 0 {
		t.Fatal("malformed feedback produced a queue signal")
	}
}

func TestTransportAcknowledgmentsNeverMoveBackward(t *testing.T) {
	p := &Pacer{}
	now := time.Now()
	for i := range 2 {
		p.departures[i].at.Store(now.Add(-50 * time.Millisecond).UnixNano())
		p.departures[i].sequence.Store(uint32(i) + 1)
		p.departures[i].total.Store(int64((i + 1) * 1000))
	}
	feedback := func(seq uint16) *rtcp.TransportLayerCC {
		return &rtcp.TransportLayerCC{BaseSequenceNumber: seq, PacketStatusCount: 1,
			PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 1, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}},
			RecvDeltas:   []*rtcp.RecvDelta{{Delta: 1000}}}
	}
	p.FeedbackRTT(feedback(1), now)
	p.FeedbackRTT(feedback(0), now)
	if p.acknowledgedBytes.Load() != 2000 {
		t.Fatal("old feedback reopened an already-consumed flight window")
	}
}

func TestSuccessfulCapacityProbeSurvivesTheNextLowRateSampleBriefly(t *testing.T) {
	p := &Pacer{}
	now := time.Now()
	p.startTrial(now.Add(100 * time.Millisecond).UnixNano())
	p.recordProbeRate(3_000_000, now)
	p.receiptRate.Store(100_000)
	p.sustainedRate.Store(100_000)
	p.sustainedRateAt.Store(now.Add(500 * time.Millisecond).UnixNano())
	p.recordProbeRate(100_000, now.Add(200*time.Millisecond))
	if p.capacityRate(now.Add(500*time.Millisecond)) != 3_000_000 {
		t.Fatal("low rendition erased the received upgrade capacity")
	}
	if p.capacityRate(now.Add(10*time.Second)) != 0 {
		t.Fatal("stale capacity proof survived its ten-second bound")
	}
	p.recordProbeRate(4_000_000, now.Add(2*time.Second))
	if p.capacityRate(now.Add(2*time.Second)) != 3_000_000 {
		t.Fatal("ordinary traffic outside a probe changed the retained capacity proof")
	}
}

func TestCapacityWindowRejectsShortBurstAndMeasuresSustainedDelivery(t *testing.T) {
	var burst, steady deliveryWindow
	for i := 0; i <= 8; i++ {
		at := time.Duration(i) * 50 * time.Millisecond
		bytes := int64(0)
		if i == 1 {
			bytes = 40_000
		}
		burst.observe(at, bytes)
		if rate := steady.observe(at, 12_500); i >= 4 && rate != 2_000_000 {
			t.Fatal("sustained rate omitted idle intervals", rate)
		}
	}
	if got := burst.observe(450*time.Millisecond, 0); got != 0 {
		t.Fatal("short packet train established lasting capacity", got)
	}
	const period = (1 << 24) * 64 * time.Millisecond
	var wrap deliveryWindow
	wrap.observe(period-100*time.Millisecond, 1000)
	wrap.observe(0, 1000)
	if got := wrap.observe(100*time.Millisecond, 1000); got != 80_000 {
		t.Fatal("feedback timestamp wrap changed delivered rate", got)
	}
}

func TestRenditionProofSurvivesPublisherKeyframeTransitButNotNewCongestion(t *testing.T) {
	now := time.Now()
	var a renditionAdmission
	if a.allow(now, false, true) || !a.allow(now, true, true) || !a.allow(now.Add(1500*time.Millisecond), false, true) {
		t.Fatal("upswitch did not wait for proof or lost a keyframe already in transit")
	}
	if a.allow(now.Add(1600*time.Millisecond), false, false) || a.allow(now.Add(1700*time.Millisecond), false, true) {
		t.Fatal("upswitch survived lost capacity without new proof")
	}
	if !a.allow(now.Add(2*time.Second), true, true) || a.allow(now.Add(4*time.Second), false, true) {
		t.Fatal("pending keyframe admission escaped its two-second bound")
	}
}

func TestLargeKeyframeGraceUsesTheFlightBoundedDrainRate(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.boundFlight.Store(true)
	p.minimumFeedbackRTT.Store(int64(time.Millisecond))
	p.acknowledgedBytes.Store(1)
	p.sustainedRate.Store(2_000_000)
	p.sustainedRateAt.Store(now.UnixNano())
	p.graceBytes.Store(250_000)
	p.graceRate.Store(6_000_000)
	deadline := p.packetDeadline(pacedPacket{at: now}, 6_000_000)
	if got := deadline.Sub(now); got != 3300*time.Millisecond {
		t.Fatal("large refresh grace used a ceiling it cannot drain at", got)
	}
	p.graceRate.Store(128_000)
	if got := p.packetDeadline(pacedPacket{at: now}, 6_000_000).Sub(now); got != maxKeyframeCatchup+100*time.Millisecond {
		t.Fatal("flight-aware grace escaped the existing bound", got)
	}
}

func TestReturnedHeartbeatRequestsKeyframeBeforeBrowserTimer(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1}
	p := &Peer{Room: r, Pacer: &Pacer{}}
	now := time.Now()
	p.Sent.Store(1)
	p.rtcpAt.Store(now.UnixNano())
	p.Pacer.pliAfter.Store(now.Add(10 * time.Second).UnixNano())
	p.noteHeartbeat(now)
	p.rtcpAt.Store(now.Add(-2 * time.Second).UnixNano())
	p.noteHeartbeat(now.Add(2 * time.Second))
	if r.PLIsSent.Load() != 1 {
		t.Fatal("returned transport waited for browser keyframe timer")
	}
	p.noteHeartbeat(now.Add(2500 * time.Millisecond))
	if r.PLIsSent.Load() != 1 {
		t.Fatal("ordinary heartbeat repeated recovery request")
	}
}

func TestRecoveryKeyframeSkipsOldGapsAndKeepsReorderedKeyPackets(t *testing.T) {
	r := newReorder()
	now := time.Now()
	r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 100}}, now)
	r.pop(now)
	r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 106}}, now)
	if len(r.pop(now)) != 0 {
		t.Fatal("ordinary delta did not wait for repair")
	}
	if !r.keyframe(105) {
		t.Fatal("new keyframe did not supersede the missing references")
	}
	r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 105}}, now)
	packets := r.pop(now)
	if len(packets) != 2 || packets[0].SequenceNumber != 105 || packets[1].SequenceNumber != 106 {
		t.Fatal("keyframe waited for obsolete repair or lost its reordered tail")
	}
	if len(r.nacks(now.Add(50*time.Millisecond))) != 0 {
		t.Fatal("obsolete references still requested repair")
	}
	if r.keyframe(100) {
		t.Fatal("late old keyframe moved the reorder cursor backward")
	}
}

func TestReconnectedViewerBypassesNewJoinCoalescing(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1, pliAt: now.Add(-150 * time.Millisecond)}
	p := &Peer{Room: r, Pacer: &Pacer{}}
	p.iceAt.Store(now.Add(-5 * time.Second).UnixNano())
	p.rtcpAt.Store(now.Add(-2 * time.Second).UnixNano())
	p.Pacer.pliAfter.Store(now.Add(5 * time.Second).UnixNano())
	p.viewerICEConnected(now)
	if r.PLIsSent.Load() != 1 || !p.livenessGap(now) {
		t.Fatal("reconnected viewer was delayed as a fresh join")
	}
}

func TestLowJoinWaitsForProvenUpgradeBeforeRequestingNativeKey(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1, lowSSRC: 2}
	r.hasLow.Store(true)
	p := &Peer{Room: r, Pacer: &Pacer{}}
	p.Low.Store(true)
	p.viewerICEConnected(time.Now())
	if !r.pliAt.IsZero() || r.lowPLIAt.IsZero() || r.PLIsSent.Load() != 1 {
		t.Fatal("a low join refreshed native before proving its reservation")
	}
	p.requestKeyframeFor(false)
	if r.pliAt.IsZero() || r.PLIsSent.Load() != 2 {
		t.Fatal("a proven native upgrade did not request its independent key")
	}
}

func TestLowRenditionProbePaddingUsesWireBudget(t *testing.T) {
	p := NewPacer()
	p.mediaPT.Store(98)
	p.twccID.Store(3)
	p.effective.Store(128_000)
	invalid := make(chan bool, 64)
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
		raw, err := (&rtp.Packet{Header: *h, Payload: payload}).Marshal()
		var packet rtp.Packet
		if err != nil || packet.Unmarshal(raw) != nil || len(packet.Payload) != 0 || packet.PaddingSize != 255 || h.Marker {
			invalid <- true
		}
		return len(raw), err
	}))
	p.startTrial(time.Now().Add(120 * time.Millisecond).UnixNano())
	p.notify()
	time.Sleep(200 * time.Millisecond)
	p.Close()
	select {
	case <-invalid:
		t.Fatal("probe was not valid padding-only RTP")
	default:
	}
	wire := p.wireBytes.Load()
	if wire < 1000 || wire > 4000 {
		t.Fatal("padding borrowed media burst credit", wire)
	}
	if wire != p.paddingBytes.Load() || p.repairBytes.Load() != 0 || p.cache.get(1, time.Now()) != nil || len(p.FullLayerFrames()) != 0 {
		t.Fatal("probe counted as productive or replayable media")
	}
}

func TestTemporalUpgradeWaitsForReceivedCapacity(t *testing.T) {
	burst := [3]int{80_000, 60_000, 600_000}
	steady := [3]int{40_000, 60_000, 100_000}
	p := &Pacer{}
	p.effective.Store(10_000_000)
	p.probeRate.Store(512_000)
	if admittedLayer(burst, int(p.mediaRate(time.Now())), 0) != 1 {
		t.Fatal("unused ceiling admitted an encoder burst or blocked a fitting stream")
	}
	p.probeRate.Store(390_000)
	if admittedLayer(steady, int(p.mediaRate(time.Now())), 0) != 2 {
		t.Fatal("current received proof did not admit fitting media")
	}
	p.probeRate.Store(0)
	if admittedLayer(burst, int(p.mediaRate(time.Now())), 0) != 2 {
		t.Fatal("unknown-path startup lost its bounded numerical reservation")
	}
}

func TestSmallNativeReturnRequiresReceivedHeadroomForRefreshDeltas(t *testing.T) {
	if got := nativeWireRequired(200_000, .1); got != 244_445 {
		t.Fatal("normal margin blocks a full stream on a fair queue", got)
	}
	if got := nativeWireRequired(2_000_000, .1); got != 2_444_445 {
		t.Fatal("large-screen native admission changed", got)
	}
	if got := nativeWireRequired(200_000, .1); got != 244_445 {
		t.Fatal("low-delay path cannot use its available native share", got)
	}
}

func TestWiFiReplayUsesFastSpacingOnlyWithMeasuredHeadroom(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.rtt.Store(int64(80 * time.Millisecond))
	p.effective.Store(4_000_000)
	p.flightDemand.Store(200_000)
	p.keyframeBytes.Store(9_000)
	if got := p.replaySpacing(now); got != 30*time.Millisecond {
		t.Fatal("lost Wi-Fi repair still waits a full RTT", got)
	}
	p.effective.Store(256_000)
	if got := p.replaySpacing(now); got != 80*time.Millisecond {
		t.Fatal("constrained path lost RTT spacing", got)
	}
	p.effective.Store(4_000_000)
	p.keyframeBytes.Store(250_000)
	if got := p.replaySpacing(now); got != 30*time.Millisecond {
		t.Fatal("the same packet repair depended on the source's key size", got)
	}
	p.keyframeBytes.Store(9_000)
	p.queue = []pacedPacket{{at: now.Add(-50 * time.Millisecond)}}
	if got := p.replaySpacing(now); got != 80*time.Millisecond {
		t.Fatal("local backlog bypassed retry spacing", got)
	}
	p.queue = nil
	p.rtt.Store(int64(400 * time.Millisecond))
	if got := p.replaySpacing(now); got != 400*time.Millisecond {
		t.Fatal("queued path bypassed retry spacing", got)
	}
	p.rtt.Store(0)
	if got := p.replaySpacing(now); got != 100*time.Millisecond {
		t.Fatal("unknown RTT lost bounded startup spacing", got)
	}
}

func TestDecodedRateUsesAdvancingMetadataCounter(t *testing.T) {
	now := time.Now()
	p := &Peer{}
	p.reportDecoded(now, 1)
	p.reportDecoded(now.Add(500*time.Millisecond), 8)
	if p.decodedFPS.Load() != 14_000 {
		t.Fatal("incorrect decoded progress rate", p.decodedFPS.Load())
	}
	p.reportDecoded(now.Add(time.Second), 1_000_000)
	if p.decodedFPS.Load() != 0 {
		t.Fatal("unbounded counter jump established throughput")
	}
}

func TestIndependentKeyframeAbandonsNativeCatchup(t *testing.T) {
	p := &Pacer{queue: []pacedPacket{{keyPacket: true, p: []byte{1}, h: rtp.Header{Timestamp: 1}}, {keyPacket: true, p: []byte{2}, h: rtp.Header{Timestamp: 1}, end: true}}}
	p.bytes = 26
	p.active.Store(true)
	p.activeKey.Store(true)
	p.lastWire.Store(10)
	p.cache.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 10}}, time.Now())
	dropped := 0
	p.OnDrop = func() { dropped++ }
	p.SetThumbnail(true)
	if len(p.queue) != 0 || p.active.Load() || p.activeKey.Load() || !p.waitKey || dropped != 0 || p.cache.get(10, time.Now()) != nil {
		t.Fatal("new independent keyframe was pinned behind native catch-up")
	}
}

func TestDecodedRateDoesNotCountFeedbackQueueJitterAsSlowDecoding(t *testing.T) {
	p := &Peer{}
	now := time.Now()
	sample := now.UnixMilli()
	p.reportDecodedSample(now, 1, sample)
	p.reportDecodedSample(now.Add(2*time.Second), 8, sample+500)
	if p.decodedFPS.Load() != 14_000 {
		t.Fatal("queued heartbeat changed the measured decoder rate", p.decodedFPS.Load())
	}
}

func TestAnsweredRepairCanRequestTheNextKeyframePromptly(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	for _, answered := range []bool{false, true} {
		r := &Room{publisher: &Peer{PC: pc}, ssrc: 1, pliAt: now.Add(-400 * time.Millisecond)}
		r.keyStart.Store(now.Add(-500 * time.Millisecond).UnixNano())
		if answered {
			r.keyStart.Store(now.Add(-300 * time.Millisecond).UnixNano())
			r.keyDone.Store(now.Add(-250 * time.Millisecond).UnixNano())
		}
		r.PLI()
		if (r.PLIsSent.Load() == 1) != answered {
			t.Fatalf("answered=%v repair sent=%d; completed repairs must not inherit unanswered backoff", answered, r.PLIsSent.Load())
		}
	}
}

func TestPartialNativeTrialUsesTheSameBoundAsTheLowRendition(t *testing.T) {
	p := &Pacer{}
	now := time.Now()
	p.rate.Store(1000000)
	p.effective.Store(500000)
	p.boundFlight.Store(true)
	p.temporal.Store(1)
	p.probeCeiling.Store(250000)
	p.startTrial(now.Add(time.Second).UnixNano())
	if p.wireRate(now) != 250000 {
		t.Fatal("partial native trial bypassed its wire ceiling")
	}
	p.temporal.Store(2)
	if p.wireRate(now) != 250000 || p.wireRate(now.Add(time.Second)) != 500000 {
		t.Fatal("a layer switch bypassed the active trial ceiling or retained it after completion")
	}
}

func TestPaddingLossCannotQualifyAnEncodingUpgrade(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.trialLossAt.Store(now.UnixNano())
	p.startTrial(now.Add(time.Second).UnixNano())
	p.trialLossFor.Store(p.probeUntil.Load())
	p.trialLossPPM.Store(100000)
	if p.trialClear(now) {
		t.Fatal("a trial dropping padding still admitted media")
	}
	if !p.trialClear(now.Add(time.Second)) {
		t.Fatal("stale loss postponed new experiments forever")
	}
	until := now.Add(time.Second).UnixNano()
	if trialFeedbackComplete(now.Add(1040*time.Millisecond), until, 80*time.Millisecond) || !trialFeedbackComplete(now.Add(1080*time.Millisecond), until, 80*time.Millisecond) {
		t.Fatal("admission did not wait for the feedback tail")
	}
}
func TestTransportFeedbackMeasuresPaddingTrialLoss(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.rtt.Store(int64(50 * time.Millisecond))
	p.startTrial(now.Add(time.Second).UnixNano())
	for i := 0; i < 2; i++ {
		p.departures[i].sequence.Store(uint32(i + 1))
		p.departures[i].at.Store(now.Add(-100 * time.Millisecond).UnixNano())
		p.departures[i].kind.Store(uint32(videoKind))
		p.departures[i].trial.Store(now.Add(time.Second).UnixNano())
	}
	f := &rtcp.TransportLayerCC{PacketStatusCount: 2, PacketChunks: []rtcp.PacketStatusChunk{&rtcp.StatusVectorChunk{SymbolList: []uint16{rtcp.TypeTCCPacketNotReceived, rtcp.TypeTCCPacketReceivedSmallDelta}}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: 1000}}}
	p.probeUntil.Store(0)
	p.FeedbackRTT(f, now)
	if p.trialClear(now) || p.trialClear(now.Add(2*time.Second)) || p.mediaLoss(now) != 0 {
		t.Fatal("padding loss was ignored or confused with media loss")
	}
}

func TestSharedRefreshChoosesASmallIndependentKeyWhenNativeWouldBeLate(t *testing.T) {
	if !nativeRefreshTooSlow(28000, 128000) || nativeRefreshTooSlow(28000, 1000000) {
		t.Fatal("refresh selection did not reserve transport/playout time")
	}
	if !nativeRefreshTooSlow(250000, 256000) {
		t.Fatal("realistic key escaped its serialization cost")
	}
}

func TestCompletedTrialRetainsItsOwnReceivedWindowThroughFeedbackTransit(t *testing.T) {
	now := time.Now()
	id := now.Add(750 * time.Millisecond).UnixNano()
	p := &Pacer{}
	p.startTrial(id)
	for i := 0; i <= 6; i++ {
		p.recordTrialDelivery(id, time.Duration(i)*100*time.Millisecond, 3125, now.Add(time.Duration(i)*100*time.Millisecond))
	}
	if p.trialRate(now.Add(time.Second)) != 250000 {
		t.Fatal("a completed trial lost its received rate", p.trialRate(now.Add(time.Second)))
	}
	p.startTrial(now.Add(3 * time.Second).UnixNano())
	if p.trialRate(now.Add(time.Second)) != 0 {
		t.Fatal("another trial reused old capacity")
	}
	p.recordTrialDelivery(id-1, time.Second, 3125, now.Add(time.Second))
	if p.trialDeliveryID != id {
		t.Fatal("old feedback replaced the current trial")
	}
}

func TestTrialReceiptDoesNotReserveRepairTwice(t *testing.T) {
	rates := [3]int{100000, 40000, 60000}
	budget := int(nativeWireRequired(200000, .1))
	target := admittedLayer(rates, budget, .1)
	if target != 2 {
		t.Fatal("wire budget did not reserve native media and repair")
	}
	p := &Pacer{}
	p.effective.Store(int64(budget))
	p.probeRate.Store(200000)
	if admittedLayer(rates, int(p.mediaRate(time.Now())), 0) != 2 {
		t.Fatal("completed trial ignored or double-counted its ten-percent media margin")
	}
	p.probeRate.Store(199999)
	if admittedLayer(rates, int(p.mediaRate(time.Now())), 0) != 1 {
		t.Fatal("insufficient received proof admitted another layer")
	}
}

func TestRequiredWireBudgetAdmitsItsOwnLayer(t *testing.T) {
	for _, rate := range []int{64000, 77777, 180000, 200000, 291231} {
		for _, reserve := range []float64{0, .1, .25, .4} {
			rates := [3]int{rate / 4, rate / 4, rate - 2*(rate/4)}
			budget := int(nativeWireRequired(rate, reserve))
			if admittedLayer(rates, budget, reserve) != 2 || admittedLayer(rates, budget-1, reserve) == 2 {
				t.Fatal("computed budget was not the exact admission minimum", rate, reserve, budget)
			}
		}
	}
}

func TestFIFOTrialCannotBypassItsCeilingWhenFlightsAreUnbounded(t *testing.T) {
	p := &Pacer{}
	now := time.Now()
	p.effective.Store(1400000)
	p.probeCeiling.Store(240000)
	p.startTrial(now.Add(750 * time.Millisecond).UnixNano())
	for _, layer := range []int32{0, 1, 2} {
		p.temporal.Store(layer)
		if p.wireRate(now) != 240000 {
			t.Fatal("FIFO padding used an unproved numerical ceiling", layer, p.wireRate(now))
		}
	}
	if p.wireRate(now.Add(time.Second)) != 1400000 {
		t.Fatal("completed trial retained a stale wire cap")
	}
}

func TestRecoveryPLIUsesThePendingEncoding(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for _, low := range []bool{false, true} {
		r := &Room{publisher: &Peer{PC: pc}, ssrc: 1, lowSSRC: 2}
		p := &Peer{Room: r}
		p.Low.Store(!low)
		target := int32(1)
		if low {
			target = 2
		}
		p.refreshEncoding.Store(target)
		p.requestKeyframe()
		r.mu.Lock()
		native, small := !r.pliAt.IsZero(), !r.lowPLIAt.IsZero()
		r.mu.Unlock()
		if small != low || native == low || p.Low.Load() == low {
			t.Fatal("recovery refreshed the old encoding or switched without a key", low, native, small)
		}
	}
}

func TestMeasuredNativeDemandDoesNotPermanentlyChargeARefresh(t *testing.T) {
	r := &Room{}
	r.keyBytes.Store(80000)
	if r.nativeDemand() != 640000 {
		t.Fatal("unknown source lost its keyframe fallback")
	}
	r.tidRate[0].Store(40000)
	r.tidRate[1].Store(60000)
	r.tidRate[2].Store(100000)
	if r.nativeDemand() != 200000 {
		t.Fatal("a one-off keyframe inflated sustained native admission", r.nativeDemand())
	}
	if !nativeRefreshTooSlow(r.keyBytes.Load(), 256000) {
		t.Fatal("steady admission bypassed the separate refresh latency bound")
	}
}
