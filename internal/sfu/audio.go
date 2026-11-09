package sfu

import (
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Audio is forwarded opaquely. Under E2EE every Opus frame is a complete
// SFrame ciphertext (track 1, no clear prefix), so nothing here parses an
// audio payload: packets are copied byte for byte, with only the RTP header
// rewritten (sequence numbers and a timestamp offset across publisher
// changes). Each viewer's ctrl pacer sends audio ahead of video from the same
// rate; its wire rate is metered separately (Costs.AudioWire) and never
// counts as video. RED is not used for audio: Chrome splits RED inside the
// audio decoder, after the receive transform, so a RED payload would reach
// the SFrame decryptor as one undecryptable frame.

const (
	audioPayloadType = 111
	// Audio older than this is useless to the receiver's jitter buffer: it is
	// the deadline of queued audio and of audio retransmissions.
	audioMaxAge = 200 * time.Millisecond
	// Opus RTP clock: 20 ms.
	audioFrameTicks = 960
)

var opusCapability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}

func registerAudio(m *webrtc.MediaEngine) error {
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opusCapability, PayloadType: audioPayloadType}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeAudio)
	return nil
}

// roomAudio counts publisher audio packets (diagnostics).
type roomAudio struct{ Packets atomic.Uint64 }

// readAudio meters the publisher's audio and offers it to every viewer.
func (p *Peer) readAudio(t *webrtc.TrackRemote) {
	r := p.Room
	for {
		pkt, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		now := time.Now()
		r.audio.Packets.Add(1)
		r.meterAudio(now, pkt.MarshalSize()+wireOverhead)
		r.mu.Lock()
		if r.publisher != p {
			r.mu.Unlock()
			return
		}
		for v := range r.viewers {
			if v.view != nil {
				v.view.pushAudio(now, pkt, r.source)
			}
		}
		r.mu.Unlock()
	}
}
