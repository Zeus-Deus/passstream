package sfu

import (
	"github.com/pion/rtp"
	"testing"
	"time"
)

func TestCompoundedLossParityRequiresItsCompleteWireReservation(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.rtt.Store(int64(80 * time.Millisecond))
	p.keyframeBytes.Store(9000)
	p.flightDemand.Store(200_000)
	p.effective.Store(1_060_000)
	p.mediaLossPPM.Store(250_000)
	p.mediaLossAt.Store(now.UnixNano())
	if p.severeParity(now) {
		t.Fatal("ordinary loss gained compounded-loss copies")
	}
	p.mediaLossPPM.Store(500_000)
	if !p.severeParity(now) {
		t.Fatal("half-loss path lost its reserved recovery copies")
	}
	p.mediaLossPPM.Store(0)
	p.mediaLossAt.Store(now.Add(50 * time.Millisecond).UnixNano())
	if !p.severeParity(now.Add(180 * time.Millisecond)) {
		t.Fatal("clean feedback canceled scheduled severe repair")
	}
	p.effective.Store(1_059_999)
	if p.severeParity(now) {
		t.Fatal("five-copy repair exceeded its wire reservation")
	}
	p.effective.Store(1_060_000)
	if p.severeParity(now.Add(2 * time.Second)) {
		t.Fatal("stale severe loss enabled copies indefinitely")
	}
	p.rtt.Store(int64(20 * time.Millisecond))
	if p.severeParity(now) {
		t.Fatal("nearby AQM gained severe-loss copies")
	}
}

func TestCompoundedLossCopiesKeepMediaFirstAndStayBounded(t *testing.T) {
	p := &Pacer{}
	p.rtt.Store(int64(80 * time.Millisecond))
	p.keyframeBytes.Store(9000)
	p.flightDemand.Store(200_000)
	p.effective.Store(1_100_000)
	p.mediaLossPPM.Store(500_000)
	p.mediaLossAt.Store(time.Now().UnixNano())
	w := parityRecordingWriter{make(chan *rtp.Packet, 6)}
	track := &repairTrack{writer: w, pacer: p, mediaPT: 98, redPT: 116, fecPT: 120, red: true}
	track.groupSize.Store(1)
	start := time.Now()
	if err := track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: 9000, Marker: true}, Payload: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 5; n++ {
		select {
		case q := <-w.packets:
			if q.SequenceNumber != uint16(n+1) || (n == 0 && q.Payload[0] != 98) || (n > 0 && q.Payload[0] != 120) {
				t.Fatal("media/parity order changed", n, q)
			}
			if n == 4 && time.Since(start) < 160*time.Millisecond {
				t.Fatal("last severe copy lost temporal diversity")
			}
		case <-time.After(time.Second):
			t.Fatal("severe repair remained pending")
		}
	}
	track.mu.Lock()
	defer track.mu.Unlock()
	if track.FECSent.Load() != 4 || len(w.packets) != 0 {
		t.Fatal("severe repair exceeded four bounded copies")
	}
}
