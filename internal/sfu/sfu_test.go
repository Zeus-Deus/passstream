package sfu

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"sync/atomic"
	"testing"
	"time"
)

func TestPacerBoundsAndIndependentWriter(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	p.AddStream(1, interceptor.RTPWriterFunc(func(_ *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
		return 0, nil
	}))
	payload := make([]byte, 1400)
	p.Write(&rtp.Header{SSRC: 1}, payload, nil)
	<-entered
	for i := 0; i < 10000; i++ {
		p.Write(&rtp.Header{SSRC: 1}, payload, nil)
	}
	p.mu.Lock()
	n, bytes := len(p.queue), p.bytes
	p.mu.Unlock()
	if n > 2048 || bytes > 2<<20 || p.Drops.Load() == 0 {
		t.Fatalf("unbounded: packets=%d bytes=%d drops=%d", n, bytes, p.Drops.Load())
	}
	other := NewPacer()
	defer other.Close()
	sent := make(chan bool, 1)
	other.AddStream(2, interceptor.RTPWriterFunc(func(_ *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) { sent <- true; return 1, nil }))
	other.Write(&rtp.Header{SSRC: 2}, []byte{1}, nil)
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("blocked viewer stalled another pacer")
	}
	close(blocked)
}
func TestKeyframeCoalescing(t *testing.T) {
	pc, e := webrtc.NewPeerConnection(webrtc.Configuration{})
	if e != nil {
		t.Fatal(e)
	}
	defer pc.Close()
	r := &Room{ssrc: 123, publisher: &Peer{PC: pc}}
	for i := 0; i < 100; i++ {
		r.PLI()
	}
	first := r.pliAt
	if first.IsZero() {
		t.Fatal("no request")
	}
	r.PLI()
	if r.pliAt != first {
		t.Fatal("PLI not coalesced")
	}
}

func TestPacerExpiresQueuedPackets(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	blocked := make(chan struct{})
	entered := make(chan struct{})
	sent := make(chan uint16, 4)
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		if h.SequenceNumber == 1 {
			close(entered)
			<-blocked
		}
		sent <- h.SequenceNumber
		return 1, nil
	}))
	p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 1}, []byte{1}, nil)
	<-entered
	p.Write(&rtp.Header{SSRC: 1, SequenceNumber: 2}, []byte{2}, nil)
	time.Sleep(160 * time.Millisecond)
	close(blocked)
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("writer did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for p.Drops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.Drops.Load() != 1 {
		t.Fatal("expired packet was not dropped")
	}
	select {
	case seq := <-sent:
		t.Fatalf("stale packet sent: %d", seq)
	default:
	}
}

func TestWireBudgetBoundsBurstAndCountsDebt(t *testing.T) {
	now := time.Now()
	b := wireBudget{last: now}
	b.accrue(now.Add(time.Hour), 256_000)
	if b.credit != 6000 {
		t.Fatalf("unbounded idle credit: %v", b.credit)
	}
	b.credit -= 6232
	b.accrue(now.Add(time.Hour+time.Millisecond), 256_000)
	if b.credit != -200 {
		t.Fatalf("packet debt lost: %v", b.credit)
	}
}
func TestPacerNeverDrainsBeyondWireBudget(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.SetTargetBitrate(64_000)
	var wire atomic.Int64
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) {
		wire.Add(int64(h.MarshalSize() + len(b) + wireOverhead))
		return len(b), nil
	}))
	from := time.Now()
	for i := 0; i < 100; i++ {
		p.Write(&rtp.Header{SSRC: 1}, make([]byte, 1000), nil)
	}
	time.Sleep(200 * time.Millisecond)
	if got := wire.Load(); float64(got) > 64_000*time.Since(from).Seconds()/8+float64(wireBurstBytes(64_000))+1500 {
		t.Fatalf("exceeded wire budget: %d", got)
	}
	if p.Drops.Load() == 0 {
		t.Fatal("expired backlog retained")
	}
}

func TestPacerDropsWholeFrameAndProtectsStartedFrame(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.SetTargetBitrate(512_000)
	p.mediaPT.Store(98)
	sent := make(chan uint16, 20)
	p.AddStream(7, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		sent <- h.SequenceNumber
		if h.SequenceNumber == 3 {
			time.Sleep(150 * time.Millisecond) // a blocked transport
		}
		return 1, nil
	}))
	// A started picture finishes as a unit at the wire rate.
	for i := 0; i < 3; i++ {
		payload := make([]byte, 1000)
		payload[0] = 0x40
		if i == 0 {
			payload[0] |= 0x08
		}
		p.Write(&rtp.Header{SSRC: 7, PayloadType: 98, Timestamp: 1, SequenceNumber: uint16(i + 1), Marker: i == 2}, payload, nil)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-sent:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("partial frame or pacer throttled below its drain target")
		}
	}
	// A frame queued behind a blocked write expires as a unit.
	for i := 0; i < 3; i++ {
		payload := make([]byte, 1000)
		payload[0] = 0x40
		if i == 0 {
			payload[0] |= 0x08
		}
		p.Write(&rtp.Header{SSRC: 7, PayloadType: 98, Timestamp: 2, SequenceNumber: uint16(i + 4), Marker: i == 2}, payload, nil)
	}
	time.Sleep(250 * time.Millisecond)
	if p.Drops.Load() != 3 || len(sent) != 0 {
		t.Fatalf("partial discard: %d drops, %d sent", p.Drops.Load(), len(sent))
	}
}

func TestPacerTopLayerDropDoesNotInvalidateBase(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	var drops atomic.Int32
	p.OnDrop = func() { drops.Add(1) }
	p.mu.Lock()
	p.queue = []pacedPacket{{h: rtp.Header{Timestamp: 1}, tid: 2, end: true}}
	p.bytes = 12
	p.discardFrame()
	waiting := p.waitKey
	p.mu.Unlock()
	if waiting || drops.Load() != 0 {
		t.Fatal("disposable frame invalidated dependencies")
	}
}

func TestPacerDropsDoNotLeaveWireSequenceGaps(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	got := make(chan uint16, 4)
	p.AddStream(3, interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		got <- h.SequenceNumber
		return 1, nil
	}))
	// The forwarder may omit arbitrary input numbers; the wire stays contiguous.
	for _, seq := range []uint16{20, 25, 200} {
		p.Write(&rtp.Header{SSRC: 3, SequenceNumber: seq, Timestamp: uint32(seq), Marker: true}, []byte{0x08, 0}, nil)
	}
	for want := uint16(1); want <= 3; want++ {
		select {
		case n := <-got:
			if n != want {
				t.Fatalf("wire %d want %d", n, want)
			}
		case <-time.After(time.Second):
			t.Fatal("packet not sent")
		}
	}
}

func TestPacerNeverAdmitsOrphanedFrameFragments(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mediaPT.Store(98)
	// Lost/evicted beginnings cannot be followed by transmitting the tail as a frame.
	_, err := p.Write(&rtp.Header{SSRC: 1, PayloadType: 98, Timestamp: 5, Marker: true}, []byte{0x44, 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	n := len(p.queue) + len(p.pending)
	p.mu.Unlock()
	if n != 0 || p.Drops.Load() != 1 {
		t.Fatal("orphan fragment queued")
	}
}
