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
func TestReplayExpiresAndRateLimits(t *testing.T) {
	var c packetCache
	now := time.Now()
	c.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 1}}, now)
	if c.get(1, now) == nil || c.get(1, now) != nil {
		t.Fatal("retry interval")
	}
	if c.get(1, now.Add(replayAge+time.Millisecond)) != nil {
		t.Fatal("stale replay")
	}
}
func TestReplayStopsAtKeyframe(t *testing.T) {
	var c packetCache
	now := time.Now()
	for _, seq := range []uint16{65534, 65535, 0, 1} {
		c.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}, now)
	}
	c.keyframe(0)
	if c.get(65534, now) != nil || c.get(65535, now) != nil {
		t.Fatal("replayed a packet the keyframe superseded")
	}
	if c.get(0, now) == nil || c.get(1, now) == nil {
		t.Fatal("keyframe or later packet not replayed")
	}
}
func TestDeadViewerGating(t *testing.T) {
	var v viewerLiveness
	now := time.Now()
	rtt := 50 * time.Millisecond
	if v.dead(now, 0, rtt) {
		t.Fatal("idle marked dead")
	}
	v.sent(now)
	if v.dead(now.Add(999*time.Millisecond), 0, rtt) || !v.dead(now.Add(1001*time.Millisecond), 0, rtt) {
		t.Fatal("deadline")
	}
	if v.dead(now.Add(2*time.Second), now.Add(time.Second).UnixNano(), rtt) {
		t.Fatal("RTCP did not revive")
	}
	var slow viewerLiveness
	slow.sent(now)
	if slow.dead(now.Add(1900*time.Millisecond), 0, 0) || !slow.dead(now.Add(2100*time.Millisecond), 0, 0) {
		t.Fatal("unknown-RTT deadline")
	}
	if slow.dead(now.Add(3100*time.Millisecond), 0, 800*time.Millisecond) || !slow.dead(now.Add(3300*time.Millisecond), 0, 800*time.Millisecond) {
		t.Fatal("RTT-scaled deadline")
	}
}
func TestPublisherContinuity(t *testing.T) {
	var c continuity
	pkt := func(ssrc, ts uint32, pid byte) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SSRC: ssrc, Timestamp: ts}, Payload: []byte{0x88, 0x80, pid, 1}}
	}
	a := pkt(1, 90000, 120)
	c.start(frame{packets: []*rtp.Packet{a}})
	c.packet(a)
	b := pkt(2, 1234, 3)
	c.start(frame{packets: []*rtp.Packet{b}})
	c.packet(b)
	if b.Timestamp != 96000 || b.Payload[2] != 121 {
		t.Fatalf("discontinuity %+v %v", b.Header, b.Payload)
	}
}

func TestReplayRetryCap(t *testing.T) {
	var c packetCache
	now := time.Now()
	c.put(&rtp.Packet{Header: rtp.Header{SequenceNumber: 8}}, now)
	for i := 0; i < replayTries; i++ {
		if c.get(8, now.Add(time.Duration(i)*40*time.Millisecond)) == nil {
			t.Fatal("early retry rejected")
		}
	}
	if c.get(8, now.Add(time.Duration(replayTries)*40*time.Millisecond)) != nil {
		t.Fatal("unbounded replay")
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
	old.Room.ssrc = 123
	old.Room.mu.Unlock()
	next, err := e.Peer("test", true)
	if err != nil {
		t.Fatal(err)
	}
	next.Room.mu.Lock()
	ssrc := next.Room.ssrc
	next.Room.mu.Unlock()
	if ssrc != 0 {
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
	c.start(frame{source: 1, packets: []*rtp.Packet{a}})
	c.packet(a)
	b := &rtp.Packet{Header: rtp.Header{SSRC: 99, Timestamp: 500}, Payload: []byte{0x88, 0x80, 2, 1}}
	c.start(frame{source: 2, packets: []*rtp.Packet{b}})
	c.packet(b)
	if b.Timestamp != 96000 || b.Payload[2] != 121 {
		t.Fatalf("publisher epoch discontinuity: %d %v", b.Timestamp, b.Payload)
	}
}

func TestUnansweredPLIWaitsForKeyframe(t *testing.T) {
	e, err := New("127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	p, err := e.Peer("pli", true)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Room
	r.mu.Lock()
	r.ssrc = 1
	r.mu.Unlock()
	r.PLI()
	if r.PLIsSent.Load() != 1 {
		t.Fatal("first PLI not sent")
	}
	time.Sleep(350 * time.Millisecond)
	r.PLI()
	if r.PLIsSent.Load() != 1 {
		t.Fatal("unanswered PLI repeated at the answered spacing")
	}
	r.keyStart.Store(time.Now().UnixNano())
	r.mu.Lock()
	r.pliAt = time.Now().Add(-1100 * time.Millisecond)
	r.mu.Unlock()
	r.PLI()
	if r.PLIsSent.Load() != 2 {
		t.Fatal("answered PLI not repeatable after the repair window")
	}
}

func TestPLIDoesNotRestartKeyframeInFlight(t *testing.T) {
	e, err := New("127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	p, err := e.Peer("inflight", true)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Room
	r.mu.Lock()
	r.ssrc = 1
	r.mu.Unlock()
	now := time.Now().UnixNano()
	r.keyStart.Store(now)
	r.keyLast.Store(now)
	r.PLI()
	if r.PLIsSent.Load() != 0 {
		t.Fatal("requested a keyframe while one was arriving")
	}
	r.keyDone.Store(time.Now().UnixNano())
	r.PLI()
	if r.PLIsSent.Load() != 1 {
		t.Fatal("finished keyframe still blocks requests")
	}
	// A stalled transfer no longer blocks.
	for i := 0; i < 100; i++ {
		r.mu.Lock()
		busy := r.pliBusy
		r.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	r.pliAt = time.Time{}
	r.mu.Unlock()
	r.keyStart.Store(time.Now().UnixNano())
	r.keyLast.Store(time.Now().Add(-1100 * time.Millisecond).UnixNano())
	r.PLI()
	if r.PLIsSent.Load() != 2 {
		t.Fatal("stalled keyframe blocked requests")
	}
}
