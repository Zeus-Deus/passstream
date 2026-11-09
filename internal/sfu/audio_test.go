package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// recorder collects (ssrc, seq) in send order.
type recorder struct {
	mu   sync.Mutex
	sent [][2]uint32
}

func (r *recorder) writer() interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		r.mu.Lock()
		r.sent = append(r.sent, [2]uint32{h.SSRC, uint32(h.SequenceNumber)})
		r.mu.Unlock()
		return 1, nil
	})
}
func (r *recorder) list() [][2]uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]uint32(nil), r.sent...)
}

// Audio is served ahead of queued video, from the same budget.
func TestPacerSendsAudioAheadOfQueuedVideo(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.effective.Store(64_000) // ~8 KB/s: a 30 KB backlog takes seconds
	var rec recorder
	p.AddStream(1, rec.writer())
	p.AddStream(2, rec.writer())
	p.audio.ssrc.Store(2)
	for i := 0; i < 30; i++ {
		p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i + 1), Timestamp: uint32(i)}, make([]byte, 1000), nil)
	}
	time.Sleep(50 * time.Millisecond)
	before := len(rec.list())
	p.Write(&rtp.Header{SSRC: 2, SequenceNumber: 500, Timestamp: 7}, make([]byte, 80), nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l := rec.list()
		for i, x := range l {
			if x[0] == 2 {
				if i > before+1 {
					t.Fatalf("audio waited behind %d video packets", i-before)
				}
				if p.audio.Sent.Load() != 1 {
					t.Fatal("audio not counted")
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("audio never sent")
}

// An audio packet in the middle of a video picture must not be read as VP9
// nor end the pending picture (Pacer.Write groups video by timestamp).
func TestPacerAudioIsOpaqueToVideoAssembly(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mediaPT.Store(98)
	var rec recorder
	p.AddStream(1, rec.writer())
	p.AddStream(2, rec.writer())
	p.audio.ssrc.Store(2)
	vp9 := func(b, e bool) []byte {
		d := byte(0)
		if b {
			d |= 0x08
		}
		if e {
			d |= 0x04
		}
		return append([]byte{d}, make([]byte, 500)...)
	}
	p.Write(&rtp.Header{SSRC: 1, PayloadType: 98, SequenceNumber: 1, Timestamp: 9000}, vp9(true, false), nil)
	// An encrypted Opus payload that happens to look like a VP9 keyframe start.
	p.Write(&rtp.Header{SSRC: 2, PayloadType: 111, SequenceNumber: 1, Timestamp: 48000}, []byte{0x08, 0xff, 0x00, 0x12}, nil)
	p.Write(&rtp.Header{SSRC: 1, PayloadType: 98, SequenceNumber: 2, Timestamp: 9000, Marker: true}, vp9(false, true), nil)
	deadline := time.Now().Add(time.Second)
	for len(rec.list()) < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := rec.list(); len(got) != 3 || p.Drops.Load() != 0 || p.keyframeBytes.Load() == 0 {
		t.Fatalf("sent %v, drops %d, keyframe bytes %d", got, p.Drops.Load(), p.keyframeBytes.Load())
	}
	if p.keyframeBytes.Load() > 1100 {
		t.Fatalf("audio counted as keyframe: %d", p.keyframeBytes.Load())
	}
}

func TestPacerDropsStaleAudio(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.audio.ssrc.Store(2)
	p.mu.Lock()
	p.audio.queue = append(p.audio.queue, pacedPacket{h: rtp.Header{SSRC: 2}, at: time.Now().Add(-time.Second)})
	ready := p.audioReady(time.Now())
	p.mu.Unlock()
	if ready || p.audio.Late.Load() != 1 {
		t.Fatal("late audio kept")
	}
}

// A viewer that never drains its queue never blocks the publisher's reader.
func TestAudioLegQueueIsBoundedAndNonBlocking(t *testing.T) {
	leg, err := newAudioLeg()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10*audioQueueLen; i++ {
			leg.offer(audioPacket{pkt: &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i)}}, at: time.Now()})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("offer blocked")
	}
	if len(leg.queue) != audioQueueLen || leg.Dropped.Load() != 9*audioQueueLen {
		t.Fatalf("queue %d dropped %d", len(leg.queue), leg.Dropped.Load())
	}
	// The newest packets are kept.
	if x := <-leg.queue; x.pkt.SequenceNumber != uint16(9*audioQueueLen) {
		t.Fatalf("oldest kept: %d", x.pkt.SequenceNumber)
	}
}

func TestRoomAudioRate(t *testing.T) {
	var a roomAudio
	now := time.Now()
	for i := 0; i <= 50; i++ {
		a.count(110, now.Add(time.Duration(i)*20*time.Millisecond))
	}
	if r := a.Rate(); r < 20_000 || r > 80_000 {
		t.Fatalf("audio rate %d", r)
	}
}

// Video NACKs reach the audio sender too (compound SRTCP): they must not replay audio.
func TestAudioNackOnlyForAudioSSRC(t *testing.T) {
	leg, err := newAudioLeg()
	if err != nil {
		t.Fatal(err)
	}
	leg.cache[5] = cachedAudio{pkt: &rtp.Packet{Header: rtp.Header{SequenceNumber: 5}}, sent: time.Now()}
	leg.handleRTCP(42, []rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 7, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{5})}})
	if leg.Replayed.Load() != 0 {
		t.Fatal("a video NACK replayed audio")
	}
	// The unbound track drops the write; the attempt is what matters here.
	leg.handleRTCP(42, []rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 42, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{5})}})
	if leg.Replayed.Load() != 1 {
		t.Fatalf("audio NACK not served: %d", leg.Replayed.Load())
	}
}
