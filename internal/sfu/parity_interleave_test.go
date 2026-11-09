package sfu

import (
	"github.com/pion/rtp"
	"testing"
	"time"
)

func TestDelayedParityCannotDiscardAPartialMediaPicture(t *testing.T) {
	p := &Pacer{}
	p.mediaPT.Store(98)
	p.redPT.Store(116)
	p.fecPT.Store(117)
	write := func(ts uint32, marker bool, payload []byte) {
		t.Helper()
		if _, err := p.Write(&rtp.Header{SSRC: 1, PayloadType: 116, Timestamp: ts, Marker: marker}, payload, nil); err != nil {
			t.Fatal(err)
		}
	}
	write(2, false, []byte{98, 0x48, 0})
	write(2, false, []byte{98, 0x40, 0})
	write(1, false, []byte{117, 0, 0}) // An older frame's delayed timer fires between media packets.
	if len(p.pending) != 2 || p.pending[0].h.Timestamp != 2 || p.Drops.Load() != 0 {
		t.Fatal("old parity discarded an unfinished live picture", len(p.pending), p.Drops.Load())
	}
	write(2, true, []byte{98, 0x44, 0})
	if len(p.pending) != 0 || len(p.queue) != 4 || p.queue[0].h.Timestamp != 1 || !p.queue[0].fec || p.Drops.Load() != 0 {
		t.Fatal("parity and complete media were not independently queued", len(p.pending), len(p.queue), p.Drops.Load())
	}
	for _, q := range p.queue[1:] {
		if q.fec || q.h.Timestamp != 2 {
			t.Fatal("picture mixed with parity")
		}
	}
}

func TestExcessParityCannotEvictQueuedOrPendingMedia(t *testing.T) {
	p := &Pacer{}
	p.mediaPT.Store(98)
	p.redPT.Store(116)
	p.fecPT.Store(117)
	p.queue = make([]pacedPacket, 2047)
	p.pending = []pacedPacket{{h: rtp.Header{Timestamp: 2}, p: []byte{98, 0x08, 0}, at: time.Now()}}
	p.pendingBytes = 15
	if _, err := p.Write(&rtp.Header{SSRC: 1, PayloadType: 116, Timestamp: 1}, []byte{117, 0, 0}, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.queue) != 2047 || len(p.pending) != 1 || p.Drops.Load() != 0 || p.Suppressed.Load() != 1 {
		t.Fatal("expendable parity evicted video or escaped the packet bound", len(p.queue), len(p.pending), p.Drops.Load(), p.Suppressed.Load())
	}
}
