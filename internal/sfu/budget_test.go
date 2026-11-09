package sfu

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"testing"
	"time"
)

func TestLayerAdmissionReservesRepair(t *testing.T) {
	rates := [3]int{80_000, 70_000, 100_000}
	if admittedLayer(rates, 300_000, .1) != 1 {
		t.Fatal("top admitted without repair margin")
	}
	if admittedLayer(rates, 300_000, .5) != 0 {
		t.Fatal("repair reservation ignored")
	}
	if admittedLayer(rates, 400_000, .1) != 2 {
		t.Fatal("fitting top layer rejected")
	}
}

func TestReplaySuppressedWhenQueueMissesDeadline(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.SetTargetBitrate(64_000)
	now := time.Now()
	p.cache.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 7}, Payload: make([]byte, 100)}, now.Add(-950*time.Millisecond))
	p.rtt.Store(int64(200 * time.Millisecond))
	p.Replay(7)
	if p.Suppressed.Load() != 1 {
		t.Fatal("useless replay was admitted")
	}
}
func TestParityNeverReplayed(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.redPT.Store(116)
	p.fecPT.Store(117)
	sent := make(chan uint16, 2)
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		sent <- h.SequenceNumber
		return 1, nil
	}))
	p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 1}, []byte{1}, nil)
	seq := <-sent
	// Construct a valid parity mapping for the media packet.
	payload := make([]byte, 15)
	payload[0] = 117
	payload[4] = 1
	payload[13] = 0x80
	p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 2, PayloadType: 116}, payload, nil)
	select {
	case seq = <-sent:
	case <-time.After(time.Second):
		t.Fatal("parity was not sent")
	}
	if p.cache.get(seq, time.Now()) != nil {
		t.Fatal("parity in replay cache")
	}
}

func TestReplacementAdmissionBarrier(t *testing.T) {
	p := &Peer{Room: &Room{}, Track: &repairTrack{}, frames: make(chan frame), done: make(chan struct{})}
	go p.forward()
	defer close(p.done)
	f := frame{at: time.Now(), key: true, packets: []*rtp.Packet{{Header: rtp.Header{Version: 2, Marker: true}, Payload: []byte{0x08, 0}}}}
	p.frames <- f
	// The second handoff means the loop has finished processing the first.
	p.frames <- f
	if p.Sent.Load() != 0 {
		t.Fatal("forwarded before re-admission")
	}
	p.Admit()
	p.frames <- f
	p.frames <- f
	if p.Sent.Load() == 0 {
		t.Fatal("admission did not open forwarding")
	}
}

func TestNewKeyframeSupersedesUnstartedQueue(t *testing.T) {
	p := &Pacer{writers: map[uint32]interceptor.RTPWriter{}, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	defer p.Close()
	p.SetTargetBitrate(128_000)
	p.mediaPT.Store(98)
	p.active.Store(true)
	p.activeTimestamp.Store(1)
	p.queue = []pacedPacket{{h: rtp.Header{Timestamp: 1}}, {h: rtp.Header{Timestamp: 1}, end: true}, {h: rtp.Header{Timestamp: 2}, key: true, end: true}, {h: rtp.Header{Timestamp: 3}, end: true}}
	p.bytes = 48
	p.Write(&rtp.Header{Timestamp: 4, PayloadType: 98, Marker: true}, []byte{0x08, 0}, nil)
	if len(p.queue) != 3 || p.queue[0].h.Timestamp != 1 || p.queue[1].h.Timestamp != 1 || p.queue[2].h.Timestamp != 4 {
		t.Fatal("queued obsolete keyframe or discarded started picture")
	}
	if p.bytes != 38 || p.Drops.Load() != 2 {
		t.Fatalf("queue accounting: %d bytes, %d drops", p.bytes, p.Drops.Load())
	}
}

func TestUsefulReplayPrecedesQueuedKeyframeBytes(t *testing.T) {
	p := &Pacer{writers: map[uint32]interceptor.RTPWriter{}, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	defer p.Close()
	p.SetTargetBitrate(64_000)
	p.queue = []pacedPacket{{h: rtp.Header{Timestamp: 1}, p: make([]byte, 1<<20), end: true}}
	p.bytes = (1 << 20) + 12
	p.cache.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9}, Payload: []byte{1}}, time.Now())
	p.Replay(9)
	if len(p.queue) != 2 || !p.queue[0].repair || p.queue[1].h.Timestamp != 1 {
		t.Fatal("useful repair stranded behind media")
	}
}

func TestNewJoinsShareKeyframeSpacing(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	r := &Room{publisher: &Peer{PC: pc}, ssrc: 1, pliAt: now.Add(-600 * time.Millisecond)}
	r.keyStart.Store(now.Add(-300 * time.Millisecond).UnixNano())
	r.keyDone.Store(now.Add(-200 * time.Millisecond).UnixNano())
	r.pli(true)
	if r.PLIsSent.Load() != 1 {
		t.Fatal("join waited for repair holdoff")
	}
	r.pli(true)
	if r.PLIsSent.Load() != 1 {
		t.Fatal("join bypassed coalescing")
	}
}
func TestAcceptedPeerDoesNotForwardBeforeTransportConnects(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	p := &Peer{PC: pc, Room: &Room{}, Track: &repairTrack{}, frames: make(chan frame), done: make(chan struct{})}
	p.Admit()
	go p.forward()
	defer close(p.done)
	f := frame{at: time.Now(), key: true, packets: []*rtp.Packet{{Header: rtp.Header{Marker: true}, Payload: []byte{8, 0}}}}
	p.frames <- f
	p.frames <- f
	if p.Sent.Load() != 0 {
		t.Fatal("forwarded before DTLS connected")
	}
}

func TestDeadlineOrderingProtectsMediaAndUrgentReplay(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.queue = []pacedPacket{{repair: true, deadline: now.Add(time.Second), at: now}, {at: now}}
	if p.nextPacket(128_000) != 1 {
		t.Fatal("nonurgent replay starved fresh media")
	}
	p.graceBytes.Store(32_000)
	p.queue[1].key = true
	if p.nextPacket(128_000) != 0 {
		t.Fatal("replay stranded behind a long keyframe")
	}
	p.graceBytes.Store(0)
	p.queue[0].deadline = now.Add(30 * time.Millisecond)
	if p.nextPacket(128_000) != 0 {
		t.Fatal("urgent replay missed usefulness deadline")
	}
}
func TestQueuedReplayIsNotDuplicatedOrChargedAnotherTry(t *testing.T) {
	p := &Pacer{writers: map[uint32]interceptor.RTPWriter{}, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	defer p.Close()
	p.SetTargetBitrate(128_000)
	p.cache.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9}, Payload: []byte{1}}, time.Now())
	p.Replay(9)
	p.cache.mu.Lock()
	p.cache.entries[9].last = time.Now().Add(-100 * time.Millisecond)
	p.cache.mu.Unlock()
	p.Replay(9)
	if len(p.queue) != 1 || p.cache.outcomes()[0] != 1 {
		t.Fatal("queued replay duplicated or exhausted retries")
	}
	p.discardAt(0)
	if p.replayPending[9] {
		t.Fatal("discard did not release replay reservation")
	}
}
func TestReplayedKeyPacketCannotRestoreDroppedReferences(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mediaPT.Store(98)
	sent := make(chan uint16, 2)
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		sent <- h.SequenceNumber
		return 1, nil
	}))
	p.mu.Lock()
	p.waitKey = true
	p.mu.Unlock()
	p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 99, PayloadType: 98, Marker: true}, []byte{8, 0}, interceptor.Attributes{"passstream.repair": true, "passstream.deadline": time.Now().Add(time.Second)})
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("replay not sent")
	}
	p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 100, Timestamp: 2, PayloadType: 98, Marker: true}, []byte{0x48, 0}, nil)
	select {
	case <-sent:
		t.Fatal("replay resumed a broken reference chain")
	case <-time.After(150 * time.Millisecond):
	}
	if p.Drops.Load() == 0 {
		t.Fatal("broken dependent picture retained")
	}
}

func TestKeyframeCatchupIncludesStartedPictureAndWireOverhead(t *testing.T) {
	p := &Pacer{writers: map[uint32]interceptor.RTPWriter{}, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	defer p.Close()
	p.SetTargetBitrate(64_000)
	p.mediaPT.Store(98)
	p.active.Store(true)
	p.activeTimestamp.Store(1)
	p.queue = []pacedPacket{{h: rtp.Header{Timestamp: 1}, p: make([]byte, 20_000), key: true, end: true}}
	p.bytes = 20_012
	payload := make([]byte, 8_000)
	payload[0] = 0x08
	p.Write(&rtp.Header{Timestamp: 2, PayloadType: 98, Marker: true}, payload, nil)
	wire := 0
	for _, x := range p.queue {
		wire += wireSize(x)
	}
	if p.graceBytes.Load() != int64(wire) {
		t.Fatalf("catch-up omits queued keyframe or wire overhead: %d != %d", p.graceBytes.Load(), wire)
	}
	now := time.Now()
	deadline := p.packetDeadline(pacedPacket{at: now}, 64_000)
	if deadline.Sub(now) <= 3*time.Second {
		t.Fatal("dependent pictures expire before the keyframe can serialize")
	}
	if faster := p.packetDeadline(pacedPacket{at: now}, 128_000); !faster.Equal(deadline) {
		t.Fatal("a probe shortened catch-up for already queued pictures")
	}
	p.graceBytes.Store(2 << 20)
	if p.packetDeadline(pacedPacket{at: now}, 64_000).Sub(now) != maxKeyframeCatchup+100*time.Millisecond {
		t.Fatal("unbounded keyframe catch-up grace")
	}
}

func TestInitialTransportRTTDetectsFirstQueuedReport(t *testing.T) {
	now := time.Now()
	for _, baseline := range []time.Duration{time.Millisecond, 800 * time.Millisecond} {
		p := &Peer{}
		p.pathEpoch.Store(1)
		p.initialRTT.Store(int64(baseline))
		rtt := 805 * time.Millisecond
		if baseline == time.Millisecond {
			rtt = 2400 * time.Millisecond
		}
		p.observePathRTT(now, rtt, false)
		excess := rtt - time.Duration(p.initialRTT.Load())
		if baseline == time.Millisecond && excess <= rttRise {
			t.Fatal("first queued RR became the baseline")
		}
		if baseline == 800*time.Millisecond && excess >= rttConfirm {
			t.Fatal("intrinsically long path treated as queue growth")
		}
	}
}

func TestInitialRoundTripUsesHandshakeUntilICEHasStats(t *testing.T) {
	for _, tc := range []struct{ ice, handshake, want time.Duration }{
		{0, 2 * time.Millisecond, 2 * time.Millisecond},
		{0, 900 * time.Millisecond, 900 * time.Millisecond},
		{80 * time.Millisecond, 200 * time.Millisecond, 80 * time.Millisecond},
		{0, 0, 0},
	} {
		if got := initialRoundTrip(tc.ice, tc.handshake); got != tc.want {
			t.Fatalf("initial RTT %s, want %s", got, tc.want)
		}
	}
}

func TestLayerStartupAdmitsMeasuredDemandImmediately(t *testing.T) {
	rates := [3]int{100_000, 100_000, 100_000}
	if admittedLayer(rates, 2_000_000, 0) != 2 {
		t.Fatal("clean startup withheld an affordable temporal layer")
	}
	if admittedLayer(rates, 150_000, 0) != 0 {
		t.Fatal("startup ignored the measured wire budget")
	}
}

func TestReplayWaitsForFeedbackOpportunity(t *testing.T) {
	now := time.Now()
	var c packetCache
	c.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9}}, now)
	if p, _ := c.replaySpaced(9, now, 100*time.Millisecond); p == nil {
		t.Fatal("initial replay suppressed")
	}
	if p, _ := c.replaySpaced(9, now.Add(66*time.Millisecond), 100*time.Millisecond); p != nil {
		t.Fatal("replayed before an RTT could return")
	}
	if p, _ := c.replaySpaced(9, now.Add(100*time.Millisecond), 100*time.Millisecond); p == nil {
		t.Fatal("useful retry suppressed after RTT")
	}
}
