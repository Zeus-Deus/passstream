package sfu

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestVideoOfferRequiresVP9ProfileZero(t *testing.T) {
	for _, tc := range []struct {
		name, codec, fmtp string
		pass              bool
	}{
		{"VP9", "VP9", "profile-id=0", true},
		{"VP9 default", "VP9", "", true},
		{"VP8", "VP8", "", false},
		{"VP9 profile 2", "VP9", "profile-id=2", false},
		{"H264", "H264", "profile-level-id=42001f", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 98\r\na=rtpmap:98 " + tc.codec + "/90000\r\na=fmtp:98 " + tc.fmtp + "\r\n"}
			err := ValidateVideoOffer(offer)
			if (err == nil) != tc.pass {
				t.Fatalf("ValidateVideoOffer: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "requires VP9 profile 0") {
				t.Fatal(err)
			}
		})
	}
}

func TestAudioWireCannotProveVideoCapacity(t *testing.T) {
	// Exercise the real audio send path and its TWCC receipt, including during a trial.
	p := NewPacer()
	defer p.Close()
	p.twccID.Store(3)
	p.probeID.Store(42)
	p.probeUntil.Store(time.Now().Add(time.Second).UnixNano())
	x := pacedPacket{kind: audioKind, h: rtp.Header{Version: 2, SSRC: 2}, p: make([]byte, 100), at: time.Now()}
	var seq uint16
	w := interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) {
		seq = binary.BigEndian.Uint16(h.GetExtension(3))
		return len(b), nil
	})
	n := p.sendAudio(w, x)
	if p.audioWireBytes.Load() != uint64(n) || p.wireBytes.Load() != uint64(n) || p.videoWireBytes.Load() != 0 || p.departedBytes.Load() != 0 || p.paddingBytes.Load() != 0 || p.repairBytes.Load() != 0 {
		t.Fatal("audio contaminated video wire accounting")
	}
	slot := &p.departures[int(seq)%len(p.departures)]
	if slot.kind.Load() != uint32(audioKind) || slot.trial.Load() != 0 || slot.media.Load() != 0 {
		t.Fatal("audio departure entered a video trial or repair")
	}
	feedback := &rtcp.TransportLayerCC{BaseSequenceNumber: seq, PacketStatusCount: 1, PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: 1}}, RecvDeltas: []*rtcp.RecvDelta{{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 250}}}
	p.FeedbackRTT(feedback, time.Now().Add(50*time.Millisecond))
	if p.feedbackRate.Load() != 0 || p.sustainedRate.Load() != 0 || p.trialReceived.Load() != 0 || p.trialReceivedID.Load() != 0 || p.acknowledgedBytes.Load() != 0 {
		t.Fatal("audio receipt proved video capacity")
	}
}

func TestKeyframeEntrypointsShareViewerLimiter(t *testing.T) {
	r := &Room{viewers: map[*Peer]bool{}}
	p := &Peer{Room: r}
	calls := 0
	p.SetObserver(nil, func() bool { calls++; return false })
	p.requestKeyframe()
	p.requestPLI()
	if calls != 2 || r.PLIsSent.Load() != 0 || p.refreshEncoding.Load() != 0 {
		t.Fatalf("limiter bypass: calls=%d", calls)
	}
}

func TestRefreshTransitAdvisoryIsBoundedAndPublisherIgnored(t *testing.T) {
	now := time.Now()
	pacer := &Pacer{queue: []pacedPacket{{}}, bytes: 32000}
	pacer.effective.Store(128000)
	pacer.graceBytes.Store(32000)
	pacer.catchupAt.Store(now.UnixNano())
	p := &Peer{Pacer: pacer}
	if got := p.RefreshTransitMS(); got != 2000 {
		t.Fatalf("refresh=%d, want 2000", got)
	}
	pacer.bytes = 1_000_000
	if got := p.RefreshTransitMS(); got != 6000 {
		t.Fatalf("refresh=%d, want bounded 6000", got)
	}
	p.Publish = true
	if p.RefreshTransitMS() != 0 {
		t.Fatal("publisher received a viewer advisory")
	}
}
