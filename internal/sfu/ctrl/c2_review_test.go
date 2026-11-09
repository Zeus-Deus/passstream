package ctrl

import (
	"testing"
	"time"
)

func TestC2ReviewHistoryBounds(t *testing.T) {
	start := time.Unix(100, 0)
	p := &Discovery{}
	for i := 0; i < 10000; i++ {
		now := start.Add(time.Duration(i) * time.Second / 10)
		p.OnSent(Sent{Seq: uint64(i), At: now, Size: 1200, Kind: KindVideo})
		// Stable full native: Viewer.Step does not call Probe.Maybe for Target=0.
	}
	t.Logf("probe history after 1000s: %d entries, oldest age %s", len(p.charges), p.charges[len(p.charges)-1].at.Sub(p.charges[0].at))
	if len(p.charges) > 301 {
		t.Error("padding-window history retains the entire session")
	}
}

func TestC2ReviewUntrustedReceiverClock(t *testing.T) {
	start := time.Unix(100, 0)
	b := &Bandwidth{}
	for i := 0; i < 20000; i++ {
		now := start.Add(time.Duration(i) * time.Millisecond)
		b.OnSent(Sent{Seq: uint64(i), At: now, Size: 1200, Kind: KindVideo})
		b.OnFeedback(now, Feedback{Arrivals: []Arrival{{Seq: uint64(i), At: start}}})
	}
	t.Logf("acked history: %d entries after %d ring slots", len(b.ack), len(b.sent))
	if len(b.ack) > len(b.sent) {
		t.Error("receiver clock can grow ack history without bound")
	}
}

func TestC2ReviewBlockedWriterDropBounds(t *testing.T) {
	start := time.Unix(100, 0)
	p := &Queue{}
	for i := 0; i < 10000; i++ {
		p.Push(start.Add(time.Duration(i)*time.Second), Packet{ID: uint64(i), Size: 1000, FrameSize: 1000, Packets: 1, Kind: KindVideo, Class: ClassKey, Key: true, Frame: uint64(i)})
		// Writer blocked outside lock: no Pop/Step/Dropped.
	}
	b, f := p.Queued()
	t.Logf("reported queue=%d bytes / %d frames; deferred drops=%d; events=%d", b, f, len(p.drops), len(p.events))
	if len(p.drops) > QueueFrames || len(p.events) > QueueFrames {
		t.Error("deferred bookkeeping evades queue bounds")
	}
}

func TestC2ReviewProbeCannotProveMoreThanItsOffer(t *testing.T) {
	start := time.Unix(100, 0)
	p := &Discovery{}
	c, ok := p.Maybe(start, 140000, 100000, Estimate{Rate: 128000, Proven: 120000, Ceiling: 40000}, false)
	if !ok {
		t.Fatal("no probe")
	}
	for i := 0; i < 5; i++ {
		p.OnSent(Sent{Seq: uint64(i), At: start.Add(time.Duration(i) * 20 * time.Millisecond), Size: 300, Kind: KindPadding, Cluster: c.ID})
	}
	fb := Feedback{}
	for i := 0; i < 5; i++ {
		fb.Arrivals = append(fb.Arrivals, Arrival{Seq: uint64(i), At: start.Add(time.Second + time.Duration(i)*time.Millisecond)})
	}
	r, ok := p.OnFeedback(start.Add(500*time.Millisecond), fb)
	if !ok {
		t.Fatal("no result")
	}
	t.Logf("planned offer=%d measured delivered=%d success=%v", c.Rate, r.Rate, r.Success)
	if r.Rate > c.Rate || r.Success {
		t.Fatal("receive compression invents sustainable delivery evidence")
	}
}

func TestC2ReviewSuppressedSpamStillBlocksNativeUpgrade(t *testing.T) {
	now := time.Unix(100, 0)
	k := &RoomKeys{}
	k.Request(now, 1, Native, CauseRecovery)
	k.OnDelivery(1, KeyEvent{Frame: 1, Rendition: Low, Phase: KeyAdmitted, At: now})
	k.OnDelivery(1, KeyEvent{Frame: 1, Rendition: Low, Phase: KeyCompleted, At: now.Add(500 * time.Millisecond)})
	at := now.Add(2100 * time.Millisecond)
	status := k.Request(at, 1, Native, CauseRecovery)
	allowed := k.Allowed(at, 1, Native, CauseUpgrade)
	t.Logf("repeated recovery status=%v, native upgrade allowed=%v", status, allowed)
	if status != KeyIgnored || allowed {
		t.Fatal("key-delivery suppression erases current decoder-spam evidence")
	}
}
