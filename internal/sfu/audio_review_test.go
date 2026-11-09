package sfu

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Regression tests for the round-1 verification of E2EE audio.

// A compound RR carries a block per SSRC. Video adaptation must take only
// the video block: before the fix, audio's highest sequence (777) and loss
// overwrote video's (123000).
func TestVideoRTCPIgnoresAudioReportBlocks(t *testing.T) {
	p := &Peer{Pacer: NewPacer()}
	defer p.Pacer.Close()
	const video, audio = 1111, 2222
	rr := &rtcp.ReceiverReport{SSRC: 1234, Reports: []rtcp.ReceptionReport{
		{SSRC: video, FractionLost: 1, TotalLost: 3, LastSequenceNumber: 123000},
		{SSRC: audio, FractionLost: 255, TotalLost: 17, LastSequenceNumber: 777},
	}}
	p.handleVideoRTCP(video, []rtcp.Packet{rr})
	if p.highestSeq.Load() != 123000 || p.totalLost.Load() != 3 || p.residualLoss.Load() != 1 || p.reports.Load() != 1 {
		t.Fatalf("video counters: highest=%d lost=%d fraction=%d reports=%d", p.highestSeq.Load(), p.totalLost.Load(), p.residualLoss.Load(), p.reports.Load())
	}
	// Audio-only blocks, NACKs and PLIs leave video untouched.
	p.handleVideoRTCP(video, []rtcp.Packet{
		&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: audio, TotalLost: 99, LastSequenceNumber: 5}}},
		&rtcp.TransportLayerNack{MediaSSRC: audio, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{1, 2, 3, 4})},
	})
	if p.highestSeq.Load() != 123000 || p.totalLost.Load() != 3 || p.reports.Load() != 1 || p.burst.Load() != 0 {
		t.Fatalf("audio feedback reached video: highest=%d lost=%d reports=%d burst=%d", p.highestSeq.Load(), p.totalLost.Load(), p.reports.Load(), p.burst.Load())
	}
	p.handleVideoRTCP(video, []rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: video, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{7, 8})}})
	if p.burst.Load() != 2 {
		t.Fatalf("video NACK burst %d, want 2", p.burst.Load())
	}
}

// The same through a real negotiated viewer PC (the verifier's repro).
func TestViewerReceiverReportUsesOnlyVideoBlock(t *testing.T) {
	e, err := New("127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	p, err := e.Peer("review", false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	c.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	c.CreateDataChannel("heartbeat", nil)
	g := webrtc.GatheringCompletePromise(c)
	o, err := c.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetLocalDescription(o); err != nil {
		t.Fatal(err)
	}
	<-g
	if err = p.PC.SetRemoteDescription(*c.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	g = webrtc.GatheringCompletePromise(p.PC)
	a, err := p.PC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.PC.SetLocalDescription(a); err != nil {
		t.Fatal(err)
	}
	<-g
	if err = c.SetRemoteDescription(*p.PC.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.ConnectionState() != webrtc.PeerConnectionStateConnected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatal("not connected")
	}
	var vs, as uint32
	for _, s := range p.PC.GetSenders() {
		if s.Track().Kind() == webrtc.RTPCodecTypeVideo {
			vs = uint32(s.GetParameters().Encodings[0].SSRC)
		} else {
			as = uint32(s.GetParameters().Encodings[0].SSRC)
		}
	}
	if err = c.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 1234, Reports: []rtcp.ReceptionReport{{SSRC: vs, FractionLost: 1, TotalLost: 3, LastSequenceNumber: 123000}, {SSRC: as, FractionLost: 255, TotalLost: 17, LastSequenceNumber: 777}}}}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for p.reports.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a late audio block would land now
	if p.highestSeq.Load() != 123000 || p.totalLost.Load() != 3 || p.reports.Load() != 1 {
		t.Fatalf("video adaptation: loss=%d highest=%d reports=%d, want 3/123000/1", p.totalLost.Load(), p.highestSeq.Load(), p.reports.Load())
	}
}

// A NACK storm on one cached audio packet replays it at most twice, spaced,
// and never once it is older than audioMaxAge (the verifier saw 10,000).
func TestAudioReplayIsBounded(t *testing.T) {
	l, err := newAudioLeg()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	put := func(seq uint16) {
		l.cache[int(seq)%len(l.cache)] = cachedAudio{pkt: &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: make([]byte, 80)}, sent: now}
	}
	nack := func(seq uint16) {
		l.handleRTCP(42, []rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 42, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{seq})}})
	}
	put(5)
	for i := 0; i < 10000; i++ {
		nack(5)
	}
	if l.Replayed.Load() != 1 {
		t.Fatalf("burst of NACKs within the spacing replayed %d times, want 1", l.Replayed.Load())
	}
	now = now.Add(audioReplaySpacing)
	for i := 0; i < 10000; i++ {
		nack(5)
		now = now.Add(time.Millisecond)
	}
	if l.Replayed.Load() != audioReplayMax {
		t.Fatalf("replayed %d times, want at most %d", l.Replayed.Load(), audioReplayMax)
	}
	put(6)
	now = now.Add(audioMaxAge + time.Millisecond)
	nack(6)
	if l.Replayed.Load() != audioReplayMax {
		t.Fatal("replayed a packet older than audioMaxAge")
	}
	if l.Refused.Load() == 0 {
		t.Fatal("refusals not counted")
	}
}

// audioFlood writes a 500-byte audio packet every 1 ms until stop closes.
func audioFlood(p *Pacer, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				p.Write(&rtp.Header{SSRC: 2}, make([]byte, 500), nil)
			}
		}
	}()
	return done
}

// The verifier's repro: a saturated audio lane at a 64 kbit/s budget sent 6
// audio and 0 video packets in 300 ms. Video must get out.
func TestAudioFloodDoesNotStarveVideo(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.effective.Store(64_000)
	p.audio.ssrc.Store(2)
	var audio, video atomic.Uint64
	p.AddStream(2, interceptor.RTPWriterFunc(func(*rtp.Header, []byte, interceptor.Attributes) (int, error) { audio.Add(1); return 500, nil }))
	p.AddStream(1, interceptor.RTPWriterFunc(func(*rtp.Header, []byte, interceptor.Attributes) (int, error) { video.Add(1); return 500, nil }))
	stop := make(chan struct{})
	done := audioFlood(p, stop)
	time.Sleep(60 * time.Millisecond)
	p.Write(&rtp.Header{SSRC: 1, PayloadType: 98, Marker: true}, []byte{0x0c, 0}, nil)
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-done
	t.Logf("audio sent=%d video sent=%d", audio.Load(), video.Load())
	if video.Load() != 1 {
		t.Fatal("video starved by the audio lane")
	}
}

// Sustained: under an audio flood, video offered below its share (half
// the budget here) gets through whole; audio has only what video leaves.
func TestAudioFloodShareWithSustainedVideo(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	const budget = 800_000
	p.effective.Store(budget)
	p.audio.ssrc.Store(2)
	var audioBytes, videoBytes atomic.Uint64
	p.AddStream(2, interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) {
		audioBytes.Add(uint64(h.MarshalSize() + len(b) + wireOverhead))
		return len(b), nil
	}))
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) {
		videoBytes.Add(uint64(h.MarshalSize() + len(b) + wireOverhead))
		return len(b), nil
	}))
	stop := make(chan struct{})
	done := audioFlood(p, stop)
	// ~330 kbit/s of video: a 3-packet frame every 40 ms.
	var seq uint16
	var offered float64
	start := time.Now()
	for f := uint32(0); time.Since(start) < time.Second; f++ {
		for i := 0; i < 3; i++ {
			seq++
			h := &rtp.Header{SSRC: 1, PayloadType: 98, SequenceNumber: seq, Timestamp: f * 3600, Marker: i == 2}
			b := append([]byte{0x0c, 0}, make([]byte, 480)...)
			offered += float64(h.MarshalSize() + len(b) + wireOverhead)
			p.Write(h, b, nil)
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	<-done
	a, v := float64(audioBytes.Load()), float64(videoBytes.Load())
	t.Logf("audio %.0f kbit/s, video %.0f of %.0f kbit/s offered, budget %d kbit/s, drops %d", a*8/1000, v*8/1000, offered*8/1000, budget/1000, p.Drops.Load())
	if p.Drops.Load() != 0 || v < offered*0.99 {
		t.Fatalf("video lost frames under an audio flood: sent %.0f of %.0f bytes, drops %d", v, offered, p.Drops.Load())
	}
}

func TestAudioPriorityRate(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	if r := p.audioPriorityRate(2_000_000); r != audioPriorityFloor {
		t.Fatalf("unmeasured: %d", r)
	}
	p.SetAudioRate(100_000)
	if r := p.audioPriorityRate(2_000_000); r != 150_000 {
		t.Fatalf("measured 100k: %d, want 150k", r)
	}
	if r := p.audioPriorityRate(200_000); r != 100_000 {
		t.Fatalf("capped at half the budget: %d", r)
	}
}
