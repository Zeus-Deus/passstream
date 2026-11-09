package sfu

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"testing"
	"time"
)

func referencePacket(seq uint16, tid uint8) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, PayloadType: 98}, Payload: []byte{0xec, 0x80, 1, tid << 5, 1, 0}}
}
func TestFailedReplayDistinguishesReferencesFromDisposableAndStalePackets(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"base", "middle", "red", "disposable", "fec", "superseded", "missing", "spaced", "exhausted"} {
		t.Run(kind, func(t *testing.T) {
			p := &Pacer{}
			p.mediaPT.Store(98)
			p.redPT.Store(116)
			packet := referencePacket(9, 0)
			switch kind {
			case "middle":
				packet = referencePacket(9, 1)
			case "disposable":
				packet = referencePacket(9, 2)
			case "red":
				packet.PayloadType = 116
				packet.Payload = append([]byte{98}, packet.Payload...)
			case "fec":
				packet.PayloadType = 116
				packet.Payload = []byte{117, 0}
			}
			if kind != "missing" {
				p.cache.put(packet, now.Add(-2*time.Second))
			}
			if kind == "superseded" {
				p.cache.keyframe(10)
			}
			if kind == "spaced" {
				p.cache.put(packet, now)
				p.cache.get(9, now)
			}
			if kind == "exhausted" {
				p.cache.put(packet, now)
				p.cache.entries[9].tries = replayTries
			}
			if p.Replay(9) {
				t.Fatal("unavailable replay was sent")
			}
			want := kind == "base" || kind == "middle" || kind == "red" || kind == "exhausted"
			if (p.unrepairableReferenceAt.Load() > 0) != want {
				t.Fatal("wrong reference recovery", kind)
			}
		})
	}
}
func TestUnrepairableReferenceRefreshesOnlyAStalledLiveDecoder(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	room := &Room{publisher: &Peer{PC: pc}, ssrc: 1, lowSSRC: 2}
	p := &Peer{Room: room, Pacer: &Pacer{}}
	p.decoderReported.Store(true)
	p.Sent.Store(1)
	p.Pacer.unrepairableReferenceAt.Store(now.UnixNano())
	p.freshAt.Store(now.Add(-100 * time.Millisecond).UnixNano())
	var recovery decoderRepairRecovery
	if recovery.request(now, p, now) {
		t.Fatal("fresh decoder requested a key")
	}
	p.freshAt.Store(now.Add(-450 * time.Millisecond).UnixNano())
	p.decodedFeedbackAt.Store(now.UnixNano())
	if recovery.request(now, p, now.Add(-time.Second)) {
		t.Fatal("publisher pause requested a key")
	}
	p.Pacer.activeKey.Store(true)
	if recovery.request(now, p, now) {
		t.Fatal("active key was aborted")
	}
	p.Pacer.activeKey.Store(false)
	p.requestKeyframeFor(true)
	if !p.pendingLowRefresh(now) || !recovery.request(now, p, now) || room.PLIsSent.Load() != 1 || room.lowPLIAt.IsZero() {
		t.Fatal("stalled decoder did not refresh its pending fallback")
	}
	p.Pacer.unrepairableReferenceAt.Store(now.UnixNano())
	if recovery.request(now.Add(100*time.Millisecond), p, now) {
		t.Fatal("one repair failure flooded PLIs")
	}
	if p.pendingLowRefresh(time.Unix(0, p.refreshRequestedAt.Load()).Add(2 * time.Second)) {
		t.Fatal("abandoned fallback never expired")
	}
	p.Low.Store(true)
	if p.pendingLowRefresh(now) {
		t.Fatal("completed fallback remained pending")
	}
	p.Low.Store(false)
	p.refreshEncoding.Store(1)
	if p.pendingLowRefresh(now) {
		t.Fatal("native admission was pinned")
	}
}
func TestDeliveredKeyClearsAnOldUnrepairableReference(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mediaPT.Store(98)
	p.unrepairableReferenceAt.Store(time.Now().UnixNano())
	delivered := make(chan struct{}, 1)
	p.AddStream(1, interceptor.RTPWriterFunc(func(_ *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) {
		delivered <- struct{}{}
		return len(b), nil
	}))
	p.Write(&rtp.Header{PayloadType: 98, SSRC: 1, Marker: true}, []byte{0x0c, 0}, nil)
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("key was not delivered")
	}
	if p.unrepairableReferenceAt.Load() != 0 {
		t.Fatal("delivered key retained an old failure")
	}
}

func TestExpiredReferenceRequestsACurrentEncodingKey(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	room := &Room{publisher: &Peer{PC: pc}, ssrc: 1}
	p := &Peer{Room: room, Pacer: &Pacer{}}
	p.Pacer.mediaPT.Store(98)
	p.Pacer.redPT.Store(116)
	p.Pacer.cache.put(referencePacket(9, 0), now.Add(-2*time.Second))
	if p.Pacer.Replay(9) {
		t.Fatal("expired reference was sent")
	}
	p.decoderReported.Store(true)
	p.Sent.Store(1)
	p.freshAt.Store(now.Add(-450 * time.Millisecond).UnixNano())
	p.decodedFeedbackAt.Store(now.UnixNano())
	var recovery decoderRepairRecovery
	if !recovery.request(now, p, now) || room.PLIsSent.Load() != 1 || room.pliAt.IsZero() {
		t.Fatal("expired reference and decoded stall did not request a key")
	}
}

func TestNormalDecoderReportingIntervalCannotRequestARepairKey(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	room := &Room{publisher: &Peer{PC: pc}, ssrc: 1}
	p := &Peer{Room: room, Pacer: &Pacer{}}
	p.Sent.Store(1)
	p.reportDecoded(now, 10)
	p.Pacer.unrepairableReferenceAt.Store(now.Add(100 * time.Millisecond).UnixNano())
	var recovery decoderRepairRecovery
	if recovery.request(now.Add(450*time.Millisecond), p, now.Add(450*time.Millisecond)) {
		t.Fatal("normal half-second feedback silence requested a key")
	}
	p.reportDecoded(now.Add(500*time.Millisecond), 10)
	if !recovery.request(now.Add(500*time.Millisecond), p, now.Add(500*time.Millisecond)) || room.PLIsSent.Load() != 1 {
		t.Fatal("explicit nonadvancing report did not confirm repair failure")
	}
}

func TestFailedRepairUsesTheIndependentKeyOnAConstrainedNativePath(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	now := time.Now()
	room := &Room{publisher: &Peer{PC: pc}, ssrc: 1, lowSSRC: 2}
	room.hasLow.Store(true)
	p := &Peer{Room: room, Pacer: &Pacer{}}
	p.pathLimited.Store(true)
	p.Sent.Store(1)
	p.reportDecoded(now.Add(-500*time.Millisecond), 10)
	p.reportDecoded(now, 10)
	p.Pacer.unrepairableReferenceAt.Store(now.UnixNano())
	var recovery decoderRepairRecovery
	if !recovery.request(now, p, now) || room.PLIsSent.Load() != 1 || room.lowPLIAt.IsZero() || !room.pliAt.IsZero() || !p.pendingLowRefresh(now) {
		t.Fatal("constrained broken decoder requested an expensive native refresh")
	}
}

func TestFailedReferenceSurvivesADelayedPictureAndMissingFeedback(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"delayed-picture", "feedback-gap", "healthy", "known-delay", "no-failure", "ordinary-feedback-gap"} {
		t.Run(kind, func(t *testing.T) {
			pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatal(err)
			}
			defer pc.Close()
			room := &Room{publisher: &Peer{PC: pc}, ssrc: 1, lowSSRC: 2}
			room.hasLow.Store(true)
			p := &Peer{Room: room, Pacer: &Pacer{}}
			p.pathLimited.Store(true)
			p.Sent.Store(1)
			p.reportDecoded(now.Add(-600*time.Millisecond), 10)
			p.Pacer.unrepairableReferenceAt.Store(now.Add(-700 * time.Millisecond).UnixNano())
			switch kind {
			case "delayed-picture":
				p.reportDecoded(now, 10)
			case "healthy":
				p.reportDecoded(now.Add(-100*time.Millisecond), 11)
			case "known-delay":
				p.initialRTT.Store(int64(400 * time.Millisecond))
			case "no-failure":
				p.Pacer.unrepairableReferenceAt.Store(0)
			case "ordinary-feedback-gap":
				room.hasLow.Store(false)
			}
			var recovery decoderRepairRecovery
			want := kind == "delayed-picture" || kind == "feedback-gap"
			if got := recovery.request(now, p, now); got != want {
				t.Fatal("wrong broken-reference recovery", got, want)
			}
			if want && (room.lowPLIAt.IsZero() || !room.pliAt.IsZero()) {
				t.Fatal("repair did not choose independent low refresh")
			}
		})
	}
}
