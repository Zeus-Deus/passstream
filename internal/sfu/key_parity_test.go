package sfu

import (
	"github.com/pion/rtp"
	"testing"
	"time"
)

func TestRefreshParityStartsImmediatelyWithoutAddingCopies(t *testing.T) {
	for _, severe := range []bool{false, true} {
		now := time.Now()
		p := &Pacer{}
		p.rtt.Store(int64(80 * time.Millisecond))
		p.keyframeBytes.Store(14_000)
		p.flightDemand.Store(200_000)
		p.effective.Store(1_100_000)
		loss, copies := uint32(50_000), 2
		if severe {
			loss, copies = 500_000, 4
		}
		p.mediaLossPPM.Store(loss)
		p.mediaLossAt.Store(now.UnixNano())
		w := parityRecordingWriter{make(chan *rtp.Packet, 8)}
		track := &repairTrack{writer: w, pacer: p, mediaPT: 98, redPT: 116, fecPT: 120, red: true}
		track.groupSize.Store(1)
		// VP9 B/E with P=0 marks a complete independent picture.
		start := time.Now()
		if err := track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: 9000, Marker: true}, Payload: []byte{0x0c, 0}}); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < copies+1; n++ {
			select {
			case q := <-w.packets:
				want := byte(120)
				if n == 0 {
					want = 98
				}
				if q.Payload[0] != want || q.SequenceNumber != uint16(n+1) {
					t.Fatal("refresh parity order changed", n, q)
				}
				if n == 2 && time.Since(start) < 60*time.Millisecond {
					t.Fatal("later refresh copy lost diversity")
				}
				if severe && n == 4 && time.Since(start) < 160*time.Millisecond {
					t.Fatal("last severe copy lost diversity")
				}
			case <-time.After(time.Second):
				t.Fatal("refresh copy missing", n)
			}
			if n == 1 && time.Since(start) > 40*time.Millisecond {
				t.Fatal("first refresh copy was deferred")
			}
		}
		track.mu.Lock()
		if int(track.FECSent.Load()) != copies || len(w.packets) != 0 {
			t.Fatal("copy count or next-picture state changed")
		}
		track.mu.Unlock()
	}
}
