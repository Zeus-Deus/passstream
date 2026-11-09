package sfu

import (
	"github.com/pion/rtp"
	"testing"
	"time"
)

func TestNackDeadlineRetryAndWindow(t *testing.T) {
	now := time.Now()
	r := newReorder()
	for _, seq := range []uint16{65534, 1} {
		r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}, now)
	}
	if len(r.nacks(now.Add(nackDelay-time.Millisecond))) != 0 {
		t.Fatal("requested before reordering could resolve")
	}
	for i := 0; i < nackTries; i++ {
		at := now.Add(nackDelay + time.Duration(i)*nackSpacing)
		if n := len(r.nacks(at)); n != 2 {
			t.Fatalf("try %d got %d", i, n)
		}
		if len(r.nacks(at.Add(nackSpacing-time.Millisecond))) != 0 {
			t.Fatal("retry spacing")
		}
	}
	if len(r.nacks(now.Add(nackDelay+nackTries*nackSpacing))) != 0 {
		t.Fatal("retry cap")
	}
	r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 3}}, now)
	if len(r.nacks(now.Add(reorderDeadline+time.Millisecond))) != 0 {
		t.Fatal("expired gap requested")
	}
	r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 10003}}, now)
	if n := len(r.nacks(now.Add(31 * time.Millisecond))); n > 255 {
		t.Fatal("unbounded outage", n)
	}
}
func TestPublisherContinuity(t *testing.T) {
	var c continuity
	pkt := func(ssrc, ts uint32, pid byte) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SSRC: ssrc, Timestamp: ts}, Payload: []byte{0x88, 0x80, pid, 1}}
	}
	a := pkt(1, 90000, 120)
	c.start([]*rtp.Packet{a}, time.Time{}, 0)
	c.packet(a)
	b := pkt(2, 1234, 3)
	c.start([]*rtp.Packet{b}, time.Time{}, 0)
	c.packet(b)
	if b.Timestamp != 96000 || b.Payload[2] != 121 {
		t.Fatalf("discontinuity %+v %v", b.Header, b.Payload)
	}
}

func TestPublisherFenceClearsSSRC(t *testing.T) {
	e, err := New("127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	old, err := e.Peer("test", true)
	if err != nil {
		t.Fatal(err)
	}
	old.Room.mu.Lock()
	old.Room.ssrc[0] = 123
	old.Room.mu.Unlock()
	next, err := e.Peer("test", true)
	if err != nil {
		t.Fatal(err)
	}
	next.Room.mu.Lock()
	ssrc := next.Room.ssrc
	next.Room.mu.Unlock()
	if ssrc != [2]uint32{} {
		t.Fatalf("fenced SSRC retained: %d", ssrc)
	}
}

func TestNackDoesNotResurrectOutageGaps(t *testing.T) {
	r := newReorder()
	now := time.Now()
	r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 1}}, now)
	r.pop(now)
	r.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 250}}, now.Add(2*time.Second))
	if len(r.nacks(now.Add(2030*time.Millisecond))) != 0 {
		t.Fatal("outage history NACKed")
	}
	if len(r.pop(now.Add(2030*time.Millisecond))) != 1 {
		t.Fatal("stale outage delayed new packet")
	}
}

func TestContinuityWhenPublisherReusesSSRC(t *testing.T) {
	var c continuity
	a := &rtp.Packet{Header: rtp.Header{SSRC: 99, Timestamp: 90000}, Payload: []byte{0x88, 0x80, 120, 1}}
	c.start([]*rtp.Packet{a}, time.Time{}, 1)
	c.packet(a)
	b := &rtp.Packet{Header: rtp.Header{SSRC: 99, Timestamp: 500}, Payload: []byte{0x88, 0x80, 2, 1}}
	c.start([]*rtp.Packet{b}, time.Time{}, 2)
	c.packet(b)
	if b.Timestamp != 96000 || b.Payload[2] != 121 {
		t.Fatalf("publisher epoch discontinuity: %d %v", b.Timestamp, b.Payload)
	}
}
