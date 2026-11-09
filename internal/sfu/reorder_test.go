package sfu

import (
	"github.com/pion/rtp"
	"testing"
	"time"
)

func TestReorderRepairAndWrap(t *testing.T) {
	r := newReorder()
	now := time.Now()
	pkt := func(n uint16) *rtp.Packet { return &rtp.Packet{Header: rtp.Header{SequenceNumber: n}} }
	r.push(pkt(65534), now)
	if len(r.pop(now)) != 1 {
		t.Fatal("contiguous packet delayed")
	}
	r.push(pkt(0), now)
	if len(r.pop(now)) != 0 {
		t.Fatal("did not hold for repair")
	}
	r.push(pkt(65535), now)
	v := r.pop(now)
	if len(v) != 2 || v[0].SequenceNumber != 65535 || v[1].SequenceNumber != 0 {
		t.Fatal("repair order or wrap")
	}
	r.push(pkt(2), now)
	if len(r.pop(now.Add(reorderDeadline-time.Millisecond))) != 0 {
		t.Fatal("gap expired early")
	}
	if len(r.pop(now.Add(reorderDeadline+time.Millisecond))) != 1 {
		t.Fatal("gap did not expire")
	}
	r.push(pkt(1), now)
	if len(r.pop(now)) != 0 {
		t.Fatal("late packet resurrected")
	}
}

func TestFinalUpstreamRetryKeepsWiFiRoundTripHeadroom(t *testing.T) {
	now := time.Now()
	r := newReorder()
	packet := func(seq uint16) *rtp.Packet { return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq}} }
	r.push(packet(10), now)
	r.pop(now)
	r.push(packet(12), now)
	var last time.Time
	for i := 0; i < nackTries; i++ {
		last = now.Add(nackDelay + time.Duration(i)*nackSpacing)
		if len(r.nacks(last)) != 1 {
			t.Fatal("lost an upstream repair opportunity", i)
		}
	}
	// Lose every earlier response; the last one needs 80ms RTT plus jitter.
	reply := last.Add(90 * time.Millisecond)
	if reply.After(now.Add(reorderDeadline)) {
		r.pop(now.Add(reorderDeadline + time.Millisecond))
	}
	r.push(packet(11), reply)
	if got := r.pop(reply); len(got) != 2 || got[0].SequenceNumber != 11 || got[1].SequenceNumber != 12 {
		t.Fatal("final Wi-Fi repair arrived after its own deadline", reply.Sub(now), got)
	}
}
